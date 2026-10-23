package sftpclient

import (
	"errors"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
)

type RemoteEntry struct {
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	IsDir      bool      `json:"is_dir"`
	Size       int64     `json:"size"`
	Mode       string    `json:"mode"`
	ModTime    time.Time `json:"mod_time"`
	IsSymlink  bool      `json:"is_symlink,omitempty"`
	LinkTarget string    `json:"link_target,omitempty"`
	LinkBroken bool      `json:"link_broken,omitempty"`
}

type ListResult struct {
	Path    string        `json:"path"`
	Entries []RemoteEntry `json:"entries"`
}

type FileInfo struct {
	Name string
	Size int64
}

// linkResolveWorkers bounds the parallel stat lookups used to follow symlinks
// while listing a directory; each lookup costs an extra SSH round trip.
const linkResolveWorkers = 8

func listDirectory(client *sftp.Client, remotePath string) (ListResult, error) {
	cleanPath := CleanPath(remotePath)
	infos, err := client.ReadDir(cleanPath)
	if err != nil {
		return ListResult{}, err
	}
	type sortableRemoteEntry struct {
		sortName string
		entry    RemoteEntry
	}
	sortable := make([]sortableRemoteEntry, 0, len(infos))
	var symlinks []int
	for _, info := range infos {
		name := info.Name()
		if name == "." || name == ".." {
			continue
		}
		sortable = append(sortable, sortableRemoteEntry{
			sortName: strings.ToLower(name),
			entry:    entryFromInfo(cleanPath, info, name, info.IsDir()),
		})
		if info.Mode()&os.ModeSymlink != 0 {
			symlinks = append(symlinks, len(sortable)-1)
		}
	}
	if len(symlinks) > 0 {
		var wg sync.WaitGroup
		workers := make(chan struct{}, linkResolveWorkers)
		for _, index := range symlinks {
			entry := &sortable[index].entry
			wg.Add(1)
			workers <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-workers }()
				entry.resolveLink(client)
			}()
		}
		wg.Wait()
	}
	sort.Slice(sortable, func(i, j int) bool {
		if sortable[i].entry.IsDir != sortable[j].entry.IsDir {
			return sortable[i].entry.IsDir
		}
		return sortable[i].sortName < sortable[j].sortName
	})
	entries := make([]RemoteEntry, len(sortable))
	for index, item := range sortable {
		entries[index] = item.entry
	}
	return ListResult{Path: cleanPath, Entries: entries}, nil
}

func uploadFile(client *sftp.Client, remoteDir, fileName string, src io.Reader) error {
	name, err := cleanUploadName(fileName)
	if err != nil {
		return err
	}
	target := joinRemotePath(remoteDir, name)
	parent := path.Dir(target)
	if parent != "." && parent != "/" {
		if err := client.MkdirAll(parent); err != nil {
			return err
		}
	}
	dst, err := client.Create(target)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	return dst.Close()
}

func uploadFilePath(client *sftp.Client, remotePath string, src io.Reader) error {
	target := CleanPath(remotePath)
	if target == "." || target == "/" {
		return errors.New("invalid upload file path")
	}
	dst, err := client.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	return dst.Close()
}

func createFile(client *sftp.Client, remotePath string) error {
	cleanPath := CleanPath(remotePath)
	if cleanPath == "." || cleanPath == "/" {
		return errors.New("invalid upload file name")
	}
	parent := path.Dir(cleanPath)
	if parent != "." && parent != "/" {
		if err := client.MkdirAll(parent); err != nil {
			return err
		}
	}
	dst, err := client.OpenFile(cleanPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if err != nil {
		return err
	}
	return dst.Close()
}

func downloadFile(client *sftp.Client, remotePath string) (*sftp.File, FileInfo, error) {
	cleanPath := CleanPath(remotePath)
	info, err := client.Stat(cleanPath)
	if err != nil {
		return nil, FileInfo{}, err
	}
	if info.IsDir() {
		return nil, FileInfo{}, errors.New("cannot download a directory")
	}
	file, err := client.Open(cleanPath)
	if err != nil {
		return nil, FileInfo{}, err
	}
	return file, FileInfo{Name: info.Name(), Size: info.Size()}, nil
}

func makeDirectory(client *sftp.Client, remotePath string) error {
	cleanPath := CleanPath(remotePath)
	if cleanPath == "." || cleanPath == "/" {
		return errors.New("invalid directory path")
	}
	return client.Mkdir(cleanPath)
}

func renamePath(client *sftp.Client, oldPath, newPath string) error {
	cleanOld := CleanPath(oldPath)
	cleanNew := CleanPath(newPath)
	if cleanOld == "." || cleanOld == "/" || cleanNew == "." || cleanNew == "/" {
		return errors.New("invalid rename path")
	}
	return client.Rename(cleanOld, cleanNew)
}

func deletePath(client *sftp.Client, remotePath string) error {
	cleanPath := CleanPath(remotePath)
	if cleanPath == "." || cleanPath == "/" {
		return errors.New("refusing to delete root or current directory")
	}
	return deletePathRecursive(client, cleanPath)
}

func deletePathRecursive(client *sftp.Client, cleanPath string) error {
	info, err := client.Lstat(cleanPath)
	if err != nil {
		return err
	}
	// A link to a directory is deleted as a link: walking the target would
	// destroy files the user only meant to unlink.
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return client.Remove(cleanPath)
	}
	entries, err := client.ReadDir(cleanPath)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == "." || entry.Name() == ".." {
			continue
		}
		if err := deletePathRecursive(client, joinRemotePath(cleanPath, entry.Name())); err != nil {
			return err
		}
	}
	return client.RemoveDirectory(cleanPath)
}

func entryFromInfo(parent string, info os.FileInfo, name string, isDir bool) RemoteEntry {
	return RemoteEntry{
		Name:    name,
		Path:    joinRemotePath(parent, name),
		IsDir:   isDir,
		Size:    info.Size(),
		Mode:    info.Mode().String(),
		ModTime: info.ModTime(),
	}
}

// resolveLink replaces the link's own attributes with the target's, so a link
// to a directory can be opened and a link to a file reports its real size.
func (e *RemoteEntry) resolveLink(client *sftp.Client) {
	e.IsSymlink = true
	if target, err := client.ReadLink(e.Path); err == nil {
		e.LinkTarget = target
	}
	info, err := client.Stat(e.Path)
	if err != nil {
		e.LinkBroken = true
		return
	}
	e.IsDir = info.IsDir()
	e.Size = info.Size()
	e.ModTime = info.ModTime()
}

func CleanPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "."
	}
	if strings.HasPrefix(value, "~") {
		return value
	}
	cleaned := path.Clean(strings.ReplaceAll(value, "\\", "/"))
	if cleaned == "/" {
		return "/"
	}
	return cleaned
}

func ParentPath(value string) string {
	cleaned := CleanPath(value)
	if cleaned == "." || cleaned == "/" {
		return cleaned
	}
	parent := path.Dir(cleaned)
	if parent == "" {
		return "."
	}
	return parent
}

func joinRemotePath(parent, name string) string {
	parent = CleanPath(parent)
	if parent == "." {
		return name
	}
	if parent == "/" {
		return "/" + strings.TrimPrefix(name, "/")
	}
	return path.Join(parent, name)
}

func cleanUploadName(value string) (string, error) {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	value = strings.TrimPrefix(value, "/")
	cleaned := path.Clean(value)
	if cleaned == "." || cleaned == "/" || cleaned == "" || strings.HasPrefix(cleaned, "../") || cleaned == ".." {
		return "", errors.New("invalid upload file name")
	}
	return cleaned, nil
}
