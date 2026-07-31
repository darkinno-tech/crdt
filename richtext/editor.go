package richtext

import (
	"sort"

	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/text"
)

// EditorOperation is one Quill-style, offset-based operation in an editor
// transaction. Exactly one of Retain, Delete, or Insert must be present.
//
// Changes on a retained range update only the named attributes. A removed
// attribute is represented by AttributeChange{Key: key, Remove: true}.
// Inserted text may only receive live attributes; a removal has no meaningful
// value on a newly created position and is rejected. Offsets are Unicode
// scalar positions, not UTF-16 code units.
//
// This is deliberately a small common denominator for rich editors. It does
// not accept HTML, editor nodes, arbitrary embeds, or a renderer schema. An
// application must bind the exact allowed attribute schema in its manifest and
// validate it before constructing an EditorOperation.
type EditorOperation struct {
	Retain  int
	Delete  int
	Insert  string
	Changes []AttributeChange
}

// ApplyEditorDelta applies one complete editor transaction as one canonical
// rich-text frame. It preserves inserted inline attributes and retained-range
// formatting while preflighting the final text/mark resource usage before the
// document changes. An invalid or over-limit transaction never leaves a
// delete-only or partially formatted local document.
func (d *Document) ApplyEditorDelta(operations []EditorOperation) (Delta, error) {
	return d.ApplyEditorDeltaWithLimits(operations, frame.DefaultLimits())
}

// ApplyEditorDeltaWithLimits is ApplyEditorDelta with explicit framing limits.
// It is intended for browser and WebView adapters, which must keep local
// editor work within the same negotiated boundary as received frames.
func (d *Document) ApplyEditorDeltaWithLimits(operations []EditorOperation, limits frame.DecoderLimits) (Delta, error) {
	if d == nil || d.text == nil {
		return Delta{}, ErrNilDocument
	}
	prepared, err := canonicalEditorOperations(operations)
	if err != nil {
		return Delta{}, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	working, err := d.editorCloneLocked(limits)
	if err != nil {
		return Delta{}, err
	}
	textDelta, hasText, formatPlans, err := applyEditorOperations(working, prepared, limits)
	if err != nil {
		return Delta{}, err
	}
	formatPlans = expandEditorBlockPlans(working, formatPlans)

	// Reserve mark tags on the isolated text copy. Advancing the live HLC
	// before frame and mark preflight would make a rejected transaction change
	// the persisted clock even when its visible text and marks stay unchanged.
	// The real RGA witnesses the greatest preflighted tag only after every
	// fallible validation step succeeds.
	operationsWithTags := make([]formatOperation, 0, len(formatPlans))
	for _, plan := range formatPlans {
		if len(plan.targets) == 0 || len(plan.changes) == 0 {
			continue
		}
		tag, err := working.NextTag()
		if err != nil {
			return Delta{}, err
		}
		operationsWithTags = append(operationsWithTags, formatOperation{
			tag:     tag,
			targets: plan.targets,
			changes: plan.changes,
		})
	}

	delta := Delta{operations: operationsWithTags}
	if hasText {
		encoded, err := textDelta.MarshalRunBinaryWithLimits(limits)
		if err != nil {
			return Delta{}, err
		}
		delta.textDelta = encoded
	}
	if _, err := delta.MarshalBinaryWithLimits(limits); err != nil {
		return Delta{}, err
	}
	if err := d.preflightOperationsLocked(delta.operations); err != nil {
		return Delta{}, err
	}
	if hasText {
		if err := d.text.ApplyDelta(textDelta); err != nil {
			return Delta{}, err
		}
	}
	if tag, ok := greatestOperationTag(delta.operations); ok {
		if err := d.text.WitnessTag(tag); err != nil {
			return Delta{}, err
		}
	}
	d.applyOperationsLocked(delta.operations)
	return delta, nil
}

type editorOperation struct {
	retain  int
	delete  int
	insert  string
	changes []AttributeChange
}

type editorFormatPlan struct {
	targets []text.Position
	changes []AttributeChange
}

func canonicalEditorOperations(source []EditorOperation) ([]editorOperation, error) {
	operations := make([]editorOperation, 0, len(source))
	for _, operation := range source {
		kindCount := 0
		if operation.Retain > 0 {
			kindCount++
		}
		if operation.Delete > 0 {
			kindCount++
		}
		if operation.Insert != "" {
			kindCount++
		}
		if operation.Retain < 0 || operation.Delete < 0 || kindCount != 1 {
			return nil, ErrInvalidDelta
		}
		changes := canonicalChanges(operation.Changes)
		if err := validateChanges(changes); err != nil {
			return nil, err
		}
		if operation.Delete > 0 && len(changes) > 0 {
			return nil, ErrInvalidDelta
		}
		if operation.Insert != "" {
			for _, change := range changes {
				if change.Remove {
					return nil, ErrInvalidDelta
				}
			}
		}
		operations = append(operations, editorOperation{
			retain:  operation.Retain,
			delete:  operation.Delete,
			insert:  operation.Insert,
			changes: changes,
		})
	}
	return operations, nil
}

func (d *Document) editorCloneLocked(limits frame.DecoderLimits) (*text.RGA, error) {
	state, err := d.text.MarshalRunBinaryWithLimits(limits)
	if err != nil {
		return nil, err
	}
	working, err := text.NewFromClockWithOptions(d.text.ClockState(), d.options.Text)
	if err != nil {
		return nil, err
	}
	if err := working.UnmarshalRunBinaryWithLimits(state, limits); err != nil {
		return nil, err
	}
	return working, nil
}

func applyEditorOperations(working *text.RGA, operations []editorOperation, limits frame.DecoderLimits) (text.Delta, bool, []editorFormatPlan, error) {
	var combined text.Delta
	hasCombined := false
	plans := make([]editorFormatPlan, 0, len(operations))
	offset := 0
	for _, operation := range operations {
		switch {
		case operation.retain > 0:
			positions := working.Positions()
			if offset > len(positions) || operation.retain > len(positions)-offset {
				return text.Delta{}, false, nil, text.ErrRange
			}
			if len(operation.changes) > 0 {
				plans = append(plans, editorFormatPlan{
					targets: sortedEditorTargets(positions[offset : offset+operation.retain]),
					changes: operation.changes,
				})
			}
			offset += operation.retain
		case operation.delete > 0:
			change, _, err := working.PrepareDeleteRunBinaryWithLimits(offset, operation.delete, limits)
			if err != nil {
				return text.Delta{}, false, nil, err
			}
			if err := working.ApplyDelta(change); err != nil {
				return text.Delta{}, false, nil, err
			}
			if !hasCombined {
				combined, hasCombined = change, true
			} else if combined, err = combined.Merge(change); err != nil {
				return text.Delta{}, false, nil, err
			}
		case operation.insert != "":
			change, _, err := working.PrepareInsertRunBinaryWithLimits(offset, operation.insert, limits)
			if err != nil {
				return text.Delta{}, false, nil, err
			}
			if err := working.ApplyDelta(change); err != nil {
				return text.Delta{}, false, nil, err
			}
			if !hasCombined {
				combined, hasCombined = change, true
			} else if combined, err = combined.Merge(change); err != nil {
				return text.Delta{}, false, nil, err
			}
			inserted := change.NodePositions()
			if len(operation.changes) > 0 && len(inserted) > 0 {
				plans = append(plans, editorFormatPlan{targets: inserted, changes: operation.changes})
			}
			offset += len(inserted)
		}
	}
	return combined, hasCombined, plans, nil
}

func sortedEditorTargets(source []text.Position) []text.Position {
	targets := append([]text.Position(nil), source...)
	sort.Slice(targets, func(left, right int) bool { return targets[left].Compare(targets[right]) < 0 })
	return targets
}

// expandEditorBlockPlans translates the newline-local convention used by
// Quill-like editors into this protocol's explicit paragraph-wide rt.block
// markers. Inline marks retain exact-position semantics; only the reserved
// block attribute expands to every currently visible position in each touched
// newline-delimited paragraph. This preserves the existing block projection
// invariant instead of storing a misleading marker on only Quill's newline.
func expandEditorBlockPlans(working *text.RGA, source []editorFormatPlan) []editorFormatPlan {
	if len(source) == 0 {
		return nil
	}
	positions, runes := working.VisibleRunes()
	if len(positions) == 0 || len(runes) == 0 {
		return nil
	}
	byPosition := make(map[text.Position]int, len(positions))
	for index, position := range positions {
		byPosition[position] = index
	}
	plans := make([]editorFormatPlan, 0, len(source)*2)
	for _, plan := range source {
		inline, blocks := splitEditorBlockChanges(plan.changes)
		if len(inline) > 0 {
			plans = append(plans, editorFormatPlan{targets: plan.targets, changes: inline})
		}
		if len(blocks) == 0 {
			continue
		}
		targetIndexes := make(map[int]struct{})
		for _, target := range plan.targets {
			index, exists := byPosition[target]
			if !exists {
				continue
			}
			start, end := paragraphBounds(runes, index, index+1)
			for current := start; current < end; current++ {
				targetIndexes[current] = struct{}{}
			}
		}
		if len(targetIndexes) == 0 {
			continue
		}
		targets := make([]text.Position, 0, len(targetIndexes))
		for index := range targetIndexes {
			targets = append(targets, positions[index])
		}
		plans = append(plans, editorFormatPlan{targets: sortedEditorTargets(targets), changes: blocks})
	}
	return plans
}

func splitEditorBlockChanges(source []AttributeChange) (inline, blocks []AttributeChange) {
	for _, change := range source {
		if change.Key == AttributeBlock {
			blocks = append(blocks, change)
			continue
		}
		inline = append(inline, change)
	}
	return inline, blocks
}
