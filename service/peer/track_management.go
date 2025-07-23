package peer

import "github.com/pion/webrtc/v4"

func (p *Peer) manageTracks(pc *webrtc.PeerConnection, existingSenders map[string]bool) error {
	for _, sender := range pc.GetSenders() {
		if sender.Track() == nil {
			continue
		}
		existingSenders[sender.Track().ID()] = true
		if _, ok := p.trackLocals[sender.Track().ID()]; !ok {
			if err := pc.RemoveTrack(sender); err != nil {
				return err
			}
		}
	}

	for _, receiver := range pc.GetReceivers() {
		if receiver.Track() == nil {
			continue
		}
		existingSenders[receiver.Track().ID()] = true
	}

	for trackID := range p.trackLocals {
		if _, ok := existingSenders[trackID]; !ok {
			if _, err := pc.AddTrack(p.trackLocals[trackID]); err != nil {
				return err
			}
		}
	}
	return nil
}
