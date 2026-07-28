// Package membership provides a transport-independent, signed membership
// protocol reference for CRDT replication groups. Gossip reports liveness but
// never changes the active set; only a signed View may fence a replica or
// change the tombstone-GC membership epoch.
package membership

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sort"
	"strings"
)

const (
	viewDomain      = "darkinno/crdt/membership-view/v1"
	maxMembers      = 1024
	maxMemberIDSize = 256
)

var (
	ErrInvalidView      = errors.New("membership: invalid view")
	ErrInvalidSignature = errors.New("membership: invalid signature")
	ErrViewRollback     = errors.New("membership: view epoch rollback")
	ErrViewFork         = errors.New("membership: view predecessor mismatch")
	ErrGroupMismatch    = errors.New("membership: replication group mismatch")
	ErrMissingView      = errors.New("membership: missing persisted view")
)

// Member is one stable logical replica authorized by a View. Incarnation must
// advance when a previously fenced replica is admitted again; a process must
// never silently resume replication with an older incarnation.
type Member struct {
	ID          string
	PublicKey   ed25519.PublicKey
	Incarnation uint64
}

// View is the authoritative active-member set for exactly one replication
// group. ManifestHash binds this control-plane decision to the application
// replication manifest. PreviousHash forms an append-only view chain.
type View struct {
	GroupID      string
	Epoch        uint64
	PreviousHash [sha256.Size]byte
	ManifestHash [sha256.Size]byte
	Members      []Member
	Signature    []byte
}

// SignView returns a canonical signed copy of view. The signing key is owned
// by the application's membership authority, not by an ordinary replica.
func SignView(view View, privateKey ed25519.PrivateKey) (View, error) {
	if len(privateKey) != ed25519.PrivateKeySize || !view.validUnsigned() {
		return View{}, ErrInvalidView
	}
	view.Signature = nil
	payload, err := view.signingBytes()
	if err != nil {
		return View{}, err
	}
	view.Signature = ed25519.Sign(privateKey, payload)
	return cloneView(view), nil
}

// VerifyView checks structure and authority signature. It does not decide
// whether the view follows a local predecessor; Manager.Install performs that
// stateful check after loading the durable current view.
func VerifyView(view View, authorityKey ed25519.PublicKey) error {
	if len(authorityKey) != ed25519.PublicKeySize || !view.validUnsigned() || len(view.Signature) != ed25519.SignatureSize {
		return ErrInvalidView
	}
	payload, err := view.signingBytes()
	if err != nil || !ed25519.Verify(authorityKey, payload, view.Signature) {
		return ErrInvalidSignature
	}
	return nil
}

// Hash returns a stable digest of the signed view. It is the predecessor and
// receipt binding value, not a substitute for signature verification.
func (v View) Hash() [sha256.Size]byte {
	payload, err := v.signingBytes()
	if err != nil {
		return [sha256.Size]byte{}
	}
	return sha256.Sum256(append(payload, v.Signature...))
}

// Member returns a detached copy of the active member identified by id.
func (v View) Member(id string) (Member, bool) {
	index := sort.Search(len(v.Members), func(index int) bool { return v.Members[index].ID >= id })
	if index >= len(v.Members) || v.Members[index].ID != id {
		return Member{}, false
	}
	return cloneMember(v.Members[index]), true
}

func (v View) validUnsigned() bool {
	if strings.TrimSpace(v.GroupID) == "" || v.Epoch == 0 || len(v.Members) == 0 || len(v.Members) > maxMembers || isZeroHash(v.ManifestHash) {
		return false
	}
	previous := ""
	for _, member := range v.Members {
		if strings.TrimSpace(member.ID) == "" || len(member.ID) > maxMemberIDSize || member.ID <= previous || len(member.PublicKey) != ed25519.PublicKeySize || member.Incarnation == 0 {
			return false
		}
		previous = member.ID
	}
	return true
}

func (v View) signingBytes() ([]byte, error) {
	if !v.validUnsigned() {
		return nil, ErrInvalidView
	}
	encoded := make([]byte, 0, len(viewDomain)+len(v.GroupID)+len(v.Members)*(ed25519.PublicKeySize+64)+128)
	encoded = appendString(encoded, viewDomain)
	encoded = appendString(encoded, v.GroupID)
	encoded = binary.AppendUvarint(encoded, v.Epoch)
	encoded = append(encoded, v.PreviousHash[:]...)
	encoded = append(encoded, v.ManifestHash[:]...)
	encoded = binary.AppendUvarint(encoded, uint64(len(v.Members)))
	for _, member := range v.Members {
		encoded = appendString(encoded, member.ID)
		encoded = append(encoded, member.PublicKey...)
		encoded = binary.AppendUvarint(encoded, member.Incarnation)
	}
	return encoded, nil
}

func appendString(dst []byte, value string) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(value)))
	return append(dst, value...)
}

func isZeroHash(value [sha256.Size]byte) bool {
	return value == [sha256.Size]byte{}
}

func cloneMember(member Member) Member {
	member.PublicKey = append(ed25519.PublicKey(nil), member.PublicKey...)
	return member
}

func cloneView(view View) View {
	view.Signature = append([]byte(nil), view.Signature...)
	members := make([]Member, len(view.Members))
	for index, member := range view.Members {
		members[index] = cloneMember(member)
	}
	view.Members = members
	return view
}
