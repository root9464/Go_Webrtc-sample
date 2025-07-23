package peer

import (
	"encoding/json"
	"root/utils"

	"github.com/pion/webrtc/v4"
)

// offer_management.go
func (p *Peer) createAndSendOffer(pc *webrtc.PeerConnection, ws *utils.ThreadSafeWriter) error {
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	if err = pc.SetLocalDescription(offer); err != nil {
		return err
	}
	offerString, err := json.Marshal(offer)
	if err != nil {
		p.log.Errorf("Failed to marshal offer to json: %v", err)
		return err
	}
	p.log.Infof("Send offer to client: %v", offer)
	return ws.WS.WriteJSON(&utils.WebsocketMessage{
		Event: "offer",
		Data:  string(offerString),
	})
}
