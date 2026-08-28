package replica

import (
	"fmt"

	"github.com/im10furry/crdt"
)

// ExampleNewManifest binds one replication group to one explicitly admitted
// protocol. A manifest describes an agreement; the transport must still
// authenticate the peer before accepting its frames.
func ExampleNewManifest() {
	policy := crdt.ProtocolPolicy{AllowExperimental: true}
	manifest, err := NewManifest("notes/42", "example.com/note/v1", 1, Protocol{
		StateID:          crdt.TypeIDRGAState,
		DeltaID:          crdt.TypeIDRGADelta,
		SemanticsVersion: 1,
	}, policy)
	if err != nil {
		panic(err)
	}

	fmt.Println(manifest.GroupID, manifest.Epoch)
	// Output: notes/42 1
}
