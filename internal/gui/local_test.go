package gui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListLocalDirectoryFollowsSymlinks(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "nowhere"), filepath.Join(root, "missing")); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}

	result, err := listLocalDirectory(root)
	if err != nil {
		t.Fatalf("listLocalDirectory: %v", err)
	}
	entries := make(map[string]localEntry, len(result.Entries))
	for _, entry := range result.Entries {
		entries[entry.Name] = entry
	}

	link := entries["link"]
	if !link.IsSymlink || !link.IsDir || link.LinkTarget != target {
		t.Fatalf("link entry = %+v, want a symlinked directory pointing at %s", link, target)
	}
	if missing := entries["missing"]; !missing.IsSymlink || missing.IsDir || !missing.LinkBroken {
		t.Fatalf("missing entry = %+v, want a broken symlink", missing)
	}
}

func TestListLocalDirectoryReturnsAbsolutePath(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	rel, err := filepath.Rel(".", root)
	if err != nil {
		t.Skipf("temp directory is not reachable from the working directory: %v", err)
	}

	result, err := listLocalDirectory(rel)
	if err != nil {
		t.Fatalf("listLocalDirectory: %v", err)
	}
	if !filepath.IsAbs(result.Path) {
		t.Fatalf("list path = %q, want an absolute path", result.Path)
	}
	if len(result.Entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(result.Entries))
	}
	if entry := result.Entries[0]; !filepath.IsAbs(entry.Path) {
		t.Fatalf("entry path = %q, want an absolute path", entry.Path)
	}
}

func TestLocalWriteHandlersRejectEmptyPaths(t *testing.T) {
	server := &Server{transfers: newTransferProgressStore()}
	tests := []struct {
		name    string
		path    string
		body    string
		handler http.HandlerFunc
	}{
		{
			name:    "delete empty path",
			path:    "/api/local/delete",
			body:    `{"path":""}`,
			handler: server.handleLocalDelete,
		},
		{
			name:    "rename empty source",
			path:    "/api/local/rename",
			body:    `{"path":"","new_path":"target.txt"}`,
			handler: server.handleLocalRename,
		},
		{
			name:    "rename empty target",
			path:    "/api/local/rename",
			body:    `{"path":"source.txt","new_path":""}`,
			handler: server.handleLocalRename,
		},
		{
			name:    "copy empty source",
			path:    "/api/local/copy",
			body:    `{"source_path":"","target_path":"target"}`,
			handler: server.handleLocalCopy,
		},
		{
			name:    "copy empty target",
			path:    "/api/local/copy",
			body:    `{"source_path":"source.txt","target_path":""}`,
			handler: server.handleLocalCopy,
		},
		{
			name:    "open empty path",
			path:    "/api/local/open",
			body:    `{"path":""}`,
			handler: server.handleLocalOpen,
		},
		{
			name:    "open text empty path",
			path:    "/api/local/open-text",
			body:    `{"path":""}`,
			handler: server.handleLocalOpenText,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			rec := httptest.NewRecorder()

			tt.handler(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
		})
	}
}
