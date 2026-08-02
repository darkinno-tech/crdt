package richtext

import (
	"errors"
	"sort"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/text"
)

// TombstoneTags returns every retained text or attribute-removal tombstone in
// canonical order. It is an exact-acknowledgement input, not proof that a tag
// is safe to collect. Before calling either compactor, an application must
// authenticate exact acknowledgements for one membership epoch, persist the
// post-compaction snapshot, and retire all old-epoch frames.
func (d *Document) TombstoneTags() []crdt.Tag {
	if d == nil || d.text == nil {
		return nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.tombstoneTagsLocked()
}

// CompactTombstones removes an exact, structurally-safe text tombstone batch
// and any selected attribute-removal tombstones. For replicated state, callers
// must establish exact acknowledgement before calling it. tombstonegc.
// SimpleCollector may call it only for its documented local-only lifecycle. It
// is all-or-nothing for text structure: an unresolved, unknown, or non-leaf
// text tombstone returns ErrUnsafeCompaction without changing text or
// formatting metadata. Formatting attached to a text position is removed only
// when that position was retained before and is no longer retained after
// successful RGA compaction.
func (d *Document) CompactTombstones(tags []crdt.Tag) (int, error) {
	return d.compactTombstones(tags, false)
}

// CompactEligibleTombstones makes best-effort progress through an
// exact-acknowledged tombstone batch. Deleted text descendants are removed
// before their deleted ancestors; a non-leaf text tombstone cannot prevent
// independent attribute-removal tombstones or structurally safe descendants
// from compacting. It remains fail-closed while the nested RGA has pending
// dependencies. tombstonegc.SimpleCollector may call it only for its
// documented local-only lifecycle.
func (d *Document) CompactEligibleTombstones(tags []crdt.Tag) (int, error) {
	return d.compactTombstones(tags, true)
}

func (d *Document) compactTombstones(tags []crdt.Tag, eligible bool) (int, error) {
	if d == nil || d.text == nil {
		return 0, ErrNilDocument
	}
	selected, err := compactionTagSet(tags)
	if err != nil {
		return 0, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	before := d.tombstoneTagsLocked()
	retainedBefore := d.retainedMarkPositionsLocked()
	if eligible {
		if _, err := d.text.CompactEligibleTombstones(tags); err != nil {
			return 0, richCompactionError(err)
		}
	} else if _, err := d.text.CompactTombstones(tags); err != nil {
		return 0, richCompactionError(err)
	}
	d.compactMarksLocked(selected, retainedBefore)
	return removedTombstoneTagCount(before, d.tombstoneTagsLocked(), selected), nil
}

func richCompactionError(err error) error {
	if errors.Is(err, text.ErrUnsafeCompaction) {
		return ErrUnsafeCompaction
	}
	return err
}

func compactionTagSet(tags []crdt.Tag) (map[crdt.Tag]struct{}, error) {
	selected := make(map[crdt.Tag]struct{}, len(tags))
	for _, tag := range tags {
		if !tag.Valid() {
			return nil, ErrUnsafeCompaction
		}
		selected[tag] = struct{}{}
	}
	return selected, nil
}

func (d *Document) tombstoneTagsLocked() []crdt.Tag {
	all := make(map[crdt.Tag]struct{})
	for _, tag := range d.text.TombstoneTags() {
		all[tag] = struct{}{}
	}
	for _, marks := range d.marks {
		marks.rangeValues(func(_ string, value markValue) {
			if value.deleted {
				all[value.tag] = struct{}{}
			}
		})
	}
	result := make([]crdt.Tag, 0, len(all))
	for tag := range all {
		result = append(result, tag)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Compare(result[j]) < 0 })
	return result
}

func (d *Document) retainedMarkPositionsLocked() map[text.Position]bool {
	retained := make(map[text.Position]bool, len(d.marks))
	for position := range d.marks {
		retained[position] = d.text.RetainsPosition(position)
	}
	return retained
}

func (d *Document) compactMarksLocked(selected map[crdt.Tag]struct{}, retainedBefore map[text.Position]bool) {
	for position, marks := range d.marks {
		if retainedBefore[position] && !d.text.RetainsPosition(position) {
			d.markCount -= marks.len()
			delete(d.marks, position)
			continue
		}
		keys := make([]string, 0, marks.len())
		marks.rangeValues(func(key string, value markValue) {
			if value.deleted {
				if _, ok := selected[value.tag]; ok {
					keys = append(keys, key)
				}
			}
		})
		for _, key := range keys {
			if marks.remove(key) {
				d.markCount--
			}
		}
		if marks.len() == 0 {
			delete(d.marks, position)
			continue
		}
		d.marks[position] = marks
	}
}

func (s *markSet) remove(key string) bool {
	if s.key == key {
		if len(s.extra) == 0 {
			*s = markSet{}
			return true
		}
		nextKey := ""
		for candidate := range s.extra {
			if nextKey == "" || candidate < nextKey {
				nextKey = candidate
			}
		}
		s.key, s.value = nextKey, s.extra[nextKey]
		delete(s.extra, nextKey)
		if len(s.extra) == 0 {
			s.extra = nil
		}
		return true
	}
	if _, exists := s.extra[key]; !exists {
		return false
	}
	delete(s.extra, key)
	if len(s.extra) == 0 {
		s.extra = nil
	}
	return true
}

func removedTombstoneTagCount(before, after []crdt.Tag, selected map[crdt.Tag]struct{}) int {
	retained := make(map[crdt.Tag]struct{}, len(after))
	for _, tag := range after {
		retained[tag] = struct{}{}
	}
	removed := 0
	for _, tag := range before {
		if _, selected := selected[tag]; !selected {
			continue
		}
		if _, stillRetained := retained[tag]; !stillRetained {
			removed++
		}
	}
	return removed
}
