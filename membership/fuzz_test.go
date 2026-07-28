package membership

import "testing"

func FuzzUnmarshalMembershipMessages(f *testing.F) {
	setup := newFixture(f, 1, "api")
	view, err := MarshalView(setup.view)
	if err != nil {
		f.Fatal(err)
	}
	gossip, err := NewGossip(setup.view, "api", setup.members["api"], 1)
	if err != nil {
		f.Fatal(err)
	}
	message, err := gossip.Heartbeat()
	if err != nil {
		f.Fatal(err)
	}
	gossipBytes, err := MarshalGossipMessage(message)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(view)
	f.Add(gossipBytes)
	f.Add([]byte{wireVersion})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = UnmarshalView(data)
		_, _ = UnmarshalGossipMessage(data)
		_, _ = UnmarshalReceipt(data)
	})
}
