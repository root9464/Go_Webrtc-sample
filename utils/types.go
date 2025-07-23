package utils

import (
	"sync"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

type PeerConnectionState struct {
	PeerConnection *webrtc.PeerConnection
	Websocket      *ThreadSafeWriter
}

type WebsocketMessage struct {
	Event string `json:"event"`
	Data  string `json:"data"`
}

type ThreadSafeWriter struct {
	WS *websocket.Conn
	MU *sync.Mutex
}
