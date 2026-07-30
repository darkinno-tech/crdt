package main

import (
	"strings"
	"testing"
)

func TestRegistryValidate(t *testing.T) {
	t.Parallel()

	valid := registry{
		FormatVersion: 1,
		FrameTypes: []frameType{
			{Name: "Counter", StateID: 1, DeltaID: 2, SemanticsVersion: 1},
			{Name: "Register", StateID: 3, DeltaID: 4, SemanticsVersion: 1, UsesHLC: true},
		},
	}
	if err := valid.validate(); err != nil {
		t.Fatalf("validate valid registry: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*registry)
	}{
		{
			name: "unsupported format",
			mutate: func(value *registry) {
				value.FormatVersion = 2
			},
		},
		{
			name: "duplicate name",
			mutate: func(value *registry) {
				value.FrameTypes[1].Name = value.FrameTypes[0].Name
			},
		},
		{
			name: "duplicate type ID",
			mutate: func(value *registry) {
				value.FrameTypes[1].DeltaID = value.FrameTypes[0].StateID
			},
		},
		{
			name: "state equals delta",
			mutate: func(value *registry) {
				value.FrameTypes[1].DeltaID = value.FrameTypes[1].StateID
			},
		},
		{
			name: "invalid exported name",
			mutate: func(value *registry) {
				value.FrameTypes[1].Name = "lowercase"
			},
		},
		{
			name: "zero semantics version",
			mutate: func(value *registry) {
				value.FrameTypes[1].SemanticsVersion = 0
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			value := valid
			value.FrameTypes = append([]frameType(nil), valid.FrameTypes...)
			test.mutate(&value)
			if err := value.validate(); err == nil {
				t.Fatal("validate accepted malformed registry")
			}
		})
	}
}

func TestRenderGoIncludesSemanticVersionsAndRegistrationNames(t *testing.T) {
	t.Parallel()

	value := registry{
		FormatVersion: 1,
		FrameTypes: []frameType{
			{Name: "Counter", StateID: 1, DeltaID: 2, SemanticsVersion: 7},
			{Name: "Register", StateID: 3, DeltaID: 4, SemanticsVersion: 8, UsesHLC: true},
		},
	}
	if err := value.validate(); err != nil {
		t.Fatal(err)
	}
	output := string(renderGo(value))
	for _, want := range []string{
		`Name: "Counter"`,
		`Name: "Register"`,
		"var frameTypeRegistrations",
		"SemanticsVersionCounter uint64 = 7",
		"SemanticsVersion: SemanticsVersionCounter",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("generated Go registry omitted %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "experimentalFrameTypes") {
		t.Fatalf("generated Go registry retained stale experimental table:\n%s", output)
	}
	if output := string(renderTypeScript(value)); !strings.Contains(output, "FrameSemanticsVersion") || !strings.Contains(output, "Counter: 7n") {
		t.Fatalf("renderTypeScript omitted semantic version:\n%s", output)
	}
}
