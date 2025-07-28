package main

import (
	"encoding/json"
	"flag"
	"sync"
	"text/template"
	"time"

	"github.com/gofiber/contrib/socketio"
	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/log"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

var (
	addr          = flag.String("addr", ":8080", "http service address")
	indexTemplate = &template.Template{}
	roomsLock     sync.RWMutex
	rooms         = make(map[string]*room)
)

type websocketMessage struct {
	Event string `json:"event"`
	Data  string `json:"data"`
}

type connection struct {
	pc     *webrtc.PeerConnection
	kws    *socketio.Websocket
	lock   sync.Mutex
	tracks map[string]*webrtc.TrackLocalStaticRTP
	roomID string
}

type room struct {
	lock        sync.RWMutex
	connections []*connection
	trackLocals map[string]*webrtc.TrackLocalStaticRTP
}

func writeJSON(kws *socketio.Websocket, lock *sync.Mutex, v interface{}) error {
	lock.Lock()
	defer lock.Unlock()
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	kws.Emit(data, socketio.TextMessage)
	return nil
}

func main() {
	flag.Parse()
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
		log.Infof("New connection: %s", ep.Kws.GetUUID())
	})

	socketio.On(socketio.EventMessage, websocketHandler)

	socketio.On(socketio.EventDisconnect, func(ep *socketio.EventPayload) {
		roomID := ep.Kws.GetStringAttribute("room_id")
		if roomID == "" {
			log.Infof("Disconnected without room: %s", ep.Kws.GetUUID())
			return
		}

		roomsLock.RLock()
		r, exists := rooms[roomID]
		roomsLock.RUnlock()

		if !exists {
			log.Infof("Disconnected from non-existing room: %s", roomID)
			return
		}

		r.lock.Lock()
		for i, conn := range r.connections {
			if conn.kws.GetUUID() == ep.Kws.GetUUID() {
				for trackID := range conn.tracks {
					if _, ok := r.trackLocals[trackID]; ok {
						delete(r.trackLocals, trackID)
					}
				}
				if err := conn.pc.Close(); err != nil {
					log.Errorf("Failed to close PeerConnection: %v", err)
				}
				r.connections = append(r.connections[:i], r.connections[i+1:]...)
				break
			}
		}

		// Remove room if empty
		if len(r.connections) == 0 {
			roomsLock.Lock()
			delete(rooms, roomID)
			roomsLock.Unlock()
			log.Infof("Room deleted: %s", roomID)
		} else {
			r.lock.Unlock()
			r.signalPeerConnections()
			r.lock.Lock()
		}

		r.lock.Unlock()
		log.Infof("Disconnected: %s from room %s", ep.Kws.GetUUID(), roomID)
	})

	app.Get("/websocket", socketio.New(func(kws *socketio.Websocket) {
		roomID := kws.Query("room_id")
		if roomID == "" {
			log.Error("Room ID not provided")
			return
		}

		kws.SetAttribute("room_id", roomID)

		mediaEngine := &webrtc.MediaEngine{}
		if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
			log.Errorf("Failed to register codecs: %v", err)
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
			log.Errorf("Failed to create PeerConnection: %v", err)
			return
		}

		for _, typ := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
			if _, err := pc.AddTransceiverFromKind(typ, webrtc.RTPTransceiverInit{
				Direction: webrtc.RTPTransceiverDirectionSendrecv,
			}); err != nil {
				log.Errorf("Failed to add transceiver: %v", err)
				return
			}
		}

		conn := &connection{
			pc:     pc,
			kws:    kws,
			tracks: make(map[string]*webrtc.TrackLocalStaticRTP),
			roomID: roomID,
		}

		roomsLock.Lock()
		r, exists := rooms[roomID]
		if !exists {
			r = &room{
				connections: make([]*connection, 0),
				trackLocals: make(map[string]*webrtc.TrackLocalStaticRTP),
			}
			rooms[roomID] = r
			log.Infof("New room created: %s", roomID)
		}
		roomsLock.Unlock()

		r.lock.Lock()
		r.connections = append(r.connections, conn)
		r.lock.Unlock()

		setupWebRTCHandlers(conn, r)

		r.signalPeerConnections()
	}))

	go func() {
		for range time.NewTicker(time.Second * 3).C {
			roomsLock.RLock()
			for _, r := range rooms {
				r.dispatchKeyFrame()
			}
			roomsLock.RUnlock()
		}
	}()

	if err := app.Listen(*addr); err != nil {
		log.Errorf("Failed to start server: %v", err)
	}
}

func setupWebRTCHandlers(conn *connection, r *room) {
	pc := conn.pc
	kws := conn.kws

	pc.OnICECandidate(func(i *webrtc.ICECandidate) {
		if i == nil {
			return
		}
		candidateString, err := json.Marshal(i.ToJSON())
		if err != nil {
			log.Errorf("Failed to marshal candidate: %v", err)
			return
		}
		if err := writeJSON(kws, &conn.lock, &websocketMessage{
			Event: "candidate",
			Data:  string(candidateString),
		}); err != nil {
			log.Errorf("Failed to send candidate: %v", err)
		}
	})

	pc.OnConnectionStateChange(func(p webrtc.PeerConnectionState) {
		switch p {
		case webrtc.PeerConnectionStateFailed:
			if err := pc.Close(); err != nil {
				log.Errorf("Failed to close PeerConnection: %v", err)
			}
		case webrtc.PeerConnectionStateClosed:
			r.signalPeerConnections()
		}
	})

	pc.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		trackLocal := r.addTrack(conn, t)
		if trackLocal == nil {
			return
		}

		go func() {
			defer r.removeTrack(trackLocal)
			buf := make([]byte, 1500)
			rtpPkt := &rtp.Packet{}
			for {
				i, _, err := t.Read(buf)
				if err != nil {
					return
				}
				if err = rtpPkt.Unmarshal(buf[:i]); err != nil {
					log.Errorf("Failed to unmarshal RTP packet: %v", err)
					return
				}
				rtpPkt.Extension = false
				rtpPkt.Extensions = nil
				if err = trackLocal.WriteRTP(rtpPkt); err != nil {
					return
				}
			}
		}()
	})

	pc.OnICEConnectionStateChange(func(is webrtc.ICEConnectionState) {
		log.Infof("ICE connection state changed: %s", is)
	})
}

func websocketHandler(ep *socketio.EventPayload) {
	message := &websocketMessage{}
	if err := json.Unmarshal(ep.Data, &message); err != nil {
		log.Errorf("Failed to unmarshal message: %v", err)
		return
	}

	roomID := ep.Kws.GetStringAttribute("room_id")
	if roomID == "" {
		log.Error("No room ID associated with connection")
		return
	}

	roomsLock.RLock()
	r, exists := rooms[roomID]
	roomsLock.RUnlock()

	if !exists {
		log.Errorf("Room not found: %s", roomID)
		return
	}

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
		log.Errorf("No PeerConnection found for UUID: %s", ep.Kws.GetUUID())
		return
	}

	switch message.Event {
	case "candidate":
		candidate := webrtc.ICECandidateInit{}
		if err := json.Unmarshal([]byte(message.Data), &candidate); err != nil {
			log.Errorf("Failed to unmarshal candidate: %v", err)
			return
		}
		if err := conn.pc.AddICECandidate(candidate); err != nil {
			log.Errorf("Failed to add ICE candidate: %v", err)
			return
		}

	case "answer":
		answer := webrtc.SessionDescription{}
		if err := json.Unmarshal([]byte(message.Data), &answer); err != nil {
			log.Errorf("Failed to unmarshal answer: %v", err)
			return
		}
		if err := conn.pc.SetRemoteDescription(answer); err != nil {
			log.Errorf("Failed to set remote description: %v", err)
			return
		}

	default:
		log.Errorf("Unknown message event: %s", message.Event)
	}
}

func (r *room) addTrack(conn *connection, t *webrtc.TrackRemote) *webrtc.TrackLocalStaticRTP {
	r.lock.Lock()
	defer func() {
		r.lock.Unlock()
		r.signalPeerConnections()
	}()

	trackLocal, err := webrtc.NewTrackLocalStaticRTP(t.Codec().RTPCodecCapability, t.ID(), t.StreamID())
	if err != nil {
		log.Errorf("Failed to create track local: %v", err)
		return nil
	}

	// Remove old track if exists
	if oldTrack, exists := r.trackLocals[t.ID()]; exists {
		r.removeTrack(oldTrack)
	}

	r.trackLocals[t.ID()] = trackLocal
	conn.tracks[t.ID()] = trackLocal
	return trackLocal
}

func (r *room) removeTrack(t *webrtc.TrackLocalStaticRTP) {
	r.lock.Lock()
	defer func() {
		r.lock.Unlock()
		r.signalPeerConnections()
	}()
	delete(r.trackLocals, t.ID())
}

func (r *room) signalPeerConnections() {
	r.lock.Lock()
	defer func() {
		r.lock.Unlock()
	}()

	attemptSync := func() (tryAgain bool) {
		for i := 0; i < len(r.connections); i++ {
			if r.connections[i].pc.ConnectionState() == webrtc.PeerConnectionStateClosed {
				r.connections = append(r.connections[:i], r.connections[i+1:]...)
				i--
				return true
			}

			existingSenders := map[string]bool{}
			for _, sender := range r.connections[i].pc.GetSenders() {
				if sender.Track() == nil {
					continue
				}
				existingSenders[sender.Track().ID()] = true
				if _, ok := r.trackLocals[sender.Track().ID()]; !ok {
					if err := r.connections[i].pc.RemoveTrack(sender); err != nil {
						return true
					}
				}
			}

			for _, receiver := range r.connections[i].pc.GetReceivers() {
				if receiver.Track() == nil {
					continue
				}
				existingSenders[receiver.Track().ID()] = true
			}

			for trackID := range r.trackLocals {
				if _, ok := existingSenders[trackID]; !ok {
					_, err := r.connections[i].pc.AddTransceiverFromTrack(r.trackLocals[trackID], webrtc.RTPTransceiverInit{
						Direction: webrtc.RTPTransceiverDirectionSendonly,
					})
					if err != nil {
						log.Errorf("Failed to add track %s to connection %s: %v", trackID, r.connections[i].kws.GetUUID(), err)
						continue
					}
				}
			}

			offer, err := r.connections[i].pc.CreateOffer(nil)
			if err != nil {
				return true
			}

			if err = r.connections[i].pc.SetLocalDescription(offer); err != nil {
				return true
			}

			offerString, err := json.Marshal(offer)
			if err != nil {
				log.Errorf("Failed to marshal offer: %v", err)
				return true
			}

			if err = writeJSON(r.connections[i].kws, &r.connections[i].lock, &websocketMessage{
				Event: "offer",
				Data:  string(offerString),
			}); err != nil {
				return true
			}
		}
		return false
	}

	for syncAttempt := 0; syncAttempt < 25; syncAttempt++ {
		if !attemptSync() {
			break
		}
	}

	if len(r.connections) > 0 {
		go func() {
			time.Sleep(time.Second * 3)
			r.signalPeerConnections()
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
