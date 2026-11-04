package gui

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"chemssh-launcher/internal/sftpclient"
)

func TestDragStageNameFlattensRemoteNames(t *testing.T) {
	cases := []struct {
		name       string
		infoName   string
		remotePath string
		want       string
	}{
		{name: "plain", infoName: "result.pdb", remotePath: "/home/user/result.pdb", want: "result.pdb"},
		{name: "slash in name", infoName: "a/b.txt", remotePath: "/home/user/b.txt", want: "b.txt"},
		{name: "windows separator", infoName: `C:\data\run.log`, remotePath: "/home/user/run.log", want: "run.log"},
		{name: "reserved characters", infoName: `re:port*?"<>|.txt`, remotePath: "/home/user/x.txt", want: "re_port______.txt"},
		{name: "trailing dots and spaces", infoName: "notes. . ", remotePath: "/home/user/notes", want: "notes"},
		{name: "control characters", infoName: "bad\x01name.txt", remotePath: "/home/user/x.txt", want: "badname.txt"},
		{name: "empty falls back to path", infoName: "", remotePath: "/home/user/output.cif", want: "output.cif"},
		{name: "dot falls back to path", infoName: ".", remotePath: "/home/user/output.cif", want: "output.cif"},
		{name: "empty after cleaning", infoName: " ... ", remotePath: "/home/user/output.cif", want: "file"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := dragStageName(testCase.infoName, testCase.remotePath); got != testCase.want {
				t.Fatalf("dragStageName(%q, %q) = %q, want %q", testCase.infoName, testCase.remotePath, got, testCase.want)
			}
		})
	}
}

func TestUniqueDragStageNameKeepsEveryFile(t *testing.T) {
	used := map[string]bool{}
	got := []string{
		uniqueDragStageName("run.log", used),
		uniqueDragStageName("run.log", used),
		uniqueDragStageName("run.log", used),
		uniqueDragStageName("other.log", used),
	}
	want := []string{"run.log", "run (2).log", "run (3).log", "other.log"}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("unique names = %v, want %v", got, want)
		}
	}
}

func TestStageDragFilesRejectsEmptyRequests(t *testing.T) {
	server := &Server{logs: &safeLog{}}
	if _, err := server.stageDragFiles(context.Background(), "", []string{"/home/user/a.txt"}); err == nil {
		t.Fatal("expected an error for a missing session")
	}
	if _, err := server.stageDragFiles(context.Background(), "session", nil); err == nil {
		t.Fatal("expected an error for an empty file list")
	}
}

type fakeDragStageSource struct {
	stats map[string]sftpclient.FileInfo
	lists map[string]sftpclient.ListResult
	files map[string]string
}

func (f fakeDragStageSource) Stat(_, remotePath string) (sftpclient.FileInfo, error) {
	info, ok := f.stats[remotePath]
	if !ok {
		return sftpclient.FileInfo{}, errors.New("missing stat: " + remotePath)
	}
	return info, nil
}

func (f fakeDragStageSource) List(_, remotePath string) (sftpclient.ListResult, error) {
	listing, ok := f.lists[remotePath]
	if !ok {
		return sftpclient.ListResult{}, errors.New("missing listing: " + remotePath)
	}
	return listing, nil
}

func (f fakeDragStageSource) Download(_, remotePath string) (io.ReadCloser, sftpclient.FileInfo, error) {
	content, ok := f.files[remotePath]
	if !ok {
		return nil, sftpclient.FileInfo{}, errors.New("missing file: " + remotePath)
	}
	return io.NopCloser(strings.NewReader(content)), sftpclient.FileInfo{
		Name:  filepath.Base(remotePath),
		Size:  int64(len(content)),
		IsDir: false,
	}, nil
}

func TestStageDragPathsRecursesIntoDirectories(t *testing.T) {
	source := fakeDragStageSource{
		stats: map[string]sftpclient.FileInfo{
			"/home/user/results": {Name: "results", IsDir: true},
		},
		lists: map[string]sftpclient.ListResult{
			"/home/user/results": {
				Path: "/home/user/results",
				Entries: []sftpclient.RemoteEntry{
					{Name: "a.pdb", Path: "/home/user/results/a.pdb"},
					{Name: "sub", Path: "/home/user/results/sub", IsDir: true},
					{Name: "loop", Path: "/home/user/results/loop", IsDir: true, IsSymlink: true},
				},
			},
			"/home/user/results/sub": {
				Path: "/home/user/results/sub",
				Entries: []sftpclient.RemoteEntry{
					{Name: "bad:name.txt", Path: "/home/user/results/sub/bad:name.txt"},
				},
			},
		},
		files: map[string]string{
			"/home/user/results/a.pdb":            "pdb",
			"/home/user/results/sub/bad:name.txt": "notes",
		},
	}
	dir := t.TempDir()
	staged, err := stageDragPaths(source, "session", []string{"/home/user/results"}, dir)
	if err != nil {
		t.Fatalf("stageDragPaths: %v", err)
	}
	if len(staged) != 1 || staged[0] != filepath.Join(dir, "results") {
		t.Fatalf("staged = %v, want one directory", staged)
	}
	assertStagedFile(t, filepath.Join(staged[0], "a.pdb"), "pdb")
	assertStagedFile(t, filepath.Join(staged[0], "sub", "bad_name.txt"), "notes")
	if _, err := os.Stat(filepath.Join(staged[0], "loop")); !os.IsNotExist(err) {
		t.Fatalf("symlinked directory was followed: %v", err)
	}
}

func TestStageDragPathsKeepsDuplicateNames(t *testing.T) {
	source := fakeDragStageSource{
		stats: map[string]sftpclient.FileInfo{
			"/home/user/a/run.log": {Name: "run.log", Size: 1},
			"/home/user/b/run.log": {Name: "run.log", Size: 1},
		},
		files: map[string]string{
			"/home/user/a/run.log": "first",
			"/home/user/b/run.log": "second",
		},
	}
	dir := t.TempDir()
	staged, err := stageDragPaths(source, "session", []string{"/home/user/a/run.log", "/home/user/b/run.log"}, dir)
	if err != nil {
		t.Fatalf("stageDragPaths: %v", err)
	}
	assertStagedFile(t, staged[0], "first")
	assertStagedFile(t, staged[1], "second")
	if filepath.Base(staged[0]) != "run.log" || filepath.Base(staged[1]) != "run (2).log" {
		t.Fatalf("staged names = %q, %q", staged[0], staged[1])
	}
}

func assertStagedFile(t *testing.T, path, want string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read staged file %s: %v", path, err)
	}
	if string(content) != want {
		t.Fatalf("staged file %s = %q, want %q", path, content, want)
	}
}
