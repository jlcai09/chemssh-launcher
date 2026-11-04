package gui

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"chemssh-launcher/internal/sftpclient"
)

func TestDragStageReservedWindowsNames(t *testing.T) {
	for _, name := range []string{"CON", "NUL.txt", "aux.log", "COM1", "lpt9.out", "COM¹.dat", "CONIN$", "CONOUT$"} {
		if got := dragStageName(name, "/home/user/file"); got != "_"+name {
			t.Errorf("reserved name %q = %q", name, got)
		}
	}
	for _, name := range []string{"COM0", "COM10.txt", "COM1-data", "normal.txt"} {
		if got := dragStageName(name, "/home/user/file"); got != name {
			t.Errorf("ordinary name %q = %q", name, got)
		}
	}
}

func TestStageDragMixedFilesAndEmptyFolderWithWindowsNameCollisions(t *testing.T) {
	source := fakeDragStageSource{
		stats: map[string]sftpclient.FileInfo{
			"/home/user/Report.txt": {Name: "Report.txt"},
			"/home/user/report.txt": {Name: "report.txt"},
			"/home/user/empty":      {Name: "empty", IsDir: true},
			"/home/user/NUL":        {Name: "NUL"},
		},
		lists: map[string]sftpclient.ListResult{"/home/user/empty": {}},
		files: map[string]string{
			"/home/user/Report.txt": "first",
			"/home/user/report.txt": "second",
			"/home/user/NUL":        "reserved",
		},
	}
	dir := t.TempDir()
	paths, err := stageDragPaths(source, "session", []string{"/home/user/Report.txt", "/home/user/report.txt", "/home/user/empty", "/home/user/NUL"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Report.txt", "report (2).txt", "empty", "_NUL"}
	if len(paths) != len(want) {
		t.Fatalf("staged %v, want %d entries", paths, len(want))
	}
	for i, name := range want {
		if filepath.Base(paths[i]) != name {
			t.Errorf("entry %d = %q, want %q", i, paths[i], name)
		}
	}
	assertStagedFile(t, paths[0], "first")
	assertStagedFile(t, paths[1], "second")
	assertStagedFile(t, paths[3], "reserved")
	entries, err := os.ReadDir(paths[2])
	if err != nil || len(entries) != 0 {
		t.Fatalf("empty directory missing or not empty: %v, %v", entries, err)
	}
}

type shortDragSource struct{ fakeDragStageSource }

func (f shortDragSource) Download(session, remotePath string) (io.ReadCloser, sftpclient.FileInfo, error) {
	reader, info, err := f.fakeDragStageSource.Download(session, remotePath)
	info.Size++
	return reader, info, err
}

func TestDragStageRefusesTruncatedFile(t *testing.T) {
	source := shortDragSource{fakeDragStageSource{files: map[string]string{"/home/user/file": "partial"}}}
	target := filepath.Join(t.TempDir(), "file")
	if err := copyDragFile(source, "session", "/home/user/file", target); err == nil {
		t.Fatal("truncated copy should fail, not be handed to Explorer")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("partial file was retained: %v", err)
	}
}
