package sftpclient

import (
	"bytes"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pkg/sftp"
)

func TestMkdirsCreatesAndCachesDirectories(t *testing.T) {
	manager, _ := newTestManager(t)
	root := filepath.ToSlash(t.TempDir())
	batch := path.Join(root, "batch")
	nested := path.Join(batch, "nested")

	if err := manager.Mkdirs("session-1", []string{nested, batch, nested}); err != nil {
		t.Fatalf("Mkdirs: %v", err)
	}
	session := manager.sessions["session-1"]
	for _, dir := range []string{batch, nested} {
		info, err := os.Stat(filepath.FromSlash(dir))
		if err != nil || !info.IsDir() {
			t.Fatalf("directory %q was not created: %v", dir, err)
		}
		if !session.dirs.has(dir) {
			t.Fatalf("directory %q was not remembered", dir)
		}
	}
}

func TestMkdirsRejectsPathOccupiedByFile(t *testing.T) {
	manager, _ := newTestManager(t)
	filePath := path.Join(filepath.ToSlash(t.TempDir()), "occupied")
	if err := os.WriteFile(filepath.FromSlash(filePath), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := manager.Mkdirs("session-1", []string{filePath}); err == nil {
		t.Fatal("Mkdirs accepted a path that is a file")
	}
}

// A directory may vanish after the cache confirmed it; the upload must rebuild
// it instead of failing the transfer.
func TestUploadRebuildsCachedDirectoryThatDisappeared(t *testing.T) {
	manager, _ := newTestManager(t)
	session := manager.sessions["session-1"]
	targetDir := path.Join(filepath.ToSlash(t.TempDir()), "gone", "deeper")
	session.dirs.add(targetDir)

	if err := manager.Upload("session-1", targetDir, "file.txt", strings.NewReader("hello")); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	content, err := os.ReadFile(filepath.FromSlash(path.Join(targetDir, "file.txt")))
	if err != nil {
		t.Fatalf("uploaded file missing: %v", err)
	}
	if string(content) != "hello" {
		t.Fatalf("content = %q, want %q", content, "hello")
	}
}

func TestMkdirWarmsDirectoryCache(t *testing.T) {
	manager, _ := newTestManager(t)
	dir := path.Join(filepath.ToSlash(t.TempDir()), "single")
	if err := manager.Mkdir("session-1", dir); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if !manager.sessions["session-1"].dirs.has(dir) {
		t.Fatal("Mkdir did not remember the directory")
	}
}

// The production client pipelines writes; a payload spanning many packets must
// still land byte for byte.
func TestUploadWithConcurrentWritesKeepsLargeFileIntact(t *testing.T) {
	client := newTestSFTPClientWithOptions(t,
		sftp.UseConcurrentWrites(true),
		sftp.MaxConcurrentRequestsPerFile(writeWindow),
	)
	root := filepath.ToSlash(t.TempDir())
	payload := make([]byte, 320*1024)
	for index := range payload {
		payload[index] = byte(index * 31)
	}
	if err := uploadFile(client, newDirCache(), root, "large.bin", bytes.NewReader(payload)); err != nil {
		t.Fatalf("uploadFile: %v", err)
	}
	written, err := os.ReadFile(filepath.FromSlash(path.Join(root, "large.bin")))
	if err != nil {
		t.Fatalf("uploaded file missing: %v", err)
	}
	if !bytes.Equal(written, payload) {
		t.Fatalf("uploaded file differs: %d bytes, want %d", len(written), len(payload))
	}
}

func TestDeleteClearsDirectoryCache(t *testing.T) {
	manager, _ := newTestManager(t)
	session := manager.sessions["session-1"]
	dir := path.Join(filepath.ToSlash(t.TempDir()), "remove-me")
	if err := manager.Mkdirs("session-1", []string{dir}); err != nil {
		t.Fatalf("Mkdirs: %v", err)
	}
	if err := manager.Delete("session-1", dir); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if session.dirs.has(dir) {
		t.Fatal("Delete left the removed directory in the cache")
	}
}
