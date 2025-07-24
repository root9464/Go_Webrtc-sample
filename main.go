package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/logging"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

var (
	addr   = flag.String("addr", ":8080", "http service address")
	cert   = flag.String("cert", "./cert.pem", "TLS certificate file")
	key    = flag.String("key", "./key.pem", "TLS private key file")
	logger = logging.NewDefaultLoggerFactory().NewLogger("sfu-ws")
)

type SFU struct {
	peers       []peerConnection
	trackLocals map[string]*webrtc.TrackLocalStaticRTP
	mu          sync.RWMutex
}

type peerConnection struct {
	pc   *webrtc.PeerConnection
	conn *threadSafeWriter
}

type threadSafeWriter struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

type websocketMessage struct {
	Event string `json:"event"`
	Data  string `json:"data"`
}

func (t *threadSafeWriter) WriteJSON(v any) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.conn.WriteJSON(v)
}

func NewSFU() *SFU {
	return &SFU{
		trackLocals: make(map[string]*webrtc.TrackLocalStaticRTP),
	}
}

func (s *SFU) addPeer(pc *webrtc.PeerConnection, conn *threadSafeWriter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.peers = append(s.peers, peerConnection{pc, conn})
}

func (s *SFU) addTrack(t *webrtc.TrackRemote) (*webrtc.TrackLocalStaticRTP, error) {
	trackLocal, err := webrtc.NewTrackLocalStaticRTP(t.Codec().RTPCodecCapability, t.ID(), t.StreamID())
	if err != nil {
		return nil, fmt.Errorf("create track: %w", err)
	}
	s.mu.Lock()
	s.trackLocals[t.ID()] = trackLocal
	s.mu.Unlock()
	logger.Infof("Added track: %v", trackLocal.ID())
	return trackLocal, nil
}

func (s *SFU) removeTrack(track *webrtc.TrackLocalStaticRTP) {
	s.mu.Lock()
	delete(s.trackLocals, track.ID())
	s.mu.Unlock()
}

func (s *SFU) signalPeers() error {
	peers := s.activePeers()
	for _, peer := range peers {
		if err := s.updatePeerTracks(peer); err != nil {
			return err
		}
		if err := s.sendOffer(peer); err != nil {
			return err
		}
	}
	s.dispatchKeyFrames()
	return nil
}

func (s *SFU) activePeers() []peerConnection {
	s.mu.Lock()
	defer s.mu.Unlock()

	var active []peerConnection
	for _, peer := range s.peers {
		if peer.pc.ConnectionState() != webrtc.PeerConnectionStateClosed {
			active = append(active, peer)
		}
	}
	s.peers = active
	return active
}

func (s *SFU) updatePeerTracks(peer peerConnection) error {
	senders := peer.pc.GetSenders()
	for _, sender := range senders {
		if sender.Track() == nil {
			continue
		}
		if _, ok := s.trackLocals[sender.Track().ID()]; !ok {
			if err := peer.pc.RemoveTrack(sender); err != nil {
				return fmt.Errorf("remove track: %w", err)
			}
		}
	}

	receivers := peer.pc.GetReceivers()
	existingTracks := make(map[string]bool, len(receivers))
	for _, receiver := range receivers {
		if track := receiver.Track(); track != nil {
			existingTracks[track.ID()] = true
		}
	}

	s.mu.RLock()
	for trackID, track := range s.trackLocals {
		if !existingTracks[trackID] {
			if _, err := peer.pc.AddTrack(track); err != nil {
				s.mu.RUnlock()
				return fmt.Errorf("add track: %w", err)
			}
		}
	}
	s.mu.RUnlock()
	return nil
}

func (s *SFU) sendOffer(peer peerConnection) error {
	offer, err := peer.pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create offer: %w", err)
	}

	if err := peer.pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set local description: %w", err)
	}

	offerData, err := json.Marshal(offer)
	if err != nil {
		return fmt.Errorf("marshal offer: %w", err)
	}

	logger.Infof("Sending offer: %v", string(offerData))
	return peer.conn.WriteJSON(&websocketMessage{
		Event: "offer",
		Data:  string(offerData),
	})
}

func (s *SFU) dispatchKeyFrames() {
	for _, peer := range s.peers {
		for _, receiver := range peer.pc.GetReceivers() {
			if track := receiver.Track(); track != nil && track.Kind() == webrtc.RTPCodecTypeVideo {
				_ = peer.pc.WriteRTCP([]rtcp.Packet{
					&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())},
				})
			}
		}
	}
}

func (s *SFU) handleWebSocket(w http.ResponseWriter, r *http.Request) error {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return fmt.Errorf("upgrade websocket: %w", err)
	}
	defer conn.Close()

	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
	}
	mediaEngine := webrtc.MediaEngine{}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeOpus,
			ClockRate:   48000,
			Channels:    2,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return fmt.Errorf("register audio codec: %w", err)
	}
	if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeVP8,
			ClockRate: 90000,
		},
		PayloadType: 96,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return fmt.Errorf("register video codec: %w", err)
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(&mediaEngine))
	pc, err := api.NewPeerConnection(config)
	if err != nil {
		return fmt.Errorf("create peer connection: %w", err)
	}
	defer pc.Close()

	for _, typ := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
		if _, err := pc.AddTransceiverFromKind(typ, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		}); err != nil {
			return fmt.Errorf("add transceiver: %w", err)
		}
	}

	writer := &threadSafeWriter{conn: conn}
	s.addPeer(pc, writer)

	pc.OnICECandidate(func(i *webrtc.ICECandidate) {
		if i == nil {
			return
		}
		candidateData, err := json.Marshal(i.ToJSON())
		if err != nil {
			logger.Errorf("marshal candidate: %v", err)
			return
		}
		if err := writer.WriteJSON(&websocketMessage{
			Event: "candidate",
			Data:  string(candidateData),
		}); err != nil {
			logger.Errorf("send candidate: %v", err)
		}
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			if state == webrtc.PeerConnectionStateClosed {
				s.signalPeers()
			}
		}
	})

	pc.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		trackLocal, err := s.addTrack(t)
		if err != nil {
			logger.Errorf("add track: %v", err)
			return
		}
		defer s.removeTrack(trackLocal)

		bufferSize := 1500
		if t.Kind() == webrtc.RTPCodecTypeAudio {
			bufferSize = 500
		}

		buf := make([]byte, bufferSize)
		for {
			n, _, err := t.Read(buf)
			if err != nil {
				return
			}
			var pkt rtp.Packet
			if err := pkt.Unmarshal(buf[:n]); err != nil {
				logger.Errorf("unmarshal RTP packet: %v", err)
				return
			}
			if t.Kind() != webrtc.RTPCodecTypeAudio {
				pkt.Extension = false
				pkt.Extensions = nil
			}
			if err := trackLocal.WriteRTP(&pkt); err != nil {
				return
			}
		}
	})

	if err := s.signalPeers(); err != nil {
		logger.Errorf("signal peers: %v", err)
	}

	var msg websocketMessage
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return nil
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			logger.Errorf("unmarshal message: %v", err)
			continue
		}
		switch msg.Event {
		case "candidate":
			var candidate webrtc.ICECandidateInit
			if err := json.Unmarshal([]byte(msg.Data), &candidate); err != nil {
				logger.Errorf("unmarshal candidate: %v", err)
				continue
			}
			if err := pc.AddICECandidate(candidate); err != nil {
				logger.Errorf("add ICE candidate: %v", err)
			}
		case "answer":
			var answer webrtc.SessionDescription
			if err := json.Unmarshal([]byte(msg.Data), &answer); err != nil {
				logger.Errorf("unmarshal answer: %v", err)
				continue
			}
			if err := pc.SetRemoteDescription(answer); err != nil {
				logger.Errorf("set remote description: %v", err)
			}
		default:
			logger.Errorf("unknown message event: %s", msg.Event)
		}
	}
}

func main() {
	flag.Parse()

	sfu := NewSFU()
	http.HandleFunc("/websocket", func(w http.ResponseWriter, r *http.Request) {
		if err := sfu.handleWebSocket(w, r); err != nil {
			logger.Errorf("handle websocket: %v", err)
		}
	})

	go func() {
		for range time.Tick(time.Second * 3) {
			sfu.dispatchKeyFrames()
		}
	}()

	if err := http.ListenAndServe(*addr, nil); err != nil {
		logger.Errorf("start server: %v", err)
	}
}
