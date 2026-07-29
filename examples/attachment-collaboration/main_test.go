package main

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/attachment"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestRun(t *testing.T) {
	var output bytes.Buffer
	if err := run(&output); err != nil {
		t.Fatal(err)
	}
	const want = "note=Inspect acoustic profile\nattachments=4\nimage/png=14\naudio/ogg=19\nvideo/mp4=19\napplication/octet-stream=18\nverified=true\n"
	if got := output.String(); got != want {
		t.Fatalf("run() output = %q, want %q", got, want)
	}
}

func TestRunReturnsWriteFailure(t *testing.T) {
	if err := run(failingWriter{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("run() error = %v, want wrapped %v", err, io.ErrClosedPipe)
	}
}

func TestAttachmentFlowRejectsTamperedDownload(t *testing.T) {
	objects := []downloadedObject{{key: "audio", objectID: "objects/audio.ogg", mediaType: "audio/ogg", body: []byte("inspection-audio-v1")}}
	refs, err := replicateAttachments(crdt.ProtocolPolicy{AllowExperimental: true}, objects)
	if err != nil {
		t.Fatal(err)
	}
	if err := refs[0].Verify(bytes.NewReader([]byte("inspection-audio-X1"))); !errors.Is(err, attachment.ErrContentMismatch) {
		t.Fatalf("Verify(tampered) = %v, want %v", err, attachment.ErrContentMismatch)
	}
}

func TestExperimentalPolicyIsRequired(t *testing.T) {
	if _, err := replicateNote(crdt.ProtocolPolicy{}); err == nil {
		t.Fatal("text replication accepted without experimental policy")
	}
	if _, err := replicateAttachments(crdt.ProtocolPolicy{}, nil); err == nil {
		t.Fatal("attachment replication accepted without experimental policy")
	}
}
