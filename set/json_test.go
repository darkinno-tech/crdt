package set

import "testing"

func TestJSONDiagnosticsCoverSetStatesAndDeltas(t *testing.T) {
	codec := stringCodec{id: "example.com/json-diagnostics/v1"}
	gset, err := NewGSet("gset", codec)
	if err != nil {
		t.Fatal(err)
	}
	gsetDelta, err := gset.Add("value")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gset.MarshalJSON(); err != nil {
		t.Fatal(err)
	}
	if _, err := gsetDelta.MarshalJSON(); err != nil {
		t.Fatal(err)
	}

	orset, err := NewORSet("orset", codec)
	if err != nil {
		t.Fatal(err)
	}
	orsetDelta, err := orset.Add("value")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orset.MarshalJSON(); err != nil {
		t.Fatal(err)
	}
	if _, err := orsetDelta.MarshalJSON(); err != nil {
		t.Fatal(err)
	}
}
