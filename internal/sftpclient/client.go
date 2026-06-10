package sftpclient

import (
	"errors"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/pkg/sftp"
)

type RemoteEntry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	IsDir   bool      `json:"is_dir"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"`
	ModTime time.Time `json:"mod_time"`
}

type ListResult struct {
	Path    string        `json:"path"`
	Entries []RemoteEntry `json:"entries"`
}

type FileInfo struct {
	Name string
	Size int64
}

func listDirectory(client *sftp.Client, remotePath string) (ListResult, error) {
	cleanPath := CleanPath(remotePath)
	infos, err := client.ReadDir(cleanPath)
	if err != nil {
		return ListResult{}, err
	}
	type sortableRemoteEntry struct {
		isFile   bool
		sortName string
		entry    RemoteEntry
	}
	sortable := make([]sortableRemoteEntry, 0, len(infos))
	for _, info := range infos {
		name := info.Name()
		if name == "." || name == ".." {
			continue
		}
		isDir := info.IsDir()
		sortable = append(sortable, sortableRemoteEntry{
			isFile:   !isDir,
			sortName: strings.ToLower(name),
			entry:    entryFromInfo(cleanPath, info, name, isDir),
		})
	}
	sort.Slice(sortable, func(i, j int) bool {
		if sortable[i].isFile != sortable[j].isFile {
			return !sortable[i].isFile
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
	info, err := client.Stat(cleanPath)
	if err != nil {
		return err
	}
	if !info.IsDir() {
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
