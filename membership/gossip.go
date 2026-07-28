package membership

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

const gossipDomain = "darkinno/crdt/membership-gossip/v1"

var ErrInvalidGossip = errors.New("membership: invalid gossip message")

// Liveness is deliberately separate from membership authorization. Suspect
// means an observer has not heard a heartbeat recently; it never permits the
// observer to remove a replica from a signed View or from tombstone GC.
type Liveness uint8

const (
	Alive Liveness = iota + 1
	Suspect
)

// GossipMessage is a signed heartbeat that may be forwarded over any
// application-selected transport. Counter is monotonic within an incarnation.
type GossipMessage struct {
	GroupID     string
	Epoch       uint64
	ViewHash    [sha256.Size]byte
	From        string
	Incarnation uint64
	Counter     uint64
	Signature   []byte
}

// LivenessEvent is emitted on a local state transition and is intended for
// observability or an external authority's decision process.
type LivenessEvent struct {
	MemberID string
	State    Liveness
}

type livenessRecord struct {
	incarnation uint64
	counter     uint64
	lastSeen    time.Time
	state       Liveness
}

// Gossip is a compact SWIM-style heartbeat/reference fanout engine. It signs
// source heartbeats, verifies them against the active View, and exposes
// deterministic peer selection for embedding transports. It does not open a
// socket, decide membership, or make a failure detector authoritative.
type Gossip struct {
	mu           sync.Mutex
	view         View
	self         Member
	privateKey   ed25519.PrivateKey
	suspectAfter time.Duration
	counter      uint64
	records      map[string]livenessRecord
}

func NewGossip(view View, selfID string, privateKey ed25519.PrivateKey, suspectAfter time.Duration) (*Gossip, error) {
	return NewGossipAt(view, selfID, privateKey, suspectAfter, time.Now())
}

// NewGossipAt is NewGossip with an explicit local start time. It makes failure
// detector tests reproducible and starts the suspect timer for every active
// peer, including peers that have not yet sent a heartbeat.
func NewGossipAt(view View, selfID string, privateKey ed25519.PrivateKey, suspectAfter time.Duration, startedAt time.Time) (*Gossip, error) {
	if !view.validUnsigned() || len(privateKey) != ed25519.PrivateKeySize || suspectAfter <= 0 || startedAt.IsZero() {
		return nil, ErrInvalidGossip
	}
	self, ok := view.Member(selfID)
	if !ok || !bytes.Equal(ed25519.PrivateKey(privateKey).Public().(ed25519.PublicKey), self.PublicKey) {
		return nil, ErrInvalidGossip
	}
	records := make(map[string]livenessRecord, len(view.Members)-1)
	for _, member := range view.Members {
		if member.ID != self.ID {
			records[member.ID] = livenessRecord{incarnation: member.Incarnation, lastSeen: startedAt, state: Alive}
		}
	}
	return &Gossip{
		view:         cloneView(view),
		self:         self,
		privateKey:   append(ed25519.PrivateKey(nil), privateKey...),
		suspectAfter: suspectAfter,
		records:      records,
	}, nil
}

// Heartbeat returns a newly signed heartbeat. The embedding transport sends it
// to some or all targets returned by Peers; no membership state is changed.
func (g *Gossip) Heartbeat() (GossipMessage, error) {
	if g == nil {
		return GossipMessage{}, ErrInvalidGossip
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.counter++
	message := GossipMessage{
		GroupID:     g.view.GroupID,
		Epoch:       g.view.Epoch,
		ViewHash:    g.view.Hash(),
		From:        g.self.ID,
		Incarnation: g.self.Incarnation,
		Counter:     g.counter,
	}
	payload, err := message.signingBytes()
	if err != nil {
		return GossipMessage{}, err
	}
	message.Signature = ed25519.Sign(g.privateKey, payload)
	return message, nil
}

// Peers deterministically selects up to fanout active peers. Determinism keeps
// simulations reproducible; changing heartbeat counters rotates selection
// without using a global random source.
func (g *Gossip) Peers(fanout int) []string {
	if g == nil || fanout <= 0 {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	type scoredPeer struct {
		id    string
		score [sha256.Size]byte
	}
	peers := make([]scoredPeer, 0, len(g.view.Members)-1)
	for _, member := range g.view.Members {
		if member.ID == g.self.ID {
			continue
		}
		seed := make([]byte, 0, len(g.view.GroupID)+len(g.self.ID)+len(member.ID)+32)
		seed = appendString(seed, g.view.GroupID)
		seed = appendString(seed, g.self.ID)
		seed = binary.AppendUvarint(seed, g.counter)
		seed = appendString(seed, member.ID)
		peers = append(peers, scoredPeer{id: member.ID, score: sha256.Sum256(seed)})
	}
	sort.Slice(peers, func(left, right int) bool {
		if peers[left].score != peers[right].score {
			return bytes.Compare(peers[left].score[:], peers[right].score[:]) < 0
		}
		return peers[left].id < peers[right].id
	})
	if fanout > len(peers) {
		fanout = len(peers)
	}
	result := make([]string, fanout)
	for index := range result {
		result[index] = peers[index].id
	}
	return result
}

// Observe verifies and records a received heartbeat. Duplicate/out-of-order
// messages are harmless. A later valid heartbeat transitions a suspect peer
// back to Alive but cannot change the signed membership view.
func (g *Gossip) Observe(message GossipMessage, now time.Time) (LivenessEvent, bool, error) {
	if g == nil || now.IsZero() {
		return LivenessEvent{}, false, ErrInvalidGossip
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if message.GroupID != g.view.GroupID || message.Epoch != g.view.Epoch || message.ViewHash != g.view.Hash() {
		return LivenessEvent{}, false, ErrInvalidGossip
	}
	member, ok := g.view.Member(message.From)
	if !ok || member.Incarnation != message.Incarnation || verifyGossip(message, member.PublicKey) != nil {
		return LivenessEvent{}, false, ErrInvalidGossip
	}
	record := g.records[message.From]
	if record.incarnation > message.Incarnation || (record.incarnation == message.Incarnation && record.counter >= message.Counter) {
		return LivenessEvent{}, false, nil
	}
	event := LivenessEvent{MemberID: message.From, State: Alive}
	emitted := record.state == Suspect
	g.records[message.From] = livenessRecord{incarnation: message.Incarnation, counter: message.Counter, lastSeen: now, state: Alive}
	return event, emitted, nil
}

// Suspects returns newly suspected members. It never includes self and it
// never modifies the active-member set used by Coordinator.
func (g *Gossip) Suspects(now time.Time) []LivenessEvent {
	if g == nil || now.IsZero() {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	events := make([]LivenessEvent, 0)
	for memberID, record := range g.records {
		if record.state == Alive && now.Sub(record.lastSeen) >= g.suspectAfter {
			record.state = Suspect
			g.records[memberID] = record
			events = append(events, LivenessEvent{MemberID: memberID, State: Suspect})
		}
	}
	sort.Slice(events, func(left, right int) bool { return events[left].MemberID < events[right].MemberID })
	return events
}

func (m GossipMessage) validUnsigned() bool {
	return strings.TrimSpace(m.GroupID) != "" && m.Epoch != 0 && !isZeroHash(m.ViewHash) && strings.TrimSpace(m.From) != "" && m.Incarnation != 0 && m.Counter != 0
}

func (m GossipMessage) signingBytes() ([]byte, error) {
	if !m.validUnsigned() {
		return nil, ErrInvalidGossip
	}
	encoded := make([]byte, 0, len(gossipDomain)+len(m.GroupID)+len(m.From)+96)
	encoded = appendString(encoded, gossipDomain)
	encoded = appendString(encoded, m.GroupID)
	encoded = binary.AppendUvarint(encoded, m.Epoch)
	encoded = append(encoded, m.ViewHash[:]...)
	encoded = appendString(encoded, m.From)
	encoded = binary.AppendUvarint(encoded, m.Incarnation)
	encoded = binary.AppendUvarint(encoded, m.Counter)
	return encoded, nil
}

func verifyGossip(message GossipMessage, publicKey ed25519.PublicKey) error {
	if len(publicKey) != ed25519.PublicKeySize || !message.validUnsigned() || len(message.Signature) != ed25519.SignatureSize {
		return ErrInvalidGossip
	}
	payload, err := message.signingBytes()
	if err != nil || !ed25519.Verify(publicKey, payload, message.Signature) {
		return ErrInvalidGossip
	}
	return nil
}
