package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/darkinno/crdt"
	frame "github.com/darkinno/crdt/encoding"
)

func TestAnalyzeReportsVerifiedFrameMetadata(t *testing.T) {
	encoded, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDGCounterDelta, Payload: []byte("state")})
	if err != nil {
		t.Fatal(err)
	}
	report, err := analyze(encoded, len(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if report.FrameBytes != len(encoded) || report.TypeID != crdt.TypeIDGCounterDelta || report.PayloadBytes != len("state") {
		t.Fatalf("analysis = %#v", report)
	}
	if len(report.SHA256) != 64 {
		t.Fatalf("SHA256 length = %d, want 64", len(report.SHA256))
	}
}

func TestReadBoundedRejectsOversizedInput(t *testing.T) {
	if _, err := readBounded(bytes.NewReader([]byte("123")), 2); err == nil {
		t.Fatal("readBounded() succeeded for oversized input")
	}
	if _, err := readBounded(bytes.NewReader(nil), 0); err == nil {
		t.Fatal("readBounded() succeeded for zero maximum")
	}
	if _, err := analyze([]byte("invalid"), 16); !errors.Is(err, frame.ErrFrameLimit) && !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("analyze invalid input error = %v", err)
	}
}

func TestRunAnalyzesFileAndValidatesArguments(t *testing.T) {
	encoded, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDGCounterDelta, Payload: []byte("state")})
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/frame"
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := run([]string{"-file", path, "-max-bytes", "1024"}, &output); err != nil {
		t.Fatal(err)
	}
	var report analysis
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.TypeID != crdt.TypeIDGCounterDelta {
		t.Fatalf("type ID = %d", report.TypeID)
	}
	if err := run(nil, &output); err == nil {
		t.Fatal("run() accepted missing file")
	}
	if err := run([]string{"-file", path, "-max-bytes", "-1"}, &output); err == nil {
		t.Fatal("run() accepted negative maximum")
	}
	if _, err := readBoundedFile(path+"-missing", 1); err == nil {
		t.Fatal("readBoundedFile() accepted missing file")
	}
	if err := run([]string{"-unknown"}, &output); err == nil {
		t.Fatal("run() accepted unknown flag")
	}
	if err := run([]string{"-file", path}, failingWriter{}); err == nil {
		t.Fatal("run() ignored output failure")
	}
	if err := os.WriteFile(path, []byte("not-a-frame"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-file", path}, &output); err == nil {
		t.Fatal("run() accepted malformed frame")
	}
}

func TestReadBoundedPropagatesReaderFailure(t *testing.T) {
	if _, err := readBounded(failingReader{}, 16); err == nil {
		t.Fatal("readBounded() ignored reader failure")
	}
	if _, err := readBoundedFile(t.TempDir(), 16); err == nil {
		t.Fatal("readBoundedFile() accepted a directory as a frame")
	}
}

func TestReadBoundedAndClosePreservesFailurePriority(t *testing.T) {
	closeFailure := errors.New("close failure")
	if _, err := readBoundedAndClose(bytes.NewReader([]byte("frame")), closeErrorReader{closeErr: closeFailure}, 16); !errors.Is(err, closeFailure) {
		t.Fatalf("close error = %v, want %v", err, closeFailure)
	}
	readFailure := errors.New("read failure")
	if _, err := readBoundedAndClose(failingReader{err: readFailure}, closeErrorReader{closeErr: closeFailure}, 16); !errors.Is(err, readFailure) {
		t.Fatalf("error priority = %v, want %v", err, readFailure)
	}
}

type failingReader struct{ err error }

func (reader failingReader) Read([]byte) (int, error) {
	if reader.err != nil {
		return 0, reader.err
	}
	return 0, fmt.Errorf("read failure")
}

type closeErrorReader struct{ closeErr error }

func (closeErrorReader) Read([]byte) (int, error) { return 0, io.EOF }
func (reader closeErrorReader) Close() error      { return reader.closeErr }

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
