package gui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"chemssh-launcher/internal/sftpclient"
)

const (
	// dragStageDirPrefix names the throwaway directory a native drag-out hands to
	// Explorer. The copies outlive the drag because Explorer keeps reading them
	// after the drop returns.
	dragStageDirPrefix = "chemssh-drag-"
	// dragStageMaxAge is how long a staged copy survives before a later staging
	// prunes it.
	dragStageMaxAge = 6 * time.Hour
)

// dragStageSource is the slice of the SFTP manager that staging needs, so tests
// can serve a remote tree without a live SSH connection.
type dragStageSource interface {
	Stat(session, path string) (sftpclient.FileInfo, error)
	List(session, path string) (sftpclient.ListResult, error)
	Download(session, path string) (io.ReadCloser, sftpclient.FileInfo, error)
}

// stageDragFiles copies the remote files and directories a native drag exposes
// into a throwaway directory and returns their local paths.
//
// The launcher's own panes never read these copies: they receive the drag marker
// through CF_UNICODETEXT and run an internal transfer instead. The copies exist
// only for drop targets outside the launcher. The native loop starts staging
// when it reaches an external window and waits before handing out the file list.
func (s *Server) stageDragFiles(ctx context.Context, session string, remotePaths []string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(session) == "" {
		return nil, errors.New("an SFTP session is required to drag files out")
	}
	if len(remotePaths) == 0 {
		return nil, errors.New("no files to drag")
	}
	root, err := dragStageRoot()
	if err != nil {
		return nil, err
	}
	pruneDragStage(root)
	dir, err := createDragStage(root)
	if err != nil {
		return nil, err
	}
	completed := false
	defer func() { finishDragStage(ctx, dir, completed) }()
	staged, err := stageDragPathsContext(ctx, s.sftp, session, remotePaths, dir)
	if err != nil {
		// A partial staging would drop a subset of the selection, which is worse
		// than reporting that the drag failed.
		_ = os.RemoveAll(dir)
		return nil, err
	}
	s.logs.add(fmt.Sprintf("SFTP drag-out: staged %d item(s) for the system drag", len(staged)))
	completed = true
	return staged, nil
}

type dragStageContextSource interface {
	StatContext(context.Context, string, string) (sftpclient.FileInfo, error)
	ListContext(context.Context, string, string) (sftpclient.ListResult, error)
	DownloadContext(context.Context, string, string) (io.ReadCloser, sftpclient.FileInfo, error)
}

// Legacy helpers keep their background-context behavior for existing callers.
func stageDragPaths(source dragStageSource, session string, remotePaths []string, dir string) ([]string, error) {
	return stageDragPathsContext(context.Background(), source, session, remotePaths, dir)
}

func stageDragPathsContext(ctx context.Context, source dragStageSource, session string, remotePaths []string, dir string) (staged []string, err error) {
	// This directory belongs to this staging attempt. No partial selection can
	// be handed to the drop target, including files completed before cancel.
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	used := make(map[string]bool, len(remotePaths))
	for _, remotePath := range remotePaths {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		var localPath string
		localPath, err = stageDragPathContext(ctx, source, session, remotePath, dir, used)
		if err != nil {
			return nil, err
		}
		staged = append(staged, localPath)
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return staged, nil
}

func stageDragPathContext(ctx context.Context, source dragStageSource, session, remotePath, dir string, used map[string]bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var info sftpclient.FileInfo
	var err error
	if contextual, ok := source.(dragStageContextSource); ok {
		info, err = contextual.StatContext(ctx, session, remotePath)
	} else {
		info, err = source.Stat(session, remotePath)
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", err
	}
	target := filepath.Join(dir, uniqueDragStageName(dragStageName(info.Name, remotePath), used))
	if info.IsDir {
		if err := os.Mkdir(target, 0o700); err != nil {
			return "", err
		}
		if err := stageDragDirectoryContext(ctx, source, session, remotePath, target); err != nil {
			return "", err
		}
		return target, nil
	}
	if err := copyDragFileContext(ctx, source, session, remotePath, target); err != nil {
		return "", err
	}
	return target, nil
}

func stageDragDirectoryContext(ctx context.Context, source dragStageSource, session, remotePath, localDir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var listing sftpclient.ListResult
	var err error
	if contextual, ok := source.(dragStageContextSource); ok {
		listing, err = contextual.ListContext(ctx, session, remotePath)
	} else {
		listing, err = source.List(session, remotePath)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	used := make(map[string]bool, len(listing.Entries))
	for _, entry := range listing.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		// A symlinked directory can point back at an ancestor, so the staged tree
		// deliberately stops at it.
		if entry.IsSymlink && entry.IsDir {
			continue
		}
		target := filepath.Join(localDir, uniqueDragStageName(dragStageName(entry.Name, entry.Path), used))
		if entry.IsDir {
			if err := os.Mkdir(target, 0o700); err != nil {
				return err
			}
			if err := stageDragDirectoryContext(ctx, source, session, entry.Path, target); err != nil {
				return err
			}
			continue
		}
		if err := copyDragFileContext(ctx, source, session, entry.Path, target); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func copyDragFile(source dragStageSource, session, remotePath, target string) error {
	return copyDragFileContext(context.Background(), source, session, remotePath, target)
}

func copyDragFileContext(ctx context.Context, source dragStageSource, session, remotePath, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var reader io.ReadCloser
	var info sftpclient.FileInfo
	var err error
	if contextual, ok := source.(dragStageContextSource); ok {
		reader, info, err = contextual.DownloadContext(ctx, session, remotePath)
	} else {
		reader, info, err = source.Download(session, remotePath)
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	defer reader.Close()
	if err := ctx.Err(); err != nil {
		return err
	}

	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	buf := make([]byte, copyBufferSize)
	copied, copyErr := io.CopyBuffer(file, &dragContextReader{ctx: ctx, reader: reader}, buf)
	if ctx.Err() != nil {
		copyErr = ctx.Err()
	}
	if copyErr == nil && copied != info.Size {
		copyErr = fmt.Errorf("the remote file changed size during drag-out: expected %d bytes, received %d", info.Size, copied)
	}
	if copyErr != nil {
		_ = file.Close()
		_ = os.Remove(target)
		return copyErr
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(target)
		return err
	}
	return nil
}

type dragContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *dragContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if r.ctx.Err() != nil {
		err = r.ctx.Err()
	}
	return n, err
}

// dragStageName flattens a remote name into something Explorer accepts: no path
// separators, no characters Windows rejects, no trailing dots or spaces.
func dragStageName(infoName, remotePath string) string {
	name := infoName
	if name == "" || name == "." || name == ".." || name == "/" {
		name = path.Base(remotePath)
	}
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		switch r {
		case ':', '*', '?', '"', '<', '>', '|':
			return '_'
		}
		if r < 0x20 {
			return -1
		}
		return r
	}, name)
	name = strings.TrimRight(name, " .")
	if name == "" {
		name = "file"
	}
	// Windows treats these as devices even when followed by an extension.
	base := strings.ToUpper(strings.TrimRight(strings.SplitN(name, ".", 2)[0], " "))
	reserved := base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || base == "CONIN$" || base == "CONOUT$"
	if len(base) > 3 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) {
		reserved = strings.Contains("123456789¹²³", base[3:]) && len([]rune(base[3:])) == 1
	}
	if reserved {
		name = "_" + name
	}
	return name
}

func uniqueDragStageName(name string, used map[string]bool) string {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	candidate := name
	// The remote filesystem may distinguish names that collide on Windows.
	for index := 2; used[strings.ToLower(candidate)]; index++ {
		candidate = fmt.Sprintf("%s (%d)%s", stem, index, ext)
	}
	used[strings.ToLower(candidate)] = true
	return candidate
}

func dragStageRoot() (string, error) {
	root := filepath.Join(os.TempDir(), "chemssh-launcher", "drag")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	return root, nil
}

// activeDragStages protects directories while staging and, on success, until a
// cancellable drag's context ends after the Shell finishes copying.
// Creation/registration and prune's check/removal share the lock so a concurrent
// prune cannot remove a directory in the gap before it is registered.
var activeDragStages = struct {
	sync.Mutex
	dirs map[string]struct{}
}{dirs: make(map[string]struct{})}

func createDragStage(root string) (string, error) {
	activeDragStages.Lock()
	defer activeDragStages.Unlock()
	dir, err := os.MkdirTemp(root, dragStageDirPrefix)
	if err != nil {
		return "", err
	}
	activeDragStages.dirs[dir] = struct{}{}
	return dir, nil
}

// finishDragStage transfers protection to the drag context only after a
// successful stage. The host cancels that context when the Shell is done; a
// background context retains the legacy release-on-return behavior.
func finishDragStage(ctx context.Context, dir string, completed bool) {
	if !completed || ctx.Done() == nil || ctx.Err() != nil {
		releaseDragStage(dir)
		return
	}
	context.AfterFunc(ctx, func() { releaseDragStage(dir) })
}

func releaseDragStage(dir string) {
	activeDragStages.Lock()
	delete(activeDragStages.dirs, dir)
	activeDragStages.Unlock()
}

// pruneDragStage drops expired copies left behind by earlier drags. Active
// staging and Shell handoffs are protected even beyond the normal age.
func pruneDragStage(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	deadline := time.Now().Add(-dragStageMaxAge)
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), dragStageDirPrefix) {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil || info.ModTime().After(deadline) {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		activeDragStages.Lock()
		if _, active := activeDragStages.dirs[dir]; !active {
			_ = os.RemoveAll(dir)
		}
		activeDragStages.Unlock()
	}
}
