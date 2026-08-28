package richtext

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	frame "github.com/im10furry/crdt/encoding"
	"github.com/im10furry/crdt/text"
)

const (
	// SemanticSchemaID identifies the optional, application-negotiated schema
	// implemented by this adapter. It does not alter rich-text v1 framing.
	SemanticSchemaID = "github.com/im10furry/crdt/richtext/semantic/v1"

	// ObjectReplacementCharacter occupies exactly one RGA position for an
	// embedded application object. Renderers must not treat its JSON as markup.
	ObjectReplacementCharacter = "\uFFFC"

	AttributeBold      = "rt.bold"
	AttributeItalic    = "rt.italic"
	AttributeEmbedKind = "rt.embed.kind"
	AttributeEmbedData = "rt.embed.data"
	AttributeBlock     = "rt.block"

	maxSemanticEmbedBytes = 64 << 10
	maxSemanticKindBytes  = 128
)

var ErrInvalidSemantic = errors.New("richtext: invalid semantic formatting")

// Embed is one application-owned, non-executable embedded object. Data is a
// bounded JSON object so schema validators can inspect fields before rendering
// it; this package never interprets it as HTML, CSS, a URL, or executable code.
type Embed struct {
	Kind string
	Data string
}

// BlockFormat is a paragraph-level presentation marker. A block format is
// applied to every existing rune in touched paragraphs so its CRDT lifetime
// follows the existing exact-position/LWW model. Editors must explicitly pass
// block attributes when inserting new text; no hidden inheritance is applied.
type BlockFormat struct {
	Kind  string
	Level int
}

// Block is one newline-delimited presentation block. Formatted is true only
// when every current position in the block carries the same valid rt.block
// value. Concurrent conflicting block edits therefore remain observable
// instead of being silently presented as one arbitrary block type.
type Block struct {
	Text      string
	Format    BlockFormat
	Formatted bool
}

// SetBold applies or removes the semantic bold mark for an exact rune range.
func (d *Document) SetBold(offset, count int, enabled bool) (Delta, error) {
	return d.setBoolean(offset, count, AttributeBold, enabled)
}

// SetItalic applies or removes the semantic italic mark for an exact rune range.
func (d *Document) SetItalic(offset, count int, enabled bool) (Delta, error) {
	return d.setBoolean(offset, count, AttributeItalic, enabled)
}

func (d *Document) setBoolean(offset, count int, key string, enabled bool) (Delta, error) {
	change := AttributeChange{Key: key, Value: "true"}
	if !enabled {
		change.Value, change.Remove = "", true
	}
	return d.Format(offset, count, []AttributeChange{change})
}

// InsertEmbed inserts a single object-replacement character with bounded,
// validated semantic metadata. Applications still authorize Kind and validate
// individual JSON fields according to their authenticated manifest schema.
func (d *Document) InsertEmbed(offset int, embed Embed) (Delta, error) {
	if !embed.valid() {
		return Delta{}, ErrInvalidSemantic
	}
	return d.InsertWithAttributes(offset, ObjectReplacementCharacter, Attributes{
		AttributeEmbedKind: embed.Kind,
		AttributeEmbedData: embed.Data,
	})
}

// InsertWithBlockFormat explicitly assigns a block marker to newly inserted
// text. It is the opt-in counterpart to the deliberately absent implicit
// block inheritance rule.
func (d *Document) InsertWithBlockFormat(offset int, value string, attributes Attributes, format BlockFormat) (Delta, error) {
	if !format.valid() {
		return Delta{}, ErrInvalidSemantic
	}
	if _, exists := attributes[AttributeBlock]; exists {
		return Delta{}, ErrInvalidSemantic
	}
	withBlock := make(Attributes, len(attributes)+1)
	for key, attribute := range attributes {
		withBlock[key] = attribute
	}
	withBlock[AttributeBlock] = format.value()
	return d.InsertWithAttributes(offset, value, withBlock)
}

// EmbedAt returns the semantic embed at offset. It rejects malformed generic
// attributes rather than exposing them as a trusted object.
func (d *Document) EmbedAt(offset int) (Embed, bool) {
	if d == nil || d.text == nil || offset < 0 {
		return Embed{}, false
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	positions, runes := d.text.VisibleRunes()
	if offset >= len(runes) || runes[offset] != '\uFFFC' {
		return Embed{}, false
	}
	attributes := d.attributesForPositionLocked(positions[offset])
	embed := Embed{Kind: attributes[AttributeEmbedKind], Data: attributes[AttributeEmbedData]}
	return embed, embed.valid()
}

// FormatBlocks expands the selection to complete touched paragraphs, then
// records a validated block marker on their current positions. A collapsed
// selection formats its current paragraph. Paragraphs are separated by '\n';
// the end boundary is exclusive unless it falls inside a paragraph. This keeps
// the feature within v1's exact-position semantics.
func (d *Document) FormatBlocks(offset, count int, format BlockFormat) (Delta, error) {
	if !format.valid() {
		return Delta{}, ErrInvalidSemantic
	}
	return d.formatBlocks(offset, count, AttributeChange{Key: AttributeBlock, Value: format.value()})
}

// ClearBlocks records LWW removals for block markers on complete touched
// paragraphs. It does not remove text or infer a replacement block format.
func (d *Document) ClearBlocks(offset, count int) (Delta, error) {
	return d.formatBlocks(offset, count, AttributeChange{Key: AttributeBlock, Remove: true})
}

// FormatBlocksAnchored formats complete paragraphs selected by two existing
// text anchors. Both anchors are resolved under the document lock, so a
// concurrent insertion cannot change the intended selection between resolve
// and mutation.
func (d *Document) FormatBlocksAnchored(start, end text.Anchor, format BlockFormat) (Delta, error) {
	if !format.valid() {
		return Delta{}, ErrInvalidSemantic
	}
	return d.formatBlocksAnchored(start, end, AttributeChange{Key: AttributeBlock, Value: format.value()})
}

// ClearBlocksAnchored removes block markers from complete paragraphs selected
// by two existing text anchors.
func (d *Document) ClearBlocksAnchored(start, end text.Anchor) (Delta, error) {
	return d.formatBlocksAnchored(start, end, AttributeChange{Key: AttributeBlock, Remove: true})
}

func (d *Document) formatBlocks(offset, count int, change AttributeChange) (Delta, error) {
	if d == nil || d.text == nil {
		return Delta{}, ErrNilDocument
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	positions, runes := d.text.VisibleRunes()
	return d.formatBlocksLocked(positions, runes, offset, count, change)
}

func (d *Document) formatBlocksAnchored(start, end text.Anchor, change AttributeChange) (Delta, error) {
	if d == nil || d.text == nil {
		return Delta{}, ErrNilDocument
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	startOffset, err := d.text.ResolveAnchor(start)
	if err != nil {
		return Delta{}, err
	}
	endOffset, err := d.text.ResolveAnchor(end)
	if err != nil {
		return Delta{}, err
	}
	if startOffset > endOffset {
		return Delta{}, text.ErrRange
	}
	positions, runes := d.text.VisibleRunes()
	return d.formatBlocksLocked(positions, runes, startOffset, endOffset-startOffset, change)
}

func (d *Document) formatBlocksLocked(positions []text.Position, runes []rune, offset, count int, change AttributeChange) (Delta, error) {
	if offset < 0 || count < 0 || offset > len(runes) || count > len(runes)-offset {
		return Delta{}, ErrInvalidSemantic
	}
	if len(runes) == 0 {
		return d.formatPositionsLocked(nil, nil, frame.DefaultLimits())
	}
	start, end := paragraphBounds(runes, offset, offset+count)
	return d.formatPositionsLocked(positions[start:end], []AttributeChange{change}, frame.DefaultLimits())
}

// BlockFormatAt reports a valid semantic block marker at a visible offset.
func (d *Document) BlockFormatAt(offset int) (BlockFormat, bool) {
	attributes, ok := d.AttributesAt(offset)
	if !ok {
		return BlockFormat{}, false
	}
	format, ok := parseBlockFormat(attributes[AttributeBlock])
	return format, ok && format.valid()
}

// Blocks returns a presentation projection of newline-delimited blocks. A
// trailing newline terminates the preceding block and does not manufacture an
// unanchored empty block after it.
func (d *Document) Blocks() []Block {
	if d == nil || d.text == nil {
		return nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	positions, runes := d.text.VisibleRunes()
	if len(runes) == 0 {
		return nil
	}
	blocks := make([]Block, 0, 1)
	start := 0
	for index, runeValue := range runes {
		if runeValue != '\n' && index+1 != len(runes) {
			continue
		}
		contentEnd, blockEnd := index+1, index+1
		if runeValue == '\n' {
			contentEnd = index
		}
		block := Block{Text: string(runes[start:contentEnd])}
		block.Format, block.Formatted = d.blockFormatForPositionsLocked(positions[start:blockEnd])
		blocks = append(blocks, block)
		start = index + 1
	}
	return blocks
}

func (d *Document) blockFormatForPositionsLocked(positions []text.Position) (BlockFormat, bool) {
	if len(positions) == 0 {
		return BlockFormat{}, false
	}
	value := d.blockValueForPositionLocked(positions[0])
	format, ok := parseBlockFormat(value)
	if !ok || !format.valid() {
		return BlockFormat{}, false
	}
	for _, position := range positions[1:] {
		if d.blockValueForPositionLocked(position) != value {
			return BlockFormat{}, false
		}
	}
	return format, true
}

func (d *Document) blockValueForPositionLocked(position text.Position) string {
	entries, exists := d.marks[position]
	if !exists {
		return ""
	}
	value, exists := entries.get(AttributeBlock)
	if !exists || value.deleted {
		return ""
	}
	return value.value
}

func (embed Embed) valid() bool {
	return validSemanticKind(embed.Kind) && len(embed.Data) <= maxSemanticEmbedBytes && json.Valid([]byte(embed.Data)) &&
		jsonObject(embed.Data)
}

func (format BlockFormat) valid() bool {
	switch format.Kind {
	case "paragraph", "quote", "code", "list-item":
		return format.Level == 0
	case "heading":
		return format.Level >= 1 && format.Level <= 6
	default:
		return false
	}
}

func (format BlockFormat) value() string {
	if format.Level == 0 {
		return format.Kind
	}
	return format.Kind + ":" + blockLevel(format.Level)
}

func parseBlockFormat(value string) (BlockFormat, bool) {
	parts := strings.Split(value, ":")
	switch len(parts) {
	case 1:
		return BlockFormat{Kind: parts[0]}, true
	case 2:
		if parts[0] != "heading" || len(parts[1]) != 1 || parts[1][0] < '1' || parts[1][0] > '6' {
			return BlockFormat{}, false
		}
		return BlockFormat{Kind: parts[0], Level: int(parts[1][0] - '0')}, true
	default:
		return BlockFormat{}, false
	}
}

func validSemanticKind(value string) bool {
	if value == "" || len(value) > maxSemanticKindBytes || !utf8.ValidString(value) {
		return false
	}
	for _, runeValue := range value {
		if (runeValue < 'a' || runeValue > 'z') && (runeValue < '0' || runeValue > '9') && runeValue != '.' && runeValue != '-' && runeValue != '_' {
			return false
		}
	}
	return true
}

func jsonObject(value string) bool {
	decoder := json.NewDecoder(strings.NewReader(value))
	var object map[string]json.RawMessage
	return decoder.Decode(&object) == nil && object != nil
}

func blockLevel(level int) string { return strconv.Itoa(level) }

func paragraphBounds(runes []rune, start, end int) (int, int) {
	collapsed := start == end
	if collapsed && start == len(runes) && start > 0 {
		start--
	}
	for start > 0 && runes[start-1] != '\n' {
		start--
	}
	if !collapsed && end > 0 && runes[end-1] == '\n' {
		return start, end
	}
	for end < len(runes) && runes[end] != '\n' {
		end++
	}
	if end < len(runes) {
		end++
	}
	return start, end
}
