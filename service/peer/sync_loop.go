package peer

import "time"

func (p *Peer) attemptSync() (tryAgain bool) {
	for i := range p.peerConnections {
		existingSenders := map[string]bool{}
		if err := p.manageTracks(p.peerConnections[i].PeerConnection, existingSenders); err != nil {
			return true
		}
		if err := p.createAndSendOffer(p.peerConnections[i].PeerConnection, p.peerConnections[i].Websocket); err != nil {
			return true
		}
	}
	return false
}

func (p *Peer) SignalPeerConnectionss() {
	p.listLock.Lock()
	defer func() {
		p.listLock.Unlock()
		p.DispatchKeyFrame()
	}()

	for syncAttempt := 0; ; syncAttempt++ {
		if syncAttempt == 25 {
			go func() {
				time.Sleep(time.Second * 3)
				p.SignalPeerConnectionss()
			}()
		}

		// p.cleanupClosedConnections()
		if !p.attemptSync() {
			break
		}
	}
}
