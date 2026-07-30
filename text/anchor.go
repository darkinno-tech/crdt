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

// Anchor is stable local cursor or selection metadata for an RGA boundary.
// It is deliberately not a CRDT state field and has no framed wire format:
// applications may transport it as ephemeral presence only after authenticating
// their peer and group. A retained tombstone continues to resolve; exact
// tombstone compaction invalidates an anchor rather than relocating it.
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
