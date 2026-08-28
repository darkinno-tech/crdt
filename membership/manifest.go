package membership

import (
	"crypto/sha256"
	"encoding/binary"

	"github.com/im10furry/crdt/replica"
)

const manifestDomain = "im10furry/crdt/membership-manifest/v1"

// ManifestHash returns the control-plane binding digest for a replication
// manifest. It includes the membership/data-plane epoch, schema, codec, frame
// IDs, semantics version, and negotiated outer frame version; changing any of
// them requires a new signed View.
func ManifestHash(manifest replica.Manifest) [sha256.Size]byte {
	encoded := make([]byte, 0, len(manifestDomain)+len(manifest.GroupID)+len(manifest.SchemaID)+len(manifest.Protocol.CodecID)+96)
	encoded = appendString(encoded, manifestDomain)
	encoded = appendString(encoded, manifest.GroupID)
	encoded = appendString(encoded, manifest.SchemaID)
	encoded = binary.AppendUvarint(encoded, manifest.Epoch)
	encoded = binary.AppendUvarint(encoded, manifest.Protocol.StateID)
	encoded = binary.AppendUvarint(encoded, manifest.Protocol.DeltaID)
	encoded = appendString(encoded, manifest.Protocol.CodecID)
	encoded = binary.AppendUvarint(encoded, manifest.Protocol.SemanticsVersion)
	encoded = binary.AppendUvarint(encoded, manifest.Protocol.FrameFormatVersion())
	return sha256.Sum256(encoded)
}

// MatchesManifest reports whether view fences exactly manifest. It is intended
// for an authenticated handshake before a replica accepts state, delta, or GC
// receipt traffic. A matching group ID alone is not sufficient.
func (v View) MatchesManifest(manifest replica.Manifest) bool {
	return v.GroupID == manifest.GroupID && v.Epoch == manifest.Epoch && v.ManifestHash == ManifestHash(manifest)
}
