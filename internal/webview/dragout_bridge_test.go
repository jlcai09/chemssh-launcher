package webview

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDragOutStagingPreservesRequestAndState(t *testing.T) {
	for _, caching := range []bool{true, false} {
		id := `frame-1-"</script>`
		script := dragOutStagingScript(id, caching)
		prefix := "window.dispatchEvent(new CustomEvent('chemssh-native-drag-staging', {detail:"
		if !strings.HasPrefix(script, prefix) || !strings.HasSuffix(script, "}))") {
			t.Fatalf("unexpected staging event: %s", script)
		}
		var state struct {
			ID      string `json:"id"`
			Caching bool   `json:"caching"`
		}
		encoded := strings.TrimSuffix(strings.TrimPrefix(script, prefix), "}))")
		if err := json.Unmarshal([]byte(encoded), &state); err != nil {
			t.Fatal(err)
		}
		if state.ID != id || state.Caching != caching || strings.Contains(script, "</script>") {
			t.Fatalf("staging state not preserved safely: %+v", state)
		}
	}
}

func TestDragOutCompletionPreservesRequestIDAndEscapesScript(t *testing.T) {
	id := `frame-1-"`
	status := "failed: \"quoted\"\n</script>"
	script := dragOutCompletionScript(id, status, false)
	prefix := "window.dispatchEvent(new CustomEvent('chemssh-native-drag-finished', {detail:"
	if !strings.HasPrefix(script, prefix) || !strings.HasSuffix(script, "}))") {
		t.Fatalf("unexpected completion event: %s", script)
	}
	encoded := strings.TrimSuffix(strings.TrimPrefix(script, prefix), "}))")
	var result struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		OK     bool   `json:"ok"`
	}
	if err := json.Unmarshal([]byte(encoded), &result); err != nil {
		t.Fatal(err)
	}
	if result.ID != id || result.Status != status || result.OK {
		t.Fatalf("completion lost request identity or result: %+v", result)
	}
	if strings.Contains(script, "</script>") {
		t.Fatalf("script contains unescaped HTML: %s", script)
	}
}
