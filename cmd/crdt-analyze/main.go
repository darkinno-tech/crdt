// Command crdt-analyze reports bounded, transport-safe metadata about one
// canonical CRDT frame. It is intended for operations and incident diagnosis;
// it does not accept a frame as trusted application state.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	frame "github.com/darkinno/crdt/encoding"
)

type analysis struct {
	FrameBytes   int    `json:"frame_bytes"`
	TypeID       uint64 `json:"type_id"`
	CodecID      string `json:"codec_id,omitempty"`
	PayloadBytes int    `json:"payload_bytes"`
	SHA256       string `json:"sha256"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("crdt-analyze", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("file", "", "path to one CRDT frame")
	maxBytes := flags.Int("max-bytes", frame.DefaultLimits().MaxFrameBytes, "maximum accepted input bytes")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *path == "" || *maxBytes <= 0 {
		return errors.New("usage: crdt-analyze -file FRAME [-max-bytes N]")
	}

	input, err := readBoundedFile(*path, *maxBytes)
	if err != nil {
		return fmt.Errorf("read frame: %w", err)
	}
	report, err := analyze(input, *maxBytes)
	if err != nil {
		return fmt.Errorf("analyze frame: %w", err)
	}
	return json.NewEncoder(stdout).Encode(report)
}

func readBoundedFile(path string, maxBytes int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return readBoundedAndClose(file, file, maxBytes)
}

func readBoundedAndClose(reader io.Reader, closer io.Closer, maxBytes int) ([]byte, error) {
	data, readErr := readBounded(reader, maxBytes)
	closeErr := closer.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return data, nil
}

func readBounded(reader io.Reader, maxBytes int) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, errors.New("maximum size must be positive")
	}
	data, err := io.ReadAll(io.LimitReader(reader, int64(maxBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBytes {
		return nil, fmt.Errorf("input exceeds %d bytes", maxBytes)
	}
	return data, nil
}

func analyze(data []byte, maxBytes int) (analysis, error) {
	limits := frame.DefaultLimits()
	limits.MaxFrameBytes = maxBytes
	if maxBytes < limits.MaxPayload {
		limits.MaxPayload = maxBytes
	}
	decoded, err := frame.UnmarshalFrame(data, limits)
	if err != nil {
		return analysis{}, err
	}
	digest := sha256.Sum256(data)
	return analysis{
		FrameBytes:   len(data),
		TypeID:       decoded.TypeID,
		CodecID:      decoded.CodecID,
		PayloadBytes: len(decoded.Payload),
		SHA256:       hex.EncodeToString(digest[:]),
	}, nil
}
