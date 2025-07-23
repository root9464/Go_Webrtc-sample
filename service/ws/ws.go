package ws

import (
	"encoding/json"
	"net/http"
	"root/utils"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/pion/logging"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

type Peer interface {
	SignalPeerConnections()
}

type Track interface {
	AddTrack(t *webrtc.TrackRemote) *webrtc.TrackLocalStaticRTP
	RemoveTrack(t *webrtc.TrackLocalStaticRTP)
}

type WS struct {
	log             logging.LeveledLogger
	listLock        *sync.RWMutex
	peerConnections []utils.PeerConnectionState
	peer            Peer
	track           Track
}

func NewW(
	log logging.LeveledLogger,
	listLock *sync.RWMutex,
	peerConnections []utils.PeerConnectionState,
	peer Peer,
	track Track,

) *WS {
	return &WS{
		log:             log,
		listLock:        listLock,
		peerConnections: peerConnections,
		peer:            peer,
		track:           track,
	}
}

type threadSafeWriter struct {
	*websocket.Conn
	sync.Mutex
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func (ws *WS) WebsocketHandler(w http.ResponseWriter, r *http.Request) { // nolint
	// Upgrade HTTP request to Websocket
	unsafeConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		ws.log.Errorf("Failed to upgrade HTTP to Websocket: ", err)

		return
	}

	c := &utils.ThreadSafeWriter{WS: unsafeConn, MU: &sync.Mutex{}} // nolint

	// When this frame returns close the Websocket
	defer c.WS.Close() //nolint

	// Create new PeerConnection
	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		ws.log.Errorf("Failed to creates a PeerConnection: %v", err)

		return
	}

	// When this frame returns close the PeerConnection
	defer peerConnection.Close() //nolint

	// Accept one audio and one video track incoming
	for _, typ := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
		if _, err := peerConnection.AddTransceiverFromKind(typ, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		}); err != nil {
			ws.log.Errorf("Failed to add transceiver: %v", err)

			return
		}
	}

	// Add our new PeerConnection to global list
	ws.listLock.Lock()
	ws.peerConnections = append(ws.peerConnections, utils.PeerConnectionState{PeerConnection: peerConnection, Websocket: c})
	ws.listLock.Unlock()

	// Trickle ICE. Emit server candidate to client
	peerConnection.OnICECandidate(func(i *webrtc.ICECandidate) {
		if i == nil {
			return
		}
		// If you are serializing a candidate make sure to use ToJSON
		// Using Marshal will result in errors around `sdpMid`
		candidateString, err := json.Marshal(i.ToJSON())
		if err != nil {
			ws.log.Errorf("Failed to marshal candidate to json: %v", err)

			return
		}

		ws.log.Infof("Send candidate to client: %s", candidateString)

		if writeErr := c.WS.WriteJSON(&utils.WebsocketMessage{
			Event: "candidate",
			Data:  string(candidateString),
		}); writeErr != nil {
			ws.log.Errorf("Failed to write JSON: %v", writeErr)
		}
	})

	// If PeerConnection is closed remove it from global list
	peerConnection.OnConnectionStateChange(func(p webrtc.PeerConnectionState) {
		ws.log.Infof("Connection state change: %s", p)

		switch p {
		case webrtc.PeerConnectionStateFailed:
			if err := peerConnection.Close(); err != nil {
				ws.log.Errorf("Failed to close PeerConnection: %v", err)
			}
		case webrtc.PeerConnectionStateClosed:
			ws.peer.SignalPeerConnections()
		default:
		}
	})

	peerConnection.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		ws.log.Infof("Got remote track: Kind=%s, ID=%s, PayloadType=%d", t.Kind(), t.ID(), t.PayloadType())

		// Create a track to fan out our incoming video to all peers
		trackLocal := ws.track.AddTrack(t)
		defer ws.track.RemoveTrack(trackLocal)

		buf := make([]byte, 1500)
		rtpPkt := &rtp.Packet{}

		for {
			i, _, err := t.Read(buf)
			if err != nil {
				return
			}

			if err = rtpPkt.Unmarshal(buf[:i]); err != nil {
				ws.log.Errorf("Failed to unmarshal incoming RTP packet: %v", err)

				return
			}

			rtpPkt.Extension = false
			rtpPkt.Extensions = nil

			if err = trackLocal.WriteRTP(rtpPkt); err != nil {
				return
			}
		}
	})

	peerConnection.OnICEConnectionStateChange(func(is webrtc.ICEConnectionState) {
		ws.log.Infof("ICE connection state changed: %s", is)
	})

	// Signal for the new PeerConnection
	ws.peer.SignalPeerConnections()

	message := &utils.WebsocketMessage{}
	for {
		_, raw, err := c.WS.ReadMessage()
		if err != nil {
			ws.log.Errorf("Failed to read message: %v", err)

			return
		}

		ws.log.Infof("Got message: %s", raw)

		if err := json.Unmarshal(raw, &message); err != nil {
			ws.log.Errorf("Failed to unmarshal json to message: %v", err)

			return
		}

		switch message.Event {
		case "candidate":
			candidate := webrtc.ICECandidateInit{}
			if err := json.Unmarshal([]byte(message.Data), &candidate); err != nil {
				ws.log.Errorf("Failed to unmarshal json to candidate: %v", err)

				return
			}

			ws.log.Infof("Got candidate: %v", candidate)

			if err := peerConnection.AddICECandidate(candidate); err != nil {
				ws.log.Errorf("Failed to add ICE candidate: %v", err)

				return
			}
		case "answer":
			answer := webrtc.SessionDescription{}
			if err := json.Unmarshal([]byte(message.Data), &answer); err != nil {
				ws.log.Errorf("Failed to unmarshal json to answer: %v", err)

				return
			}

			ws.log.Infof("Got answer: %v", answer)

			if err := peerConnection.SetRemoteDescription(answer); err != nil {
				ws.log.Errorf("Failed to set remote description: %v", err)

				return
			}
		default:
			ws.log.Errorf("unknown message: %+v", message)
		}
	}
}
