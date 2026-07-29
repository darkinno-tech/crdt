package extensions

import (
	"bufio"
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/counter"
	"github.com/DarkInno/crdt/replica"
)

func TestChangeWireRoundTripsAndRejectsMalformedInput(t *testing.T) {
	manifest := testManifest(t, "wire-group")
	writer, err := counter.NewGCounter("writer")
	if err != nil {
		t.Fatal(err)
	}
	delta, err := writer.Increment(1)
	if err != nil {
		t.Fatal(err)
	}
	change := newCounterChange(t, manifest, "writer", 1, delta)
	encoded, err := marshalChange(change)
	if err != nil {
		t.Fatal(err)
	}
	dot, decoded, err := unmarshalChange(encoded, 1024, 128)
	if err != nil || dot != change.Dot || !bytes.Equal(decoded, change.Delta()) {
		t.Fatalf("round trip = %#v %x %v", dot, decoded, err)
	}
	for _, malformed := range [][]byte{
		nil,
		{0},
		{transportVersion, 0x81, 0x00},
		append(append([]byte(nil), encoded...), 0),
	} {
		if _, _, err := unmarshalChange(malformed, 1024, 128); !errors.Is(err, errInvalidWireMessage) {
			t.Fatalf("malformed %x error = %v", malformed, err)
		}
	}
	if _, _, err := unmarshalChange(encoded, len(encoded)-1, 128); !errors.Is(err, errInvalidWireMessage) {
		t.Fatalf("oversized configured limit error = %v", err)
	}
}

func TestHelloAndSSEWireValidation(t *testing.T) {
	manifest := testManifest(t, "hello-group")
	hello, err := marshalHello(manifest)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := unmarshalHello(hello)
	if err != nil || decoded != manifest {
		t.Fatalf("hello round trip = %#v, %v", decoded, err)
	}
	for _, malformed := range [][]byte{
		nil,
		[]byte(`{"version":2,"manifest":{}}`),
		append(append([]byte(nil), hello...), []byte(" true")...),
	} {
		if _, err := unmarshalHello(malformed); !errors.Is(err, errInvalidWireMessage) {
			t.Fatalf("malformed hello %q error = %v", malformed, err)
		}
	}

	var stream bytes.Buffer
	if !writeSSEEvent(&stream, "manifest", hello) {
		t.Fatal("writeSSEEvent failed")
	}
	event, data, err := readSSEEvent(bufio.NewReader(&stream), maxControlBytes)
	if err != nil || event != "manifest" || !bytes.Equal(data, hello) {
		t.Fatalf("SSE round trip = %q %x %v", event, data, err)
	}
	for _, malformed := range []string{
		"event: change\ndata: !!!\n\n",
		"event: change\ndata: YQ==\nnot-empty\n",
		"data: YQ==\n\n\n",
	} {
		if _, _, err := readSSEEvent(bufio.NewReader(strings.NewReader(malformed)), 16); !errors.Is(err, errInvalidWireMessage) {
			t.Fatalf("malformed SSE %q error = %v", malformed, err)
		}
	}
	if _, err := decodeSSEData("YQ==", 0); !errors.Is(err, errInvalidWireMessage) {
		t.Fatalf("zero max data error = %v", err)
	}
}

func TestNormalizeLimitsAndClientLimits(t *testing.T) {
	limits, err := normalizeLimits(0, 0, 0, 0, 0, 0)
	if err != nil || limits.maxMessageBytes != defaultMaxMessageBytes || limits.maxQueuedBytes < limits.maxMessageBytes {
		t.Fatalf("default limits = %#v, %v", limits, err)
	}
	if _, err := normalizeLimits(1023, 1, 1, 1023, 1, 1); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("too-small message error = %v", err)
	}
	if _, err := normalizeLimits(1024, 1, 1, 1023, 1, 1); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("too-small queue bytes error = %v", err)
	}
	clientLimits, err := normalizeClientLimits(ClientConfig{MaxMessageBytes: 1024})
	if err != nil || clientLimits.maxQueuedBytes != 1024 {
		t.Fatalf("client limits = %#v, %v", clientLimits, err)
	}
	if controlLimit(1024) != 1024 || controlLimit(defaultMaxMessageBytes) != maxControlBytes {
		t.Fatal("unexpected control limit")
	}
}

func FuzzWireDecoders(f *testing.F) {
	f.Add([]byte{transportVersion, 1, 'a', 1, 1, 0})
	f.Add([]byte{batchTransportVersion, 1, 6, transportVersion, 1, 'a', 1, 1, 0})
	f.Add([]byte(`{"version":1,"manifest":{}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _, _ = unmarshalChange(data, 1<<20, 128)
		_, _ = unmarshalChangeBatch(data, 1<<20, 128, defaultMaxBatchChanges)
		_, _ = unmarshalHello(data)
		_, _, _ = readSSEEvent(bufio.NewReader(bytes.NewReader(data)), 1<<20)
	})
}

func testManifest(t testing.TB, groupID string) replica.Manifest {
	t.Helper()
	manifest, err := replica.NewManifest(groupID, "example.com/counter/v1", 1, replica.Protocol{
		StateID:          crdt.TypeIDGCounterState,
		DeltaID:          crdt.TypeIDGCounterDelta,
		SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}
