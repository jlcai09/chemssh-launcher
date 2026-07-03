package gui

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"chemssh-launcher/internal/config"
)

func TestIsRetriableProxyError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"connection refused", errors.New("dial tcp 127.0.0.1:8888: connection refused"), true},
		{"forcibly closed", errors.New("read tcp 127.0.0.1:61163->127.0.0.1:8889: wsarecv: An existing connection was forcibly closed by the remote host"), true},
		{"connection reset", errors.New("read tcp 127.0.0.1:12345->127.0.0.1:8888: connection reset by peer"), true},
		{"wsarecv error", errors.New("wsarecv: connection was aborted"), true},
		{"wsasend error", errors.New("wsasend: connection was aborted"), true},
		{"broken pipe", errors.New("write: broken pipe"), true},
		{"EOF error", errors.New("unexpected EOF"), true},
		{"generic error", errors.New("some other error"), false},
		{"timeout error", &timeoutError{}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRetriableProxyError(tt.err)
			if got != tt.want {
				t.Errorf("isRetriableProxyError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// timeoutError implements net.Error for testing
type timeoutError struct{}

func (e *timeoutError) Error() string   { return "timeout" }
func (e *timeoutError) Timeout() bool   { return true }
func (e *timeoutError) Temporary() bool { return true }

var _ net.Error = (*timeoutError)(nil)

func TestChemSSHProxyHTMLShowsLoadingUntilSessionReady(t *testing.T) {
	profile := config.NewProfileDefaults()
	profile.ID = "remote-1"
	profile.Name = "remote"
	session := &activeSession{
		id:         profile.ID,
		name:       profile.Name,
		profile:    profile,
		forwarding: true,
		ready:      false,
	}
	server := &Server{
		sessions:       map[string]*activeSession{profile.ID: session},
		session:        session,
		proxyProfileID: profile.ID,
		logs:           &safeLog{},
	}

	req := httptest.NewRequest(http.MethodGet, "/remote/chemssh?profile_id=remote-1", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()

	server.handleChemSSHProxy(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if got := rec.Header().Get("Refresh"); got == "" {
		t.Fatal("expected loading page refresh header")
	}
	if !strings.Contains(rec.Body.String(), "ChemSSH is starting up...") {
		t.Fatalf("response did not contain loading page: %s", rec.Body.String())
	}
}

func TestRewriteChemSSHProxyRequestStripsLauncherQuery(t *testing.T) {
	profile := config.NewProfileDefaults()
	profile.ID = "remote-1"
	profile.Name = "remote"
	session := &activeSession{id: profile.ID, name: profile.Name, profile: profile}
	server := &Server{}
	target, err := url.Parse("http://127.0.0.1:8888")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/remote/chemssh/api/status?profile_id=remote-1&launcher_open_seq=9&keep=1", nil)

	rewritten := server.rewriteChemSSHProxyRequest(req, target, session)

	if got := rewritten.URL.Query().Get("profile_id"); got != "" {
		t.Fatalf("profile_id leaked to target query: %q", got)
	}
	if got := rewritten.URL.Query().Get("launcher_open_seq"); got != "" {
		t.Fatalf("launcher_open_seq leaked to target query: %q", got)
	}
	if got := rewritten.URL.Query().Get("keep"); got != "1" {
		t.Fatalf("expected keep query to survive, got %q", got)
	}
}

func TestRewriteChemSSHProxyRequestAppliesLauncherClientIdentity(t *testing.T) {
	profile := config.NewProfileDefaults()
	profile.ID = "remote-1"
	profile.Name = "remote"
	session := &activeSession{id: profile.ID, name: profile.Name, profile: profile}
	server := &Server{
		clientIdentity: launcherClientIdentity{
			Version:   launcherClientIdentityVersion,
			ClientID:  "client_launcher_stable",
			CreatedAt: "2026-06-09T10:00:00Z",
		},
	}
	target, err := url.Parse("http://127.0.0.1:8888")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/remote/chemssh/api/client-cache", nil)
	req.Header.Set("X-ChemSSH-Client-Id", "client_random")

	rewritten := server.rewriteChemSSHProxyRequest(req, target, session)

	if got := rewritten.Header.Get("X-ChemSSH-Client-Id"); got != "client_launcher_stable" {
		t.Fatalf("client id header = %q, want stable launcher id", got)
	}
}

func TestRewriteChemSSHProxyRequestAppliesLauncherClientIdentityToTerminalWebSocket(t *testing.T) {
	profile := config.NewProfileDefaults()
	profile.ID = "remote-1"
	profile.Name = "remote"
	session := &activeSession{id: profile.ID, name: profile.Name, profile: profile}
	server := &Server{
		clientIdentity: launcherClientIdentity{
			Version:   launcherClientIdentityVersion,
			ClientID:  "client_launcher_stable",
			CreatedAt: "2026-06-09T10:00:00Z",
		},
	}
	target, err := url.Parse("http://127.0.0.1:8888")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/remote/chemssh/api/terminal/ws/session-1?client_id=client_random&keep=1", nil)

	rewritten := server.rewriteChemSSHProxyRequest(req, target, session)

	query := rewritten.URL.Query()
	if got := query.Get("client_id"); got != "client_launcher_stable" {
		t.Fatalf("client_id query = %q, want stable launcher id", got)
	}
	if got := query.Get("keep"); got != "1" {
		t.Fatalf("expected keep query to survive, got %q", got)
	}
	if got := rewritten.Header.Get("X-ChemSSH-Client-Id"); got != "client_launcher_stable" {
		t.Fatalf("client id header = %q, want stable launcher id", got)
	}
}

func TestInjectAfterHeadOpen(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"plain head", "<html><head><title>x</title></head><body></body></html>"},
		{"head with attributes", `<!doctype html><html lang="en"><head data-x="1"><meta charset="utf-8">`},
		{"no head", "<html><body></body></html>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := injectAfterHeadOpen([]byte(tt.body), "SNIPPET")
			if !bytes.Contains(out, []byte("SNIPPET")) {
				t.Fatalf("snippet not injected: %s", out)
			}
			// Removing the snippet must reconstruct the original body.
			reconstructed := bytes.ReplaceAll(out, []byte("SNIPPET"), nil)
			if !bytes.Equal(reconstructed, []byte(tt.body)) {
				t.Fatalf("original body content lost: got %s", reconstructed)
			}
		})
	}
}

func TestInjectChemSSHImportReloaderPlainHTML(t *testing.T) {
	body := []byte("<html><head><title>x</title></head><body></body></html>")
	resp := &http.Response{
		Header:        http.Header{},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
	}
	resp.Header.Set("Content-Type", "text/html; charset=utf-8")

	injectChemSSHImportReloader(resp)

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte("__chemsshReloaded")) {
		t.Fatalf("reloader script not injected: %s", got)
	}
	if resp.Header.Get("Content-Encoding") != "" {
		t.Fatalf("Content-Encoding should be stripped, got %q", resp.Header.Get("Content-Encoding"))
	}
	wantLen := strconv.Itoa(len(got))
	if got := resp.Header.Get("Content-Length"); got != wantLen {
		t.Fatalf("Content-Length = %q, want %s", got, wantLen)
	}
	if int64(len(got)) != resp.ContentLength {
		t.Fatalf("ContentLength = %d, want %d", resp.ContentLength, len(got))
	}
}

func TestInjectChemSSHImportReloaderGzip(t *testing.T) {
	original := []byte("<html><head><title>g</title></head><body></body></html>")
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(original); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}

	resp := &http.Response{
		Header:        http.Header{},
		Body:          io.NopCloser(bytes.NewReader(buf.Bytes())),
		ContentLength: int64(buf.Len()),
	}
	resp.Header.Set("Content-Type", "text/html")
	resp.Header.Set("Content-Encoding", "gzip")

	injectChemSSHImportReloader(resp)

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte("__chemsshReloaded")) {
		t.Fatalf("reloader script not injected after gzip decode: %s", got)
	}
	if !bytes.Contains(got, []byte("<title>g</title>")) {
		t.Fatalf("original content lost after gzip decode: %s", got)
	}
	if resp.Header.Get("Content-Encoding") != "" {
		t.Fatalf("Content-Encoding should be stripped for gzip, got %q", resp.Header.Get("Content-Encoding"))
	}
}

func TestInjectChemSSHImportReloaderSkipsNonHTML(t *testing.T) {
	body := []byte("function(){}")
	resp := &http.Response{
		Header: http.Header{},
		Body:   io.NopCloser(bytes.NewReader(body)),
	}
	resp.Header.Set("Content-Type", "application/javascript")

	injectChemSSHImportReloader(resp)

	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, body) {
		t.Fatalf("non-HTML body should be untouched, got %s", got)
	}
}

func TestInjectChemSSHImportReloaderSkipsUnsupportedEncoding(t *testing.T) {
	// A brotli-encoded body we cannot decode: original bytes must pass through.
	body := []byte{0x01, 0x02, 0x03, 0x04, 0x05}
	resp := &http.Response{
		Header: http.Header{},
		Body:   io.NopCloser(bytes.NewReader(body)),
	}
	resp.Header.Set("Content-Type", "text/html")
	resp.Header.Set("Content-Encoding", "br")

	injectChemSSHImportReloader(resp)

	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, body) {
		t.Fatalf("unsupported encoding body should be untouched, got %v", got)
	}
}
