package config

import "testing"

func FuzzLoaderTypedAccessors(f *testing.F) {
	f.Add("true", "16", "100ms")
	f.Add("invalid", "-1", "0s")
	f.Fuzz(func(t *testing.T, boolean, integer, duration string) {
		loader, err := New(NewMap(map[string]string{
			"BOOLEAN":  boolean,
			"INTEGER":  integer,
			"DURATION": duration,
		}))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = loader.Bool("BOOLEAN", false)
		_, _ = loader.Int("INTEGER", 1, 1, 1024)
		_, _ = loader.Duration("DURATION", 100)
	})
}
