package track

import (
	"sync"

	"github.com/pion/webrtc/v4"
)

type Peer interface {
	SignalPeerConnections()
}

type Track struct {
	listLock    *sync.RWMutex
	trackLocals map[string]*webrtc.TrackLocalStaticRTP
	peer        Peer
}

func NewTrack(
	listLock *sync.RWMutex,
	trackLocals map[string]*webrtc.TrackLocalStaticRTP,
	peer Peer,
) *Track {
	return &Track{
		listLock:    listLock,
		trackLocals: trackLocals,
		peer:        peer,
	}
}

func (tr *Track) AddTrack(t *webrtc.TrackRemote) *webrtc.TrackLocalStaticRTP { // nolint
	tr.listLock.Lock()

	defer func() {
		tr.listLock.Unlock()
		tr.peer.SignalPeerConnections()
	}()

	// Create a new TrackLocal with the same codec as our incoming
	trackLocal, err := webrtc.NewTrackLocalStaticRTP(t.Codec().RTPCodecCapability, t.ID(), t.StreamID())
	if err != nil {
		panic(err)
	}

	tr.trackLocals[t.ID()] = trackLocal
	return trackLocal
}

// Remove from list of tracks and fire renegotation for all PeerConnections.
func (tr *Track) RemoveTrack(t *webrtc.TrackLocalStaticRTP) {
	tr.listLock.Lock()
	defer func() {
		tr.listLock.Unlock()
		tr.peer.SignalPeerConnections()
	}()

	delete(tr.trackLocals, t.ID())
}
