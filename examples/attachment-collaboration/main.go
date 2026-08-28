// Command attachment-collaboration demonstrates one document using separate,
// authenticated replication groups for editable text and external media
// references. It deliberately replicates metadata, not media bytes.
package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"

	"github.com/im10furry/crdt"
	"github.com/im10furry/crdt/attachment"
	frame "github.com/im10furry/crdt/encoding"
	"github.com/im10furry/crdt/replica"
	"github.com/im10furry/crdt/text"
)

var receiveLimits = frame.DecoderLimits{
	MaxFrameBytes:  4 << 10,
	MaxPayload:     3 << 10,
	MaxCodecID:     128,
	MaxElements:    128,
	MaxTags:        256,
	MaxStringBytes: 512,
}

var textLimits = text.Options{
	MaxNodes:        128,
	MaxTombstones:   128,
	MaxPendingNodes: 16,
	MaxPendingBytes: 1024,
}

var attachmentLimits = attachment.Options{
	MaxEntries:       16,
	MaxKeyBytes:      128,
	MaxObjectIDBytes: 256,
	MaxObjectBytes:   1 << 20,
}

type downloadedObject struct {
	key       string
	objectID  string
	mediaType string
	body      []byte
}

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(writer io.Writer) error {
	// Authenticate the peer and compare the exact manifest for each replication
	// group before accepting any frame.
	policy := crdt.ProtocolPolicy{}
	note, err := replicateNote(policy)
	if err != nil {
		return fmt.Errorf("replicate note: %w", err)
	}

	// These bodies stand in for already authorized object-store downloads. They
	// are never put in a CRDT delta, snapshot, log, or diagnostic output.
	objects := []downloadedObject{
		{key: "cover", objectID: "objects/inspection-42/cover.png", mediaType: "image/png", body: []byte("cover-image-v1")},
		{key: "acoustic-profile", objectID: "objects/inspection-42/acoustic-profile.ogg", mediaType: "audio/ogg", body: []byte("inspection-audio-v1")},
		{key: "walkthrough", objectID: "objects/inspection-42/walkthrough.mp4", mediaType: "video/mp4", body: []byte("inspection-video-v1")},
		{key: "measurement", objectID: "objects/inspection-42/measurement.bin", mediaType: "application/octet-stream", body: []byte("inspection-data-v1")},
	}
	refs, err := replicateAttachments(policy, objects)
	if err != nil {
		return fmt.Errorf("replicate attachments: %w", err)
	}
	for index, ref := range refs {
		if err := ref.Verify(bytes.NewReader(objects[index].body)); err != nil {
			return fmt.Errorf("verify downloaded object %s: %w", objects[index].key, err)
		}
	}

	if _, err := fmt.Fprintf(writer, "note=%s\nattachments=%d\n", note, len(refs)); err != nil {
		return fmt.Errorf("write collaboration result: %w", err)
	}
	for _, ref := range refs {
		if _, err := fmt.Fprintf(writer, "%s=%d\n", ref.MediaType, ref.Size); err != nil {
			return fmt.Errorf("write collaboration result: %w", err)
		}
	}
	if _, err := fmt.Fprintln(writer, "verified=true"); err != nil {
		return fmt.Errorf("write collaboration result: %w", err)
	}
	return nil
}

func replicateNote(policy crdt.ProtocolPolicy) (string, error) {
	if !policy.SupportsFrame(crdt.TypeIDRGADelta) {
		return "", fmt.Errorf("RGA is not enabled by the replication policy")
	}
	manifest, err := replica.NewManifest("inspection-42/text", "example.com/inspection-note/v1", 1, replica.Protocol{
		StateID:          crdt.TypeIDRGAState,
		DeltaID:          crdt.TypeIDRGADelta,
		SemanticsVersion: 1,
	}, policy)
	if err != nil {
		return "", err
	}
	author, err := text.NewWithOptions("inspector", textLimits)
	if err != nil {
		return "", err
	}
	dashboard, err := text.NewWithOptions("dashboard", textLimits)
	if err != nil {
		return "", err
	}
	delta, err := author.Insert(0, "Inspect acoustic profile")
	if err != nil {
		return "", err
	}
	encoded, err := delta.MarshalBinary()
	if err != nil {
		return "", err
	}
	change, err := replica.NewChangeWithPolicy(manifest, replica.Dot{Actor: "inspector", Counter: 1}, encoded, policy)
	if err != nil {
		return "", err
	}
	inbox, err := newInbox(manifest, func(frameBytes []byte) error {
		received, err := text.UnmarshalRGADeltaWithLimits(frameBytes, receiveLimits)
		if err != nil {
			return err
		}
		return dashboard.ApplyDelta(received)
	}, policy)
	if err != nil {
		return "", err
	}
	delivery, err := inbox.Receive(change)
	if err != nil {
		return "", fmt.Errorf("deliver text change: %w", err)
	}
	if delivery.Buffered || len(delivery.Applied) != 1 {
		return "", fmt.Errorf("unexpected text delivery result")
	}
	saved, err := dashboard.SnapshotCurrentState()
	if err != nil {
		return "", err
	}
	recovered, err := text.NewFromSnapshot(saved)
	if err != nil {
		return "", err
	}
	return recovered.String(), nil
}

func replicateAttachments(policy crdt.ProtocolPolicy, objects []downloadedObject) ([]attachment.Reference, error) {
	if !policy.SupportsFrame(crdt.TypeIDLWWMapDelta) {
		return nil, fmt.Errorf("LWW-Map is not enabled by the replication policy")
	}
	manifest, err := replica.NewManifest("inspection-42/attachments", "github.com/im10furry/crdt/attachment-reference/v1", 1, replica.Protocol{
		StateID:          crdt.TypeIDLWWMapState,
		DeltaID:          crdt.TypeIDLWWMapDelta,
		SemanticsVersion: attachment.SemanticsVersion,
	}, policy)
	if err != nil {
		return nil, err
	}
	recorder, err := attachment.NewWithOptions("recorder", attachmentLimits)
	if err != nil {
		return nil, err
	}
	dashboard, err := attachment.NewWithOptions("dashboard", attachmentLimits)
	if err != nil {
		return nil, err
	}
	inbox, err := newInbox(manifest, func(frameBytes []byte) error {
		received, err := attachment.UnmarshalDeltaWithLimits(frameBytes, receiveLimits, attachmentLimits)
		if err != nil {
			return err
		}
		return dashboard.ApplyDelta(received)
	}, policy)
	if err != nil {
		return nil, err
	}
	for index, object := range objects {
		digest := sha256.Sum256(object.body)
		delta, err := recorder.Put(object.key, attachment.Reference{
			ObjectID:  object.objectID,
			MediaType: object.mediaType,
			Size:      uint64(len(object.body)),
			Digest:    digest,
		})
		if err != nil {
			return nil, err
		}
		encoded, err := delta.MarshalBinary()
		if err != nil {
			return nil, err
		}
		change, err := replica.NewChangeWithPolicy(manifest, replica.Dot{Actor: "recorder", Counter: uint64(index + 1)}, encoded, policy)
		if err != nil {
			return nil, err
		}
		delivery, err := inbox.Receive(change)
		if err != nil {
			return nil, fmt.Errorf("deliver attachment change: %w", err)
		}
		if delivery.Buffered || len(delivery.Applied) != 1 {
			return nil, fmt.Errorf("unexpected attachment delivery result")
		}
	}
	saved, err := dashboard.SnapshotCurrentState()
	if err != nil {
		return nil, err
	}
	recovered, err := attachment.NewFromSnapshotWithOptions(saved, attachmentLimits)
	if err != nil {
		return nil, err
	}
	refs := make([]attachment.Reference, 0, len(objects))
	for _, object := range objects {
		ref, ok := recovered.Get(object.key)
		if !ok {
			return nil, fmt.Errorf("attachment %s missing after recovery", object.key)
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

func newInbox(manifest replica.Manifest, apply func([]byte) error, policy crdt.ProtocolPolicy) (*replica.Inbox, error) {
	frontier, err := replica.NewFrontier(nil)
	if err != nil {
		return nil, err
	}
	return replica.NewInboxWithPolicy(manifest, frontier, 4, 4*receiveLimits.MaxFrameBytes, apply, policy)
}
