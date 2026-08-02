package text

import "errors"

var (
	// ErrInvalidAnchor reports an anchor with an unknown association or a
	// malformed non-root position. Anchors are local metadata, not CRDT deltas,
	// so callers must validate them before using untrusted presence payloads.
	ErrInvalidAnchor = errors.New("text: invalid anchor")
	// ErrAnchorGone reports that the referenced position was compacted. The
	// caller must drop or refresh the cursor instead of guessing a new offset.
	ErrAnchorGone = errors.New("text: anchor is no longer retained")
)

// AnchorAssociation selects the boundary represented by an Anchor.
//
// AnchorBefore resolves immediately before Position. AnchorAfter resolves
// immediately after Position itself, before any of its descendants. For the
// root Position{}, AnchorBefore is the document start and AnchorAfter is the
// document end.
type AnchorAssociation uint8

const (
	AnchorBefore AnchorAssociation = iota + 1
	AnchorAfter
)

// Anchor is stable cursor, selection, or comment metadata for an RGA boundary.
// It is deliberately not a CRDT state field: Anchor values can be stored or
// sent in a host-owned metadata record with MarshalBinary, but never in an RGA
// state/delta frame, snapshot, or unauthenticated peer message. A retained
// tombstone continues to resolve; exact tombstone compaction invalidates an
// anchor rather than relocating it.
type Anchor struct {
	Position    Position
	Association AnchorAssociation
}

// Valid reports whether anchor has a supported association and either a valid
// RGA position or the zero root position.
func (anchor Anchor) Valid() bool {
	if anchor.Association != AnchorBefore && anchor.Association != AnchorAfter {
		return false
	}
	return anchor.Position.Valid() || anchor.Position == (Position{})
}

// AnchorAt returns a relative cursor boundary for visible rune offset. The
// returned anchor references the following visible position; an end offset is
// represented by AnchorAfter on the root. Consequently it tracks the same
// logical boundary as concurrent inserts shift absolute offsets.
func (r *RGA) AnchorAt(offset int) (Anchor, error) {
	if r == nil {
		return Anchor{}, ErrNilText
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.anchorAtLocked(offset)
}

func (r *RGA) anchorAtLocked(offset int) (Anchor, error) {
	visible := visibleCount(r.sequence.root)
	if offset < 0 || offset > visible {
		return Anchor{}, ErrRange
	}
	if offset == visible {
		return Anchor{Association: AnchorAfter}, nil
	}
	position, ok := r.sequence.visibleAt(offset)
	if !ok {
		return Anchor{}, ErrRange
	}
	return Anchor{Position: position, Association: AnchorBefore}, nil
}

// ResolveAnchor returns the current visible rune offset represented by anchor.
// Resolution is O(log n) and does not materialize Positions. A tombstoned
// position remains a structural boundary; a compacted position returns
// ErrAnchorGone so callers never silently move a selection or cursor.
func (r *RGA) ResolveAnchor(anchor Anchor) (int, error) {
	if r == nil {
		return 0, ErrNilText
	}
	if !anchor.Valid() {
		return 0, ErrInvalidAnchor
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.resolveAnchorLocked(anchor)
}

func (r *RGA) resolveAnchorLocked(anchor Anchor) (int, error) {
	if !anchor.Position.Valid() {
		if anchor.Association == AnchorBefore {
			return 0, nil
		}
		return visibleCount(r.sequence.root), nil
	}
	entry := r.sequence.entry(anchor.Position)
	if entry == nil {
		return 0, ErrAnchorGone
	}
	offset := visibleRank(entry)
	if anchor.Association == AnchorAfter && entry.visible {
		offset++
	}
	return offset, nil
}

// AnchorRange is two relative RGA boundaries captured from one document
// revision. It is suitable for a selection or a comment range; Start and End
// intentionally preserve their caller-provided order so a backwards editor
// selection does not lose its direction. Comment hosts should require the
// resolved Start offset to be at or before End before attaching content.
//
// AnchorRange is host-owned metadata, not CRDT state. Its binary form is
// versioned by MarshalBinary and is safe to retain next to an authenticated
// document checkpoint, subject to the same compaction lifecycle as Anchor.
type AnchorRange struct {
	Start Anchor
	End   Anchor
}

// Valid reports whether both boundaries use the supported Anchor form.
func (anchors AnchorRange) Valid() bool {
	return anchors.Start.Valid() && anchors.End.Valid()
}

// AnchorRangeAt captures two relative boundaries under one RGA read lock.
// Unlike two separate AnchorAt calls, a concurrent document change cannot
// leave the pair referring to different revisions.
func (r *RGA) AnchorRangeAt(start, end int) (AnchorRange, error) {
	if r == nil {
		return AnchorRange{}, ErrNilText
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	startAnchor, err := r.anchorAtLocked(start)
	if err != nil {
		return AnchorRange{}, err
	}
	endAnchor, err := r.anchorAtLocked(end)
	if err != nil {
		return AnchorRange{}, err
	}
	return AnchorRange{Start: startAnchor, End: endAnchor}, nil
}

// ResolveAnchorRange resolves both boundaries from one RGA projection. The
// returned offsets retain the Start/End ordering captured by AnchorRangeAt.
// A compacted boundary fails closed with ErrAnchorGone.
func (r *RGA) ResolveAnchorRange(anchors AnchorRange) (start, end int, err error) {
	if r == nil {
		return 0, 0, ErrNilText
	}
	if !anchors.Valid() {
		return 0, 0, ErrInvalidAnchor
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	start, err = r.resolveAnchorLocked(anchors.Start)
	if err != nil {
		return 0, 0, err
	}
	end, err = r.resolveAnchorLocked(anchors.End)
	if err != nil {
		return 0, 0, err
	}
	return start, end, nil
}
