package gui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"chemssh-launcher/internal/browser"
)

type localEntry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	IsDir   bool      `json:"is_dir"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"`
	ModTime time.Time `json:"mod_time"`
}

type localListResult struct {
	Path    string       `json:"path"`
	Entries []localEntry `json:"entries"`
}

func (s *Server) handleLocalHome(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	path, err := defaultLocalPath()
	writeJSON(w, map[string]string{"path": path}, err)
}

func (s *Server) handleLocalList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	result, err := listLocalDirectory(r.URL.Query().Get("path"))
	writeJSON(w, result, err)
}

func (s *Server) handleLocalMkdir(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		writeError(w, http.StatusBadRequest, errors.New("missing local path"))
		return
	}
	if err := os.Mkdir(localCleanPath(req.Path), 0o755); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) handleLocalCreateFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		writeError(w, http.StatusBadRequest, errors.New("missing local path"))
		return
	}
	if err := createLocalFile(req.Path); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) handleLocalDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	clean := localCleanPath(req.Path)
	if clean == "." || filepath.Dir(clean) == clean {
		writeError(w, http.StatusBadRequest, errors.New("refusing to delete root or current directory"))
		return
	}
	info, err := os.Stat(clean)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if info.IsDir() {
		if err := os.RemoveAll(clean); err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
	} else if err := os.Remove(clean); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) handleLocalOpen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	clean := localCleanPath(req.Path)
	if clean == "." || filepath.Dir(clean) == clean {
		writeError(w, http.StatusBadRequest, errors.New("invalid local path"))
		return
	}
	info, err := os.Stat(clean)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if info.IsDir() {
		writeError(w, http.StatusBadRequest, errors.New("cannot open a directory with file association"))
		return
	}
	if err := browser.OpenFile(clean); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) handleLocalOpenText(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	clean := localCleanPath(req.Path)
	if clean == "." || filepath.Dir(clean) == clean {
		writeError(w, http.StatusBadRequest, errors.New("invalid local path"))
		return
	}
	info, err := os.Stat(clean)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if info.IsDir() {
		writeError(w, http.StatusBadRequest, errors.New("cannot edit a directory"))
		return
	}
	if err := browser.OpenTextFile(clean); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) handleLocalRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req struct {
		Path    string `json:"path"`
		NewPath string `json:"new_path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	oldPath := localCleanPath(req.Path)
	newPath := localCleanPath(req.NewPath)
	if oldPath == "." || filepath.Dir(oldPath) == oldPath || newPath == "." || filepath.Dir(newPath) == newPath {
		writeError(w, http.StatusBadRequest, errors.New("invalid rename path"))
		return
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) handleLocalCopy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req struct {
		SourcePath   string `json:"source_path"`
		TargetPath   string `json:"target_path"`
		RelativePath string `json:"relative_path"`
		TransferID   string `json:"transfer_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.copyLocalFile(req.SourcePath, req.TargetPath, req.RelativePath, req.TransferID); err != nil {
		s.finishProgress(req.TransferID, err)
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.finishProgress(req.TransferID, nil)
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func listLocalDirectory(value string) (localListResult, error) {
	clean := localCleanPath(value)
	entries, err := os.ReadDir(clean)
	if err != nil {
		return localListResult{}, err
	}
	items := make([]localEntry, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		items = append(items, localEntry{
			Name:    entry.Name(),
			Path:    filepath.Join(clean, entry.Name()),
			IsDir:   info.IsDir(),
			Size:    info.Size(),
			Mode:    info.Mode().String(),
			ModTime: info.ModTime(),
		})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].IsDir != items[j].IsDir {
			return items[i].IsDir
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
	return localListResult{Path: clean, Entries: items}, nil
}

func (s *Server) copyLocalFile(sourcePath, targetDir, relativePath, transferID string) error {
	source := localCleanPath(sourcePath)
	targetBase := localCleanPath(targetDir)
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("directory transfer is not supported yet: %s", source)
	}
	targetInfo, err := os.Stat(targetBase)
	if err != nil {
		return err
	}
	if !targetInfo.IsDir() {
		return fmt.Errorf("target is not a directory: %s", targetBase)
	}
	targetRel, err := cleanLocalTransferName(relativePath, filepath.Base(source))
	if err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	target := filepath.Join(targetBase, targetRel)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	out, err := os.Create(target)
	if err != nil {
		return err
	}
	src := s.progressReader(transferID, info.Size(), in)
	if _, err := io.Copy(out, src); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func cleanLocalTransferName(value, fallback string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = fallback
	}
	clean := filepath.Clean(filepath.FromSlash(strings.ReplaceAll(value, "\\", "/")))
	if clean == "." || clean == "" || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("invalid transfer path")
	}
	return clean, nil
}

func createLocalFile(value string) error {
	clean := localCleanPath(value)
	if clean == "." || filepath.Dir(clean) == clean {
		return errors.New("invalid local file path")
	}
	if err := os.MkdirAll(filepath.Dir(clean), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(clean, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return file.Close()
}

func localCleanPath(value string) string {
	if strings.TrimSpace(value) == "" {
		path, err := defaultLocalPath()
		if err == nil && path != "" {
			return filepath.Clean(path)
		}
		return "."
	}
	return filepath.Clean(value)
}

func defaultLocalPath() (string, error) {
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		desktop := filepath.Join(home, "Desktop")
		if info, statErr := os.Stat(desktop); statErr == nil && info.IsDir() {
			return filepath.Clean(desktop), nil
		}
		return filepath.Clean(home), nil
	}
	return os.Getwd()
}
