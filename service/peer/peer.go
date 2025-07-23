package peer

import (
	"root/utils"
	"sync"

	"github.com/pion/logging"
	"github.com/pion/webrtc/v4"
)

type Peer struct {
	listLock        *sync.RWMutex
	peerConnections []utils.PeerConnectionState
	trackLocals     map[string]*webrtc.TrackLocalStaticRTP
	log             logging.LeveledLogger
}

func NewPeer(
	listLock *sync.RWMutex,
	peerConnections []utils.PeerConnectionState,
	trackLocals map[string]*webrtc.TrackLocalStaticRTP,
	log logging.LeveledLogger,
) *Peer {
	return &Peer{
		listLock:        listLock,
		peerConnections: peerConnections,
		trackLocals:     trackLocals,
		log:             log,
	}
}
func (p *Peer) SignalPeerConnections() {
	p.listLock.Lock()
	defer func() {
		p.listLock.Unlock()
		p.DispatchKeyFrame()
	}()

	p.SignalPeerConnectionss()
}
