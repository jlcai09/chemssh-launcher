package gui

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func testEventsServer() *Server {
	return &Server{
		apiToken:  "test-token",
		transfers: newTransferProgressStore(),
		shells:    newShellManager(),
	}
}

func dialEvents(t *testing.T, ts *httptest.Server, cookie string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	header := http.Header{}
	if cookie != "" {
		header.Set("Cookie", launcherAPITokenCookie+"="+cookie)
	}
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http"), &websocket.DialOptions{HTTPHeader: header})
}

func readEventsMessage(t *testing.T, ctx context.Context, conn *websocket.Conn) eventsServerMessage {
	t.Helper()
	typ, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("message type = %v, want text", typ)
	}
	var msg eventsServerMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	return msg
}

func TestEventsWebSocketRejectsMissingToken(t *testing.T) {
	server := testEventsServer()
	ts := httptest.NewServer(http.HandlerFunc(server.handleEventsWS))
	defer ts.Close()
	conn, resp, err := dialEvents(t, ts, "")
	if conn != nil {
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}
	if err == nil {
		t.Fatal("websocket without the token cookie must fail")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %v, want 403", resp)
	}
}

func TestEventsWebSocketPushesTransferProgress(t *testing.T) {
	server := testEventsServer()
	ts := httptest.NewServer(http.HandlerFunc(server.handleEventsWS))
	defer ts.Close()
	conn, _, err := dialEvents(t, ts, "test-token")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sendEvents(t, ctx, conn, map[string]any{"type": "transfer:watch", "id": "transfer-1"})

	first := readEventsMessage(t, ctx, conn)
	if first.Type != "transfer:progress" || first.ID != "transfer-1" || first.Progress == nil {
		t.Fatalf("first frame = %+v", first)
	}

	if _, err := server.transfers.begin("transfer-1", 100, false); err != nil {
		t.Fatal(err)
	}
	server.transfers.add("transfer-1", 40)
	waitForProgress(t, ctx, conn, func(progress transferProgress) bool { return progress.Loaded == 40 })

	if err := server.transfers.control("transfer-1", "cancel"); err != nil {
		t.Fatal(err)
	}
	waitForProgress(t, ctx, conn, func(progress transferProgress) bool { return progress.Cancelled })
}

func TestEventsWebSocketStreamsTerminalOutputAndExit(t *testing.T) {
	server := testEventsServer()
	hub := newShellHub()
	hub.publish([]byte("before-attach"))
	server.shells.sessions["shell-1"] = &launcherShell{id: "shell-1", hub: hub}

	ts := httptest.NewServer(http.HandlerFunc(server.handleEventsWS))
	defer ts.Close()
	conn, _, err := dialEvents(t, ts, "test-token")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sendEvents(t, ctx, conn, map[string]any{"type": "terminal:attach", "session": "shell-1", "since": 0})
	first := readEventsMessage(t, ctx, conn)
	if first.Type != "terminal:data" || first.Session != "shell-1" {
		t.Fatalf("replay frame = %+v", first)
	}
	decoded, err := base64.StdEncoding.DecodeString(first.Data)
	if err != nil || !bytes.Equal(decoded, []byte("before-attach")) {
		t.Fatalf("replay data = %q, %v", decoded, err)
	}

	hub.publish([]byte("live"))
	live := readEventsMessage(t, ctx, conn)
	decoded, _ = base64.StdEncoding.DecodeString(live.Data)
	if live.Type != "terminal:data" || !bytes.Equal(decoded, []byte("live")) {
		t.Fatalf("live frame = %+v (%q)", live, decoded)
	}

	hub.close()
	exit := waitForEventType(t, ctx, conn, "terminal:exit")
	if exit.Session != "shell-1" {
		t.Fatalf("exit frame = %+v", exit)
	}
}

func sendEvents(t *testing.T, ctx context.Context, conn *websocket.Conn, payload map[string]any) {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write events: %v", err)
	}
}

func waitForProgress(t *testing.T, ctx context.Context, conn *websocket.Conn, match func(transferProgress) bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		msg := readEventsMessage(t, ctx, conn)
		if msg.Type == "transfer:progress" && msg.Progress != nil && match(*msg.Progress) {
			return
		}
	}
	t.Fatal("progress frame not received")
}

func waitForEventType(t *testing.T, ctx context.Context, conn *websocket.Conn, eventType string) eventsServerMessage {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		msg := readEventsMessage(t, ctx, conn)
		if msg.Type == eventType {
			return msg
		}
	}
	t.Fatalf("%s frame not received", eventType)
	return eventsServerMessage{}
}

func TestShellHubReplaysChunksAndReportsGaps(t *testing.T) {
	hub := newShellHub()
	hub.publish([]byte("one"))
	hub.publish([]byte("two"))

	sub, replay, gap, closed := hub.attach(0)
	if sub == nil || closed || gap || len(replay) != 2 {
		t.Fatalf("attach = %v (gap=%v closed=%v)", replay, gap, closed)
	}
	if replay[0].seq != 1 || replay[1].seq != 2 {
		t.Fatalf("sequences = %d,%d", replay[0].seq, replay[1].seq)
	}
	hub.detach(sub)

	// Trim the replay window by publishing more than it can hold.
	big := bytes.Repeat([]byte("x"), 512*1024)
	hub.publish(big)
	hub.publish(big)
	hub.publish(big)
	_, replay, gap, _ = hub.attach(0)
	if !gap {
		t.Fatal("attach(0) after trimming must report a gap")
	}
	if len(replay) == 0 || replay[len(replay)-1].seq != 5 {
		t.Fatalf("replay tail = %+v", replay)
	}

	hub.close()
	if _, _, _, closed := hub.attach(5); !closed {
		t.Fatal("attach on a closed hub must report closed")
	}
}
