// Command shared-document demonstrates the high-level shared.Document facade.
// It remains a local teaching flow: a real receiver authenticates and
// authorizes the peer, limits the transport body, and durably records its
// outbox/checkpoint before calling ApplyUpdate.
package main

import (
	"fmt"
	"io"
	"os"

	frame "github.com/im10furry/crdt/encoding"
	"github.com/im10furry/crdt/shared"
)

var receiveLimits = frame.DecoderLimits{
	MaxFrameBytes:  8 << 10,
	MaxPayload:     7 << 10,
	MaxCodecID:     128,
	MaxElements:    128,
	MaxTags:        128,
	MaxStringBytes: 512,
}

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(writer io.Writer) error {
	writerDocument, err := shared.NewWithLimits("editor-a", receiveLimits)
	if err != nil {
		return fmt.Errorf("create writer document: %w", err)
	}
	readerDocument, err := shared.NewWithLimits("editor-b", receiveLimits)
	if err != nil {
		return fmt.Errorf("create reader document: %w", err)
	}

	// An application normally appends these frames to an authenticated durable
	// outbox. The facade gives the caller owned canonical frames directly.
	updates := make([][]byte, 0, 8)
	if _, err := writerDocument.OnUpdate(func(update []byte) {
		updates = append(updates, update)
	}); err != nil {
		return fmt.Errorf("subscribe writer updates: %w", err)
	}

	board, err := writerDocument.Map("board")
	if err != nil {
		return fmt.Errorf("open board: %w", err)
	}
	if err := board.SetString("title", "Release plan"); err != nil {
		return fmt.Errorf("set title: %w", err)
	}
	tasks, err := board.CreateArray("tasks")
	if err != nil {
		return fmt.Errorf("create tasks: %w", err)
	}
	task, err := tasks.InsertMap(0)
	if err != nil {
		return fmt.Errorf("insert task: %w", err)
	}
	if err := task.SetJSON("task", map[string]any{"id": "release-notes", "done": false}); err != nil {
		return fmt.Errorf("set task: %w", err)
	}

	// A CRDT receiver accepts a retry and dependencies before their parents.
	// Authentication and authorization happen before this bounded decode call.
	for index := len(updates) - 1; index >= 0; index-- {
		if err := readerDocument.ApplyUpdate(updates[index]); err != nil {
			return fmt.Errorf("apply update %d: %w", index, err)
		}
		if index == 0 { // Simulate an at-least-once retry.
			if err := readerDocument.ApplyUpdate(updates[index]); err != nil {
				return fmt.Errorf("retry update %d: %w", index, err)
			}
		}
	}

	readerBoard, ok := readerDocument.LookupMap("board")
	if !ok {
		return fmt.Errorf("received board is missing")
	}
	title, ok := readerBoard.String("title")
	if !ok {
		return fmt.Errorf("read received title")
	}
	readerTasks, ok := readerBoard.Array("tasks")
	if !ok {
		return fmt.Errorf("read received tasks")
	}
	readerTask, ok := readerTasks.Map(0)
	if !ok {
		return fmt.Errorf("read received task")
	}
	var payload struct {
		ID   string `json:"id"`
		Done bool   `json:"done"`
	}
	if found, err := readerTask.JSON("task", &payload); err != nil || !found {
		return fmt.Errorf("decode received task: %w", err)
	}
	_, err = fmt.Fprintf(writer, "title=%s\ntask=%s\ndone=%t\nupdates=%d\n", title, payload.ID, payload.Done, len(updates))
	return err
}
