package peer

import "github.com/pion/rtcp"

func (p *Peer) DispatchKeyFrame() {
	p.listLock.Lock()
	defer p.listLock.Unlock()

	for i := range p.peerConnections {
		for _, receiver := range p.peerConnections[i].PeerConnection.GetReceivers() {
			if receiver.Track() == nil {
				continue
			}
			_ = p.peerConnections[i].PeerConnection.WriteRTCP([]rtcp.Packet{
				&rtcp.PictureLossIndication{
					MediaSSRC: uint32(receiver.Track().SSRC()),
				},
			})
		}
	}
}
