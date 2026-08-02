package crdt

// ReplicationProfile is a curated, machine-readable starting point for one
// concrete CRDT protocol. It helps an application choose a merge rule from a
// business fact before it builds a manifest; it is not a security policy or a
// capacity configuration.
//
// The profile ID, frame type, and semantics version are stable integration
// inputs. Applications must still authenticate the exact manifest, authorize
// every sender, choose decoder and retention limits, and persist the recovery
// state described by HostRequirements.
type ReplicationProfile struct {
	// ID is the stable, case-sensitive profile identifier.
	ID string
	// Title is a short human-facing name for the underlying CRDT.
	Title string
	// Summary describes the merge rule in product language.
	Summary string
	// ConflictRule states the deterministic outcome of concurrent updates.
	ConflictRule string
	// RecommendedFor lists product facts that fit this merge rule.
	RecommendedFor []string
	// NotFor lists product decisions that must stay authoritative.
	NotFor []string
	// HostRequirements lists protocol-specific work that remains with the host.
	HostRequirements []string
	// RequiresCodecID reports whether the selected frame contract carries an
	// application-defined deterministic element codec ID.
	RequiresCodecID bool
	// FrameType is the canonical state/delta pair and semantics version.
	FrameType FrameType
}

// ReplicationProfiles returns defensive copies of every curated profile in a
// stable learning order. The returned values are metadata only: changing them
// cannot enable a frame type or alter protocol admission.
func ReplicationProfiles() []ReplicationProfile {
	profiles := make([]ReplicationProfile, len(replicationProfiles))
	for index, profile := range replicationProfiles {
		profiles[index] = cloneReplicationProfile(profile)
	}
	return profiles
}

// ReplicationProfileFor returns the profile named by the exact stable ID.
// IDs are intentionally not normalized: a configuration typo must not choose
// a different merge rule.
func ReplicationProfileFor(id string) (ReplicationProfile, bool) {
	for _, profile := range replicationProfiles {
		if profile.ID == id {
			return cloneReplicationProfile(profile), true
		}
	}
	return ReplicationProfile{}, false
}

func cloneReplicationProfile(profile ReplicationProfile) ReplicationProfile {
	profile.RecommendedFor = append([]string(nil), profile.RecommendedFor...)
	profile.NotFor = append([]string(nil), profile.NotFor...)
	profile.HostRequirements = append([]string(nil), profile.HostRequirements...)
	return profile
}

var replicationProfiles = [...]ReplicationProfile{
	{
		ID:               "counter/grow-only",
		Title:            "Grow-only counter",
		Summary:          "A monotonic total where each replica only increases its own component.",
		ConflictRule:     "Concurrent increments accumulate.",
		RecommendedFor:   []string{"completed jobs", "page views", "immutable historical milestones"},
		NotFor:           []string{"balances", "stock availability", "counters that reset or decrease"},
		HostRequirements: []string{"Authorize each actor to increment only its permitted fact.", "Choose an application overflow and retention policy."},
		FrameType:        FrameType{StateID: TypeIDGCounterState, DeltaID: TypeIDGCounterDelta, SemanticsVersion: SemanticsVersionGCounter},
	},
	{
		ID:               "counter/signed",
		Title:            "Positive-negative counter",
		Summary:          "An eventually consistent signed total built from independent positive and negative components.",
		ConflictRule:     "Concurrent increments and decrements accumulate independently.",
		RecommendedFor:   []string{"non-authoritative telemetry deltas", "eventually consistent signed totals"},
		NotFor:           []string{"preventing overspend", "inventory reservations", "exclusive allocation"},
		HostRequirements: []string{"Keep reservations, balances, and authorization decisions in an authoritative service."},
		FrameType:        FrameType{StateID: TypeIDPNCounterState, DeltaID: TypeIDPNCounterDelta, SemanticsVersion: SemanticsVersionPNCounter},
	},
	{
		ID:               "set/grow-only",
		Title:            "Grow-only set",
		Summary:          "A set of facts that may be added but never removed.",
		ConflictRule:     "Concurrent adds are unioned; an element never disappears.",
		RecommendedFor:   []string{"immutable audit labels", "facts that are never deleted"},
		NotFor:           []string{"membership that can be revoked", "permissions or access control", "tasks that close or reopen"},
		HostRequirements: []string{"Use one deterministic, versioned element codec ID across the group."},
		RequiresCodecID:  true,
		FrameType:        FrameType{StateID: TypeIDGSetState, DeltaID: TypeIDGSetDelta, SemanticsVersion: SemanticsVersionGSet},
	},
	{
		ID:               "set/add-wins",
		Title:            "Add-wins observed-remove set",
		Summary:          "Offline membership where independently added elements survive a concurrent remove.",
		ConflictRule:     "An add concurrent with a remove remains present.",
		RecommendedFor:   []string{"collaborative task membership", "offline labels", "shared selections"},
		NotFor:           []string{"access revocation", "exclusive booking", "a last-writer-wins document field"},
		HostRequirements: []string{"Persist state and HLC state atomically before reusing a replica ID.", "Use one deterministic, versioned element codec ID across the group.", "Retain tombstones until exact-acknowledgement compaction is authorized."},
		RequiresCodecID:  true,
		FrameType:        FrameType{StateID: TypeIDORSetState, DeltaID: TypeIDORSetDelta, SemanticsVersion: SemanticsVersionORSet, UsesHLC: true},
	},
	{
		ID:               "register/multi-value",
		Title:            "Multi-value register",
		Summary:          "One field where causally concurrent values must remain visible for the product to resolve.",
		ConflictRule:     "Concurrent writes are retained as multiple values.",
		RecommendedFor:   []string{"independently edited status proposals", "conflict-review queues"},
		NotFor:           []string{"silently choosing a winner", "authorization or workflow transitions"},
		HostRequirements: []string{"Persist state and causal snapshot atomically before reusing a replica ID.", "Make the product-level choice among Values explicit."},
		FrameType:        FrameType{StateID: TypeIDMVRegisterState, DeltaID: TypeIDMVRegisterDelta, SemanticsVersion: SemanticsVersionMVRegister},
	},
	{
		ID:               "register/last-writer-wins-set",
		Title:            "Last-writer-wins set",
		Summary:          "Membership where the product explicitly accepts HLC ordering as the winner selection rule.",
		ConflictRule:     "The greater HLC tag wins a concurrent add/remove decision.",
		RecommendedFor:   []string{"non-authoritative preferences with an accepted winner rule"},
		NotFor:           []string{"security-sensitive membership", "business decisions that need a human conflict review"},
		HostRequirements: []string{"Persist state and HLC state atomically before reusing a replica ID.", "Use one deterministic, versioned element codec ID across the group.", "Retain tombstones until exact-acknowledgement compaction is authorized."},
		RequiresCodecID:  true,
		FrameType:        FrameType{StateID: TypeIDLWWSetState, DeltaID: TypeIDLWWSetDelta, SemanticsVersion: SemanticsVersionLWWSet, UsesHLC: true},
	},
	{
		ID:               "register/last-writer-wins-map",
		Title:            "Last-writer-wins map",
		Summary:          "Keyed values where the product explicitly accepts HLC ordering for each key.",
		ConflictRule:     "The greater HLC tag wins for each key.",
		RecommendedFor:   []string{"non-authoritative keyed preferences with accepted winner semantics"},
		NotFor:           []string{"authorization state", "transactions spanning keys", "business conflict review"},
		HostRequirements: []string{"Persist state and HLC state atomically before reusing a replica ID.", "Use one deterministic, versioned value codec ID across the group.", "Retain tombstones until exact-acknowledgement compaction is authorized."},
		RequiresCodecID:  true,
		FrameType:        FrameType{StateID: TypeIDLWWMapState, DeltaID: TypeIDLWWMapDelta, SemanticsVersion: SemanticsVersionLWWMap, UsesHLC: true},
	},
	{
		ID:               "text/scalar-v1",
		Title:            "Scalar RGA text v1",
		Summary:          "The legacy scalar text protocol for an already negotiated v1 integration.",
		ConflictRule:     "Concurrent inserts have deterministic position and identifier ordering.",
		RecommendedFor:   []string{"existing scalar-v1 integrations that cannot yet migrate"},
		NotFor:           []string{"new text groups", "rich-text formatting", "business invariants"},
		HostRequirements: []string{"Persist state and HLC state atomically before reusing a replica ID.", "Keep tombstones until exact-acknowledgement compaction is authorized."},
		FrameType:        FrameType{StateID: TypeIDRGAState, DeltaID: TypeIDRGADelta, SemanticsVersion: SemanticsVersionRGA, UsesHLC: true},
	},
	{
		ID:               "text/run-v2",
		Title:            "Run RGA text v2",
		Summary:          "The compact default protocol for new plain collaborative text groups.",
		ConflictRule:     "Concurrent inserts have deterministic run ordering and deletes retain causal anchors.",
		RecommendedFor:   []string{"plain collaborative notes", "source text", "new browser or native text groups"},
		NotFor:           []string{"rich-text formatting", "cross-object transactions", "authorization decisions"},
		HostRequirements: []string{"Persist state and HLC state atomically before reusing a replica ID.", "Keep tombstones until exact-acknowledgement compaction is authorized.", "Use the exact run-v2 manifest; never fall back to scalar-v1 after a mismatch."},
		FrameType:        FrameType{StateID: TypeIDRGARunState, DeltaID: TypeIDRGARunDelta, SemanticsVersion: SemanticsVersionRGARun, UsesHLC: true},
	},
	{
		ID:               "text/packed-v3",
		Title:            "Packed RGA text v3",
		Summary:          "Plain collaborative text that retains scalar RGA positions while compacting dense local HLC runs.",
		ConflictRule:     "Concurrent inserts have deterministic position ordering and deletes retain causal anchors.",
		RecommendedFor:   []string{"large plain-text pastes", "bandwidth-sensitive Go or Wasm text groups", "snapshot-heavy collaborative notes"},
		NotFor:           []string{"mixed run-v2 and packed-v3 groups", "rich-text formatting", "authorization decisions"},
		HostRequirements: []string{"Bind the exact packed-v3 manifest before exchanging frames; never fall back to run-v2.", "Persist state and HLC state atomically before reusing a replica ID.", "Keep tombstones until exact-acknowledgement compaction is authorized."},
		FrameType:        FrameType{StateID: TypeIDRGAPackedState, DeltaID: TypeIDRGAPackedDelta, SemanticsVersion: SemanticsVersionRGAPacked, UsesHLC: true},
	},
	{
		ID:               "list/ordered",
		Title:            "Ordered RGA list",
		Summary:          "An ordered collection with immutable positions and add/remove semantics.",
		ConflictRule:     "Concurrent inserts order deterministically; a move is a remove plus a new item.",
		RecommendedFor:   []string{"offline ordered checklists", "append/edit/remove sequences"},
		NotFor:           []string{"semantic moves that must preserve identity", "nested arbitrary JSON"},
		HostRequirements: []string{"Persist state and HLC state atomically before reusing a replica ID.", "Use one deterministic, versioned element codec ID across the group.", "Keep tombstones until exact-acknowledgement compaction is authorized."},
		RequiresCodecID:  true,
		FrameType:        FrameType{StateID: TypeIDListRGAState, DeltaID: TypeIDListRGADelta, SemanticsVersion: SemanticsVersionListRGA, UsesHLC: true},
	},
	{
		ID:               "list/movable",
		Title:            "Move-aware RGA list",
		Summary:          "An ordered collection that needs a first-class move operation under one versioned protocol.",
		ConflictRule:     "Concurrent inserts, deletes, and moves resolve by the protocol's deterministic move rules.",
		RecommendedFor:   []string{"collaborative prioritization boards", "reorderable offline lists"},
		NotFor:           []string{"unbounded nested documents", "business transitions that need serialization"},
		HostRequirements: []string{"Persist state and HLC state atomically before reusing a replica ID.", "Use one deterministic, versioned element codec ID across the group.", "Keep tombstones until exact-acknowledgement compaction is authorized."},
		RequiresCodecID:  true,
		FrameType:        FrameType{StateID: TypeIDMoveRGAState, DeltaID: TypeIDMoveRGADelta, SemanticsVersion: SemanticsVersionMoveRGA, UsesHLC: true},
	},
	{
		ID:               "tree/observed-remove",
		Title:            "Observed-remove tree",
		Summary:          "A hierarchy with immutable parent links and add/remove semantics.",
		ConflictRule:     "Concurrent child additions survive; moving is modeled as remove plus a new node instance.",
		RecommendedFor:   []string{"collaborative category trees", "offline hierarchies without identity-preserving moves"},
		NotFor:           []string{"mutable parent links", "unbounded arbitrary nested objects"},
		HostRequirements: []string{"Persist state and HLC state atomically before reusing a replica ID.", "Use one deterministic, versioned node-value codec ID across the group.", "Keep tombstones until exact-acknowledgement compaction is authorized."},
		RequiresCodecID:  true,
		FrameType:        FrameType{StateID: TypeIDORTreeState, DeltaID: TypeIDORTreeDelta, SemanticsVersion: SemanticsVersionORTree, UsesHLC: true},
	},
	{
		ID:               "text/rich",
		Title:            "Rich text",
		Summary:          "Collaborative text with the library's bounded inline formatting model.",
		ConflictRule:     "Text and approved formatting changes merge under the rich-text v1 protocol.",
		RecommendedFor:   []string{"schema-bound collaborative editor content"},
		NotFor:           []string{"arbitrary HTML", "unvalidated attributes", "business authorization"},
		HostRequirements: []string{"Persist state and HLC state atomically before reusing a replica ID.", "Bind and validate the exact renderer and attribute schema.", "Sanitize rendered attributes and keep tombstones until exact-acknowledgement compaction is authorized."},
		FrameType:        FrameType{StateID: TypeIDRichTextState, DeltaID: TypeIDRichTextDelta, SemanticsVersion: SemanticsVersionRichText, UsesHLC: true},
	},
	{
		ID:               "document/tree-v1",
		Title:            "Nested document tree",
		Summary:          "A bounded, explicitly declared nested CRDT document protocol.",
		ConflictRule:     "Declared child CRDT operations merge within a versioned document-tree contract.",
		RecommendedFor:   []string{"structured collaborative documents with a fixed declared child schema"},
		NotFor:           []string{"arbitrary recursive JSON", "mixed unnegotiated type IDs", "dynamic plugin documents"},
		HostRequirements: []string{"Preflight identity, ownership, depth, width, bytes, operations, and pending work before mutation.", "Persist roots, declarations, frontier, HLC state, and local counter atomically."},
		FrameType:        FrameType{StateID: TypeIDDocumentTreeState, DeltaID: TypeIDDocumentTreeDelta, SemanticsVersion: SemanticsVersionDocumentTree, UsesHLC: true},
	},
}
