package gui

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLogsStreamMarksEndOfReplay(t *testing.T) {
	server := NewServer(nil)
	server.logs.add("first line")
	server.logs.add("second line")

	httpServer := httptest.NewServer(http.HandlerFunc(server.handleLogsStream))
	defer httpServer.Close()

	response, err := http.Get(httpServer.URL + "/api/logs/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	reader := bufio.NewReader(response.Body)
	var events []string
	deadline := time.Now().Add(5 * time.Second)
	for len(events) < 4 {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for replay events, got %v", events)
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read stream: %v; got %v", err, events)
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data: ") || strings.HasPrefix(line, "event: ") {
			events = append(events, line)
		}
	}

	if !strings.HasSuffix(events[0], "first line") || !strings.HasSuffix(events[1], "second line") {
		t.Fatalf("replayed events = %v, want first and second line", events[:2])
	}
	if events[2] != "event: replay-end" {
		t.Fatalf("third event = %q, want replay-end marker after the history", events[2])
	}
	if events[3] != "data: {}" {
		t.Fatalf("fourth event = %q, want the replay-end payload", events[3])
	}
}
