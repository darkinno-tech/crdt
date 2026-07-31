package history

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

type testExecutor struct {
	mu       sync.Mutex
	values   map[string][]string
	commands []string
}

func newTestExecutor() *testExecutor {
	return &testExecutor{values: make(map[string][]string)}
}

func (e *testExecutor) Execute(scope string, command []byte) (Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	parts := strings.SplitN(string(command), ":", 2)
	if len(parts) != 2 {
		return Result{}, errors.New("bad test command")
	}
	verb, value := parts[0], parts[1]
	switch verb {
	case "push":
		e.values[scope] = append(e.values[scope], value)
	case "pop":
		items := e.values[scope]
		if len(items) == 0 || items[len(items)-1] != value {
			return Result{}, errors.New("missing test value")
		}
		e.values[scope] = items[:len(items)-1]
	default:
		return Result{}, errors.New("bad test verb")
	}
	e.commands = append(e.commands, scope+"/"+string(command))
	other := "push"
	if verb == "push" {
		other = "pop"
	}
	return Result{Reverse: []byte(other + ":" + value), Emitted: []byte("delta/" + scope + "/" + string(command))}, nil
}

func TestManagerTracksUndoAcrossScopesAndRestores(t *testing.T) {
	executor := newTestExecutor()
	manager, err := NewManager(executor, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []struct {
		scope   string
		command string
	}{
		{"list/tasks", "push:draft"},
		{"tree/outline", "push:chapter"},
		{"richtext/body", "push:intro"},
	} {
		event, err := manager.Execute(operation.scope, []byte(operation.command))
		if err != nil {
			t.Fatal(err)
		}
		if got := string(event.Emitted); got != "delta/"+operation.scope+"/"+operation.command {
			t.Fatalf("emitted = %q", got)
		}
	}
	if event, err := manager.Undo(); err != nil || event.Scope != "richtext/body" || string(event.Command) != "pop:intro" {
		t.Fatalf("first undo = %#v, %v", event, err)
	}
	if event, err := manager.Undo(); err != nil || event.Scope != "tree/outline" || string(event.Command) != "pop:chapter" {
		t.Fatalf("second undo = %#v, %v", event, err)
	}
	if !manager.CanUndo() || !manager.CanRedo() || manager.Len() != 3 {
		t.Fatalf("history flags undo=%t redo=%t len=%d", manager.CanUndo(), manager.CanRedo(), manager.Len())
	}

	persisted, err := manager.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := NewManagerFromBinary(executor, DefaultOptions(), persisted)
	if err != nil {
		t.Fatal(err)
	}
	if event, err := restored.Redo(); err != nil || event.Scope != "tree/outline" || string(event.Command) != "push:chapter" {
		t.Fatalf("restored redo = %#v, %v", event, err)
	}
	if event, err := restored.Redo(); err != nil || event.Scope != "richtext/body" || string(event.Command) != "push:intro" {
		t.Fatalf("restored redo = %#v, %v", event, err)
	}
	if event, err := restored.Undo(); err != nil || event.Scope != "richtext/body" || string(event.Command) != "pop:intro" {
		t.Fatalf("restored undo = %#v, %v", event, err)
	}
	executor.mu.Lock()
	defer executor.mu.Unlock()
	if got := fmt.Sprint(executor.values["list/tasks"]); got != "[draft]" {
		t.Fatalf("list values = %s", got)
	}
	if got := fmt.Sprint(executor.values["tree/outline"]); got != "[chapter]" {
		t.Fatalf("tree values = %s", got)
	}
	if got := len(executor.values["richtext/body"]); got != 0 {
		t.Fatalf("rich text values = %d", got)
	}
}

func TestManagerFailureAndLimitsLeaveStackUnchanged(t *testing.T) {
	executor := newTestExecutor()
	manager, err := NewManager(executor, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Execute("list/tasks", []byte("pop:missing")); err == nil {
		t.Fatal("missing pop unexpectedly succeeded")
	}
	if manager.Len() != 0 || manager.CanUndo() || manager.CanRedo() {
		t.Fatalf("failed command changed stack: len=%d", manager.Len())
	}
	options := DefaultOptions()
	options.MaxEntries = 1
	limited, err := NewManager(executor, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := limited.Execute("list/tasks", []byte("push:one")); err != nil {
		t.Fatal(err)
	}
	if _, err := limited.Execute("list/tasks", []byte("push:two")); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("second entry error = %v", err)
	}
	if _, err := limited.Execute("bad scope", []byte("push:three")); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("bad scope error = %v", err)
	}
	if _, err := limited.Undo(); err != nil {
		t.Fatal(err)
	}
	if _, err := limited.Undo(); !errors.Is(err, ErrNoUndo) {
		t.Fatalf("empty undo = %v", err)
	}
	limited.Clear()
	if _, err := limited.Redo(); !errors.Is(err, ErrNoRedo) {
		t.Fatalf("redo after clear = %v", err)
	}
}

func TestManagerRejectsPanickingExecutor(t *testing.T) {
	manager, err := NewManager(ExecutorFunc(func(string, []byte) (Result, error) {
		panic("test panic")
	}), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Execute("text/body", []byte("push:value")); !errors.Is(err, ErrExecutor) {
		t.Fatalf("panic error = %v", err)
	}
	if manager.Len() != 0 {
		t.Fatalf("panicking executor changed history: %d", manager.Len())
	}
}

func TestManagerConcurrentExecution(t *testing.T) {
	executor := newTestExecutor()
	manager, err := NewManager(executor, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	const workers = 32
	var group sync.WaitGroup
	errors := make(chan error, workers)
	for index := 0; index < workers; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			_, err := manager.Execute("list/tasks", []byte(fmt.Sprintf("push:%02d", index)))
			errors <- err
		}(index)
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if manager.Len() != workers {
		t.Fatalf("entry count = %d", manager.Len())
	}
}

func FuzzManagerUnmarshal(f *testing.F) {
	executor := newTestExecutor()
	manager, err := NewManager(executor, DefaultOptions())
	if err != nil {
		f.Fatal(err)
	}
	if _, err := manager.Execute("list/tasks", []byte("push:seed")); err != nil {
		f.Fatal(err)
	}
	seed, err := manager.MarshalBinary()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte("bad"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = NewManagerFromBinary(executor, DefaultOptions(), data)
	})
}

func BenchmarkManagerExecuteUndo(b *testing.B) {
	executor := newTestExecutor()
	manager, err := NewManager(executor, DefaultOptions())
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		command := []byte(fmt.Sprintf("push:%d", index))
		if _, err := manager.Execute("list/tasks", command); err != nil {
			b.Fatal(err)
		}
		if _, err := manager.Undo(); err != nil {
			b.Fatal(err)
		}
	}
}
