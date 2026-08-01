package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/DarkInno/crdt"
)

func TestRunWritesOneMachineReadableProfile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if exitCode := run([]string{"-id", "text/run-v2", "-format", "json"}, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("run exit code = %d, stderr=%q", exitCode, stderr.String())
	}
	var profiles []profileOutput
	if err := json.Unmarshal(stdout.Bytes(), &profiles); err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 || profiles[0].ID != "text/run-v2" || profiles[0].FrameType.StateID != crdt.TypeIDRGARunState || profiles[0].FrameType.DeltaID != crdt.TypeIDRGARunDelta || profiles[0].FrameType.SemanticsVersion != crdt.SemanticsVersionRGARun || !profiles[0].FrameType.UsesHLC || profiles[0].RequiresCodecID {
		t.Fatalf("JSON profiles = %#v", profiles)
	}
	if len(profiles[0].HostRequirements) == 0 || len(profiles[0].NotFor) == 0 {
		t.Fatalf("JSON profile omitted safety guidance: %#v", profiles[0])
	}
}

func TestRunListsProfilesForHumans(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if exitCode := run(nil, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("run exit code = %d, stderr=%q", exitCode, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"counter/grow-only", "set/add-wins", "text/run-v2", "document/tree-v1", "host must:"} {
		if !strings.Contains(output, want) {
			t.Fatalf("text output missing %q:\n%s", want, output)
		}
	}
}

func TestRunRejectsUnknownProfilesFormatsAndArguments(t *testing.T) {
	for _, args := range [][]string{
		{"-id", "counter/unknown"},
		{"-format", "yaml"},
		{"extra"},
		{"-unknown-flag"},
	} {
		var stdout, stderr bytes.Buffer
		if exitCode := run(args, &stdout, &stderr); exitCode != 2 || stderr.Len() == 0 {
			t.Fatalf("run(%q) = %d, stdout=%q stderr=%q", args, exitCode, stdout.String(), stderr.String())
		}
	}
}

func TestRunReportsTextAndJSONOutputFailures(t *testing.T) {
	for _, args := range [][]string{
		{"-id", "counter/grow-only"},
		{"-id", "counter/grow-only", "-format", "json"},
	} {
		var stderr bytes.Buffer
		if exitCode := run(args, errWriter{}, &stderr); exitCode != 1 || !strings.Contains(stderr.String(), "write profile output") {
			t.Fatalf("run(%q) = %d, stderr=%q", args, exitCode, stderr.String())
		}
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("write profile output") }
