package version

import "testing"

func TestString(t *testing.T) {
	if String() != "0.1.0" {
		t.Fatalf("unexpected version: %q", String())
	}
}
