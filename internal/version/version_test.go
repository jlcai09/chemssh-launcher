package version

import (
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	if String() != strings.TrimSpace(rawVersion) {
		t.Fatalf("unexpected version: %q", String())
	}
}
