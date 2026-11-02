package gui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func newLocalArchiveTestServer() *Server {
	return &Server{archiveTools: newArchiveToolCache(), localLog: &safeLog{}}
}

func TestLocalCompressNeverOverwritesAndExtracts(t *testing.T) {
	if !hasLocalTool("tar") {
		t.Skip("tar is not installed")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "b.txt"), []byte("world"), 0o600); err != nil {
		t.Fatal(err)
	}

	server := newLocalArchiveTestServer()
	paths := []string{filepath.Join(dir, "a.txt"), filepath.Join(dir, "sub")}

	first := postLocalCompress(t, server, localArchiveCompressRequest{TargetDirectory: dir, Paths: paths, Format: "tar"})
	if first != "archive.tar" {
		t.Fatalf("first archive = %q", first)
	}
	second := postLocalCompress(t, server, localArchiveCompressRequest{TargetDirectory: dir, Paths: paths[:1], Format: "tar"})
	if second != "archive_new.tar" {
		t.Fatalf("second archive = %q", second)
	}

	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o700); err != nil {
		t.Fatal(err)
	}
	postLocalExtract(t, server, localArchiveExtractRequest{Path: filepath.Join(dir, "archive.tar"), TargetDirectory: out})
	for _, name := range []string{"a.txt", filepath.Join("sub", "b.txt")} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Fatalf("extracted %s missing: %v", name, err)
		}
	}
}

func TestLocalCompressRejectsEntriesOutsideTheFolder(t *testing.T) {
	if !hasLocalTool("tar") {
		t.Skip("tar is not installed")
	}
	dir := t.TempDir()
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "a.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := newLocalArchiveTestServer()
	body, _ := json.Marshal(localArchiveCompressRequest{
		TargetDirectory: dir,
		Paths:           []string{filepath.Join(other, "a.txt")},
		Format:          "tar",
	})
	rec := httptest.NewRecorder()
	server.handleLocalCompress(rec, httptest.NewRequest(http.MethodPost, "/api/local/compress", bytes.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func postLocalCompress(t *testing.T, server *Server, payload localArchiveCompressRequest) string {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	server.handleLocalCompress(rec, httptest.NewRequest(http.MethodPost, "/api/local/compress", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("compress status = %d: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Archive string `json:"archive"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response.Archive
}

func postLocalExtract(t *testing.T, server *Server, payload localArchiveExtractRequest) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	server.handleLocalExtract(rec, httptest.NewRequest(http.MethodPost, "/api/local/extract", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("extract status = %d: %s", rec.Code, rec.Body.String())
	}
}
