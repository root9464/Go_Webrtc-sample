package peer

import (
	"root/utils"

	"github.com/pion/webrtc/v4"
)

func (p *Peer) cleanupClosedConnections() []utils.PeerConnectionState {
	p.listLock.Lock()
	defer p.listLock.Unlock()

	for i := 0; i < len(p.peerConnections); i++ {
		if p.peerConnections[i].PeerConnection.ConnectionState() == webrtc.PeerConnectionStateClosed {
			p.peerConnections = append(p.peerConnections[:i], p.peerConnections[i+1:]...)
			i-- // Adjust index after removal
		}
	}
	return p.peerConnections
}
