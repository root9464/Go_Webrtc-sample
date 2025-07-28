package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"text/template"
	"time"

	"github.com/gofiber/contrib/socketio"
	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	addr          = flag.String("addr", ":8080", "http service address")
	indexTemplate = &template.Template{}
	roomsLock     sync.RWMutex
	rooms         = make(map[string]*room)
	logger        *zap.Logger
	bufferPool    = sync.Pool{
		New: func() interface{} {
			return make([]byte, 1500)
		},
	}
	serverRunning uint32 = 1
)

type websocketMessage struct {
	Event string `json:"event"`
	Data  string `json:"data"`
}

type connection struct {
	pc             *webrtc.PeerConnection
	kws            *socketio.Websocket
	lock           sync.Mutex
	tracks         map[string]*webrtc.TrackLocalStaticRTP
	roomID         string
	closed         chan struct{}
	lastSignal     time.Time
	signalDebounce time.Duration
}

type room struct {
	lock        sync.RWMutex
	connections []*connection
	trackLocals map[string]*webrtc.TrackLocalStaticRTP
	trackCount  int64
}

func initLogger() {
	config := zap.NewProductionConfig()
	config.EncoderConfig.TimeKey = "timestamp"
	config.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	config.EncoderConfig.MessageKey = "message"
	var err error
	logger, err = config.Build()
	if err != nil {
		panic(err)
	}
}

func writeJSON(kws *socketio.Websocket, lock *sync.Mutex, v interface{}, requestID string) error {
	lock.Lock()
	defer lock.Unlock()
	data, err := json.Marshal(v)
	if err != nil {
		logger.Error("Failed to marshal JSON",
			zap.Error(err),
			zap.String("request_id", requestID))
		return err
	}
	kws.Emit(data, socketio.TextMessage)
	return nil
}

func generateRequestID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func main() {
	flag.Parse()
	initLogger()
	defer logger.Sync()

	app := fiber.New()

	app.Use(func(c *fiber.Ctx) error {
		if websocket.IsWebSocketUpgrade(c) {
			c.Locals("allowed", true)
			return c.Next()
		}
		return fiber.ErrUpgradeRequired
	})

	app.Get("/", func(c *fiber.Ctx) error {
		return indexTemplate.Execute(c.Response().BodyWriter(), "ws://"+c.Hostname()+"/websocket")
	})

	socketio.On(socketio.EventConnect, func(ep *socketio.EventPayload) {
		logger.Info("New connection",
			zap.String("uuid", ep.Kws.GetUUID()),
			zap.String("request_id", generateRequestID()))
	})

	socketio.On(socketio.EventMessage, websocketHandler)

	socketio.On(socketio.EventDisconnect, func(ep *socketio.EventPayload) {
		requestID := generateRequestID()
		roomID := ep.Kws.GetStringAttribute("room_id")
		if roomID == "" {
			logger.Info("Disconnected without room",
				zap.String("uuid", ep.Kws.GetUUID()),
				zap.String("request_id", requestID))
			return
		}

		roomsLock.RLock()
		r, exists := rooms[roomID]
		roomsLock.RUnlock()

		if !exists {
			logger.Info("Disconnected from non-existing room",
				zap.String("room_id", roomID),
				zap.String("request_id", requestID))
			return
		}

		r.lock.Lock()
		for i, conn := range r.connections {
			if conn.kws.GetUUID() == ep.Kws.GetUUID() {
				for trackID := range conn.tracks {
					if _, ok := r.trackLocals[trackID]; ok {
						delete(r.trackLocals, trackID)
						atomic.AddInt64(&r.trackCount, -1)
					}
				}
				close(conn.closed)
				if err := conn.pc.Close(); err != nil {
					logger.Error("Failed to close PeerConnection",
						zap.Error(err),
						zap.String("uuid", conn.kws.GetUUID()),
						zap.String("request_id", requestID))
				}
				r.connections = append(r.connections[:i], r.connections[i+1:]...)
				break
			}
		}

		if len(r.connections) == 0 {
			roomsLock.Lock()
			delete(rooms, roomID)
			roomsLock.Unlock()
			logger.Info("Room deleted",
				zap.String("room_id", roomID),
				zap.String("request_id", requestID))
		} else {
			r.lock.Unlock()
			r.signalPeerConnections(requestID)
			r.lock.Lock()
		}

		r.lock.Unlock()
		logger.Info("Disconnected",
			zap.String("uuid", ep.Kws.GetUUID()),
			zap.String("room_id", roomID),
			zap.String("request_id", requestID),
			zap.Int64("remaining_tracks", r.trackCount))
	})

	app.Get("/websocket", socketio.New(func(kws *socketio.Websocket) {
		if atomic.LoadUint32(&serverRunning) == 0 {
			logger.Warn("Connection attempt while server is shutting down",
				zap.String("uuid", kws.GetUUID()))
			return
		}

		requestID := generateRequestID()
		roomID := kws.Query("room_id")
		if roomID == "" {
			logger.Error("Room ID not provided",
				zap.String("uuid", kws.GetUUID()),
				zap.String("request_id", requestID))
			return
		}

		kws.SetAttribute("room_id", roomID)

		mediaEngine := &webrtc.MediaEngine{}
		if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
			logger.Error("Failed to register codecs",
				zap.Error(err),
				zap.String("request_id", requestID))
			return
		}

		settingEngine := webrtc.SettingEngine{}
		api := webrtc.NewAPI(
			webrtc.WithMediaEngine(mediaEngine),
			webrtc.WithSettingEngine(settingEngine),
		)

		pc, err := api.NewPeerConnection(webrtc.Configuration{
			SDPSemantics: webrtc.SDPSemanticsUnifiedPlanWithFallback,
		})
		if err != nil {
			logger.Error("Failed to create PeerConnection",
				zap.Error(err),
				zap.String("request_id", requestID))
			return
		}

		for _, typ := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
			if _, err := pc.AddTransceiverFromKind(typ, webrtc.RTPTransceiverInit{
				Direction: webrtc.RTPTransceiverDirectionRecvonly,
			}); err != nil {
				logger.Error("Failed to add transceiver",
					zap.Error(err),
					zap.String("request_id", requestID))
				return
			}
		}

		conn := &connection{
			pc:             pc,
			kws:            kws,
			tracks:         make(map[string]*webrtc.TrackLocalStaticRTP),
			roomID:         roomID,
			closed:         make(chan struct{}),
			signalDebounce: time.Millisecond * 500,
		}

		roomsLock.Lock()
		r, exists := rooms[roomID]
		if !exists {
			r = &room{
				connections: make([]*connection, 0),
				trackLocals: make(map[string]*webrtc.TrackLocalStaticRTP),
				trackCount:  0,
			}
			rooms[roomID] = r
			logger.Info("New room created",
				zap.String("room_id", roomID),
				zap.String("request_id", requestID))
		}
		roomsLock.Unlock()

		r.lock.Lock()
		r.connections = append(r.connections, conn)
		r.lock.Unlock()

		setupWebRTCHandlers(conn, r, requestID)

		r.signalPeerConnections(requestID)
	}))

	go func() {
		ticker := time.NewTicker(time.Second * 5)
		defer ticker.Stop()
		for range ticker.C {
			if atomic.LoadUint32(&serverRunning) == 0 {
				break
			}
			roomsLock.RLock()
			for roomID, r := range rooms {
				r.dispatchKeyFrame()
				logger.Info("Dispatching key frame",
					zap.String("room_id", roomID),
					zap.Int64("track_count", r.trackCount))
			}
			roomsLock.RUnlock()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-ctx.Done()
		atomic.StoreUint32(&serverRunning, 0)
		logger.Info("Initiating graceful shutdown")
		roomsLock.Lock()
		for roomID, r := range rooms {
			r.lock.Lock()
			for _, conn := range r.connections {
				close(conn.closed)
				if err := conn.pc.Close(); err != nil {
					logger.Error("Failed to close PeerConnection during shutdown",
						zap.Error(err),
						zap.String("uuid", conn.kws.GetUUID()))
				}
			}
			r.connections = nil
			r.trackLocals = nil
			r.lock.Unlock()
			delete(rooms, roomID)
		}
		roomsLock.Unlock()
		logger.Info("All connections closed, server shutdown complete")
	}()

	if err := app.Listen(*addr); err != nil {
		logger.Error("Failed to start server", zap.Error(err))
	}
	cancel()
}

func setupWebRTCHandlers(conn *connection, r *room, requestID string) {
	pc := conn.pc
	kws := conn.kws

	pc.OnICECandidate(func(i *webrtc.ICECandidate) {
		if i == nil {
			return
		}
		candidateString, err := json.Marshal(i.ToJSON())
		if err != nil {
			logger.Error("Failed to marshal candidate",
				zap.Error(err),
				zap.String("request_id", requestID))
			return
		}
		if err := writeJSON(kws, &conn.lock, &websocketMessage{
			Event: "candidate",
			Data:  string(candidateString),
		}, requestID); err != nil {
			logger.Error("Failed to send candidate",
				zap.Error(err),
				zap.String("request_id", requestID))
		}
	})

	pc.OnConnectionStateChange(func(p webrtc.PeerConnectionState) {
		logger.Info("PeerConnection state changed",
			zap.String("state", p.String()),
			zap.String("uuid", kws.GetUUID()),
			zap.String("request_id", requestID))
		switch p {
		case webrtc.PeerConnectionStateFailed:
			if err := pc.Close(); err != nil {
				logger.Error("Failed to close PeerConnection",
					zap.Error(err),
					zap.String("request_id", requestID))
			}
		case webrtc.PeerConnectionStateClosed:
			r.signalPeerConnections(requestID)
		}
	})

	pc.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if conn.pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
			logger.Warn("Track received but connection not stable",
				zap.String("track_id", t.ID()),
				zap.String("uuid", kws.GetUUID()),
				zap.String("request_id", requestID),
				zap.Uint32("ssrc", uint32(t.SSRC())))
			return
		}

		logger.Info("New track received",
			zap.String("track_id", t.ID()),
			zap.String("uuid", kws.GetUUID()),
			zap.String("request_id", requestID),
			zap.String("kind", t.Kind().String()),
			zap.Uint32("ssrc", uint32(t.SSRC())))

		trackLocal := r.addTrack(conn, t)
		if trackLocal == nil {
			return
		}

		go func() {
			defer func() {
				r.removeTrack(trackLocal)
				logger.Info("Track processing stopped",
					zap.String("track_id", t.ID()),
					zap.String("uuid", kws.GetUUID()),
					zap.String("request_id", requestID))
			}()

			rtpPkt := &rtp.Packet{}
			for {
				select {
				case <-conn.closed:
					logger.Info("Track processing stopped due to connection closed",
						zap.String("track_id", t.ID()),
						zap.String("request_id", requestID))
					return
				default:
					buf := bufferPool.Get().([]byte)
					i, _, err := t.Read(buf)
					if err != nil {
						bufferPool.Put(buf)
						if err == io.EOF {
							logger.Info("Track closed",
								zap.String("track_id", t.ID()),
								zap.String("request_id", requestID))
							return
						}
						logger.Error("Failed to read RTP packet",
							zap.Error(err),
							zap.String("track_id", t.ID()),
							zap.String("request_id", requestID))
						return
					}
					if err = rtpPkt.Unmarshal(buf[:i]); err != nil {
						bufferPool.Put(buf)
						logger.Error("Failed to unmarshal RTP packet",
							zap.Error(err),
							zap.String("track_id", t.ID()),
							zap.String("request_id", requestID))
						return
					}
					rtpPkt.Extension = false
					rtpPkt.Extensions = nil
					if err = trackLocal.WriteRTP(rtpPkt); err != nil {
						bufferPool.Put(buf)
						logger.Error("Failed to write RTP packet",
							zap.Error(err),
							zap.String("track_id", t.ID()),
							zap.String("request_id", requestID))
						return
					}
					bufferPool.Put(buf)
				}
			}
		}()
	})

	pc.OnICEConnectionStateChange(func(is webrtc.ICEConnectionState) {
		logger.Info("ICE connection state changed",
			zap.String("state", is.String()),
			zap.String("uuid", kws.GetUUID()),
			zap.String("request_id", requestID))
	})
}

func websocketHandler(ep *socketio.EventPayload) {
	requestID := generateRequestID()
	message := &websocketMessage{}
	if err := json.Unmarshal(ep.Data, &message); err != nil {
		logger.Error("Failed to unmarshal message",
			zap.Error(err),
			zap.String("request_id", requestID))
		return
	}

	roomID := ep.Kws.GetStringAttribute("room_id")
	if roomID == "" {
		logger.Error("No room ID associated with connection",
			zap.String("uuid", ep.Kws.GetUUID()),
			zap.String("request_id", requestID))
		return
	}

	roomsLock.Lock()
	r, exists := rooms[roomID]
	if !exists {
		r = &room{
			connections: make([]*connection, 0),
			trackLocals: make(map[string]*webrtc.TrackLocalStaticRTP),
			trackCount:  0,
		}
		rooms[roomID] = r
		logger.Info("New room created in websocketHandler",
			zap.String("room_id", roomID),
			zap.String("request_id", requestID))
	}
	roomsLock.Unlock()

	r.lock.RLock()
	var conn *connection
	for _, c := range r.connections {
		if c.kws.GetUUID() == ep.Kws.GetUUID() {
			conn = c
			break
		}
	}
	r.lock.RUnlock()

	if conn == nil || conn.pc == nil {
		logger.Error("No PeerConnection found",
			zap.String("uuid", ep.Kws.GetUUID()),
			zap.String("request_id", requestID))
		return
	}

	switch message.Event {
	case "offer":
		offer := webrtc.SessionDescription{}
		if err := json.Unmarshal([]byte(message.Data), &offer); err != nil {
			logger.Error("Failed to unmarshal offer",
				zap.Error(err),
				zap.String("request_id", requestID))
			return
		}
		if err := conn.pc.SetRemoteDescription(offer); err != nil {
			logger.Error("Failed to set remote description",
				zap.Error(err),
				zap.String("request_id", requestID))
			return
		}
		answer, err := conn.pc.CreateAnswer(nil)
		if err != nil {
			logger.Error("Failed to create answer",
				zap.Error(err),
				zap.String("request_id", requestID))
			return
		}
		if err := conn.pc.SetLocalDescription(answer); err != nil {
			logger.Error("Failed to set local description",
				zap.Error(err),
				zap.String("request_id", requestID))
			return
		}
		answerString, err := json.Marshal(answer)
		if err != nil {
			logger.Error("Failed to marshal answer",
				zap.Error(err),
				zap.String("request_id", requestID))
			return
		}
		if err := writeJSON(conn.kws, &conn.lock, &websocketMessage{
			Event: "answer",
			Data:  string(answerString),
		}, requestID); err != nil {
			logger.Error("Failed to send answer",
				zap.Error(err),
				zap.String("request_id", requestID))
			return
		}
		logger.Info("Offer processed and answer sent",
			zap.String("uuid", conn.kws.GetUUID()),
			zap.String("request_id", requestID))

	case "candidate":
		candidate := webrtc.ICECandidateInit{}
		if err := json.Unmarshal([]byte(message.Data), &candidate); err != nil {
			logger.Error("Failed to unmarshal candidate",
				zap.Error(err),
				zap.String("request_id", requestID))
			return
		}
		if err := conn.pc.AddICECandidate(candidate); err != nil {
			logger.Error("Failed to add ICE candidate",
				zap.Error(err),
				zap.String("request_id", requestID))
			return
		}

	case "answer":
		answer := webrtc.SessionDescription{}
		if err := json.Unmarshal([]byte(message.Data), &answer); err != nil {
			logger.Error("Failed to unmarshal answer",
				zap.Error(err),
				zap.String("request_id", requestID))
			return
		}
		if err := conn.pc.SetRemoteDescription(answer); err != nil {
			logger.Error("Failed to set remote description",
				zap.Error(err),
				zap.String("request_id", requestID))
			return
		}
		logger.Info("Answer processed",
			zap.String("uuid", conn.kws.GetUUID()),
			zap.String("request_id", requestID))
		r.signalPeerConnections(requestID)

	default:
		logger.Warn("Unknown message event",
			zap.String("event", message.Event),
			zap.String("request_id", requestID))
	}
}

func (r *room) addTrack(conn *connection, t *webrtc.TrackRemote) *webrtc.TrackLocalStaticRTP {
	r.lock.Lock()
	defer func() {
		r.lock.Unlock()
		r.signalPeerConnections(generateRequestID())
	}()

	trackLocal, err := webrtc.NewTrackLocalStaticRTP(t.Codec().RTPCodecCapability, t.ID(), t.StreamID())
	if err != nil {
		logger.Error("Failed to create track local",
			zap.Error(err),
			zap.String("track_id", t.ID()),
			zap.String("uuid", conn.kws.GetUUID()),
			zap.String("request_id", generateRequestID()))
		return nil
	}

	if oldTrack, exists := r.trackLocals[t.ID()]; exists {
		logger.Warn("Replacing existing track",
			zap.String("track_id", t.ID()),
			zap.String("uuid", conn.kws.GetUUID()),
			zap.Uint32("ssrc", uint32(t.SSRC())))
		r.removeTrack(oldTrack)
	}

	r.trackLocals[t.ID()] = trackLocal
	conn.tracks[t.ID()] = trackLocal
	atomic.AddInt64(&r.trackCount, 1)
	logger.Info("Track added",
		zap.String("track_id", t.ID()),
		zap.String("uuid", conn.kws.GetUUID()),
		zap.Int64("track_count", r.trackCount),
		zap.Uint32("ssrc", uint32(t.SSRC())))
	return trackLocal
}

func (r *room) removeTrack(t *webrtc.TrackLocalStaticRTP) {
	r.lock.Lock()
	defer func() {
		r.lock.Unlock()
		r.signalPeerConnections(generateRequestID())
	}()
	delete(r.trackLocals, t.ID())
	atomic.AddInt64(&r.trackCount, -1)
	logger.Info("Track removed",
		zap.String("track_id", t.ID()),
		zap.Int64("track_count", r.trackCount))
}

func (r *room) signalPeerConnections(requestID string) {
	r.lock.Lock()
	defer r.lock.Unlock()

	for _, conn := range r.connections {
		if time.Since(conn.lastSignal) < conn.signalDebounce {
			logger.Debug("Skipping signaling due to debounce",
				zap.String("uuid", conn.kws.GetUUID()),
				zap.String("request_id", requestID))
			continue
		}

		if conn.pc.ConnectionState() == webrtc.PeerConnectionStateClosed {
			continue
		}

		if conn.pc.SignalingState() != webrtc.SignalingStateStable && conn.pc.SignalingState() != webrtc.SignalingStateHaveRemoteOffer {
			logger.Debug("Skipping offer creation due to non-stable signaling state",
				zap.String("uuid", conn.kws.GetUUID()),
				zap.String("state", conn.pc.SignalingState().String()),
				zap.String("request_id", requestID))
			continue
		}

		existingSenders := map[string]bool{}
		for _, sender := range conn.pc.GetSenders() {
			if sender.Track() == nil {
				continue
			}
			existingSenders[sender.Track().ID()] = true
			if _, ok := r.trackLocals[sender.Track().ID()]; !ok {
				if err := conn.pc.RemoveTrack(sender); err != nil {
					logger.Error("Failed to remove track",
						zap.Error(err),
						zap.String("uuid", conn.kws.GetUUID()),
						zap.String("request_id", requestID))
					continue
				}
			}
		}

		for _, receiver := range conn.pc.GetReceivers() {
			if receiver.Track() == nil {
				continue
			}
			existingSenders[receiver.Track().ID()] = true
		}

		for trackID := range r.trackLocals {
			if _, ok := existingSenders[trackID]; !ok {
				_, err := conn.pc.AddTransceiverFromTrack(r.trackLocals[trackID], webrtc.RTPTransceiverInit{
					Direction: webrtc.RTPTransceiverDirectionSendonly,
				})
				if err != nil {
					logger.Error("Failed to add track",
						zap.Error(err),
						zap.String("track_id", trackID),
						zap.String("uuid", conn.kws.GetUUID()),
						zap.String("request_id", requestID))
					continue
				}
			}
		}

		offer, err := conn.pc.CreateOffer(nil)
		if err != nil {
			logger.Error("Failed to create offer",
				zap.Error(err),
				zap.String("uuid", conn.kws.GetUUID()),
				zap.String("request_id", requestID))
			continue
		}

		if err = conn.pc.SetLocalDescription(offer); err != nil {
			logger.Error("Failed to set local description",
				zap.Error(err),
				zap.String("uuid", conn.kws.GetUUID()),
				zap.String("request_id", requestID))
			continue
		}

		offerString, err := json.Marshal(offer)
		if err != nil {
			logger.Error("Failed to marshal offer",
				zap.Error(err),
				zap.String("uuid", conn.kws.GetUUID()),
				zap.String("request_id", requestID))
			continue
		}

		if err := writeJSON(conn.kws, &conn.lock, &websocketMessage{
			Event: "offer",
			Data:  string(offerString),
		}, requestID); err != nil {
			logger.Error("Failed to send offer",
				zap.Error(err),
				zap.String("uuid", conn.kws.GetUUID()),
				zap.String("request_id", requestID))
			continue
		}

		conn.lastSignal = time.Now()
		logger.Info("Offer sent",
			zap.String("uuid", conn.kws.GetUUID()),
			zap.String("request_id", requestID))
	}

	if len(r.connections) > 0 {
		go func() {
			time.Sleep(time.Second * 5)
			if atomic.LoadUint32(&serverRunning) == 1 {
				r.signalPeerConnections(generateRequestID())
			}
		}()
	}
}

func (r *room) dispatchKeyFrame() {
	r.lock.RLock()
	defer r.lock.RUnlock()

	for i := range r.connections {
		for _, receiver := range r.connections[i].pc.GetReceivers() {
			if receiver.Track() == nil {
				continue
			}
			_ = r.connections[i].pc.WriteRTCP([]rtcp.Packet{
				&rtcp.PictureLossIndication{
					MediaSSRC: uint32(receiver.Track().SSRC()),
				},
			})
		}
	}
}
