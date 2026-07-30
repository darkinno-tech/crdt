// persistent-replica demonstrates a local CRDT checkpoint that survives a
// process restart. The temporary directory is only for an executable example;
// a host application must choose a protected durable volume and backup policy.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/DarkInno/crdt/persistence"
	"github.com/DarkInno/crdt/set"
)

type taskCodec struct{}

func (taskCodec) ID() string                            { return "example.com/maintenance-task/v1" }
func (taskCodec) Marshal(value string) ([]byte, error)  { return []byte(value), nil }
func (taskCodec) Unmarshal(data []byte) (string, error) { return string(data), nil }

func main() {
	if err := run(os.Stdout); err != nil {
		panic(err)
	}
}

func run(writer io.Writer) error {
	directory, err := os.MkdirTemp("", "crdt-persistence-example-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(directory) }()

	store, err := openStore(filepath.Join(directory, "replica.db"))
	if err != nil {
		return err
	}
	tasks, err := set.NewORSet("maintenance", taskCodec{})
	if err != nil {
		return err
	}
	if _, err := tasks.Add("inspect-filter"); err != nil {
		return err
	}
	saved, err := tasks.SnapshotCurrentState()
	if err != nil {
		return err
	}
	if err := store.Save("maintenance", persistence.Checkpoint{
		Snapshot: saved,
		Cursor:   41,
		Outbox:   []byte("canonical-pending-change"),
	}); err != nil {
		return err
	}
	if err := store.Close(); err != nil {
		return err
	}

	store, err = openStore(filepath.Join(directory, "replica.db"))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	checkpoint, found, err := store.Load("maintenance")
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("checkpoint not found")
	}
	restored, err := set.NewORSetFromSnapshot(checkpoint.Snapshot, taskCodec{})
	if err != nil {
		return err
	}
	if _, err := restored.Add("replace-filter"); err != nil {
		return err
	}
	_, err = fmt.Fprintf(writer, "recovered=%t cursor=%d outbox_bytes=%d\n", restored.Contains("inspect-filter") && restored.Contains("replace-filter"), checkpoint.Cursor, len(checkpoint.Outbox))
	return err
}

func openStore(path string) (*persistence.Store, error) {
	return persistence.Open(path, persistence.Config{
		MaxRecordBytes:     1 << 20,
		MaxStateBytes:      512 << 10,
		MaxFrontierEntries: 4 << 10,
		MaxReplicaIDBytes:  256,
		MaxOutboxBytes:     64 << 10,
		MaxNameBytes:       128,
		Validate: func(data []byte) error {
			candidate, err := set.NewORSet("validation", taskCodec{})
			if err != nil {
				return err
			}
			return candidate.UnmarshalBinary(data)
		},
	})
}
