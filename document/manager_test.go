package document

import (
	"errors"
	"reflect"
	"testing"
	"unicode/utf8"

	"github.com/DarkInno/crdt/list"
)

type stringCodec struct{}

func (stringCodec) ID() string { return "example.com/document-string/v1" }

func (stringCodec) Marshal(value string) ([]byte, error) {
	if !utf8.ValidString(value) {
		return nil, errors.New("invalid UTF-8")
	}
	return []byte(value), nil
}

func (stringCodec) Unmarshal(value []byte) (string, error) {
	if !utf8.Valid(value) {
		return "", errors.New("invalid UTF-8")
	}
	return string(value), nil
}

func TestDocManagerRoutesMoveDeltasWithoutCreatingRemoteDocuments(t *testing.T) {
	left := mustManager(t)
	right := mustManager(t)
	for _, manager := range []*DocManager[string]{left, right} {
		for _, id := range []string{"backlog", "roadmap"} {
			if _, err := manager.CreateDocument(id, "replica-"+id); err != nil {
				t.Fatal(err)
			}
		}
	}

	seed, err := left.Insert("roadmap", 0, []string{"draft", "review", "publish"})
	if err != nil {
		t.Fatal(err)
	}
	if err := right.ApplyDelta("roadmap", seed); err != nil {
		t.Fatal(err)
	}
	move, err := left.Move("roadmap", 2, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := right.ApplyDelta("roadmap", move); err != nil {
		t.Fatal(err)
	}

	for _, manager := range []*DocManager[string]{left, right} {
		roadmap, ok := manager.Document("roadmap")
		if !ok {
			t.Fatal("roadmap missing")
		}
		if got, err := roadmap.Values(); err != nil || !reflect.DeepEqual(got, []string{"publish", "draft", "review"}) {
			t.Fatalf("roadmap values = %q, %v", got, err)
		}
		backlog, ok := manager.Document("backlog")
		if !ok {
			t.Fatal("backlog missing")
		}
		if got, err := backlog.Values(); err != nil || len(got) != 0 {
			t.Fatalf("backlog values = %q, %v", got, err)
		}
	}

	if err := right.ApplyDelta("unknown", move); !errors.Is(err, ErrDocumentNotFound) {
		t.Fatalf("unknown document apply = %v", err)
	}
	if right.Len() != 2 {
		t.Fatalf("remote apply created a document: %d", right.Len())
	}
	if got, want := right.DocumentIDs(), []string{"backlog", "roadmap"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DocumentIDs() = %q, want %q", got, want)
	}
}

func TestDocManagerBoundsAndIdentifiers(t *testing.T) {
	if _, err := NewDocManager(stringCodec{}, Options{}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("invalid options = %v", err)
	}
	if _, err := NewDocManager[string](nil, DefaultOptions()); !errors.Is(err, list.ErrInvalidCodec) {
		t.Fatalf("nil codec = %v", err)
	}
	invalidListOptions := DefaultOptions()
	invalidListOptions.ListOptions.MaxNodes = 0
	if _, err := NewDocManager(stringCodec{}, invalidListOptions); !errors.Is(err, list.ErrResourceLimit) {
		t.Fatalf("invalid list options = %v", err)
	}
	options := DefaultOptions()
	options.MaxDocuments = 1
	options.MaxDocumentIDBytes = 8
	manager, err := NewDocManager(stringCodec{}, options)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", " lead", "trail ", "line\nbreak", "too-long-id"} {
		if _, err := manager.CreateDocument(id, "writer"); !errors.Is(err, ErrInvalidDocumentID) {
			t.Fatalf("CreateDocument(%q) = %v, want invalid ID", id, err)
		}
	}
	if _, err := manager.CreateDocument("one", "writer"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CreateDocument("one", "writer"); !errors.Is(err, ErrDocumentExists) {
		t.Fatalf("duplicate document = %v", err)
	}
	if _, err := manager.CreateDocument("two", "writer"); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("document limit = %v", err)
	}
	if _, err := manager.Insert("missing", 0, []string{"x"}); !errors.Is(err, ErrDocumentNotFound) {
		t.Fatalf("missing insert = %v", err)
	}

	var nilManager *DocManager[string]
	if _, err := nilManager.CreateDocument("one", "writer"); !errors.Is(err, ErrNilManager) {
		t.Fatalf("nil create = %v", err)
	}
	if err := nilManager.ApplyDelta("one", list.MoveDelta{}); !errors.Is(err, ErrNilManager) {
		t.Fatalf("nil apply = %v", err)
	}
}

func mustManager(t testing.TB) *DocManager[string] {
	t.Helper()
	manager, err := NewDocManager(stringCodec{}, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	return manager
}
