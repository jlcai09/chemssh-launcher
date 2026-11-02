package gui

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"chemssh-launcher/internal/archive"
)

// archiveProbeTTL keeps `command -v` probes rare while still letting a user who
// installs a tool see it without restarting the launcher.
const archiveProbeTTL = 60 * time.Second

// remoteToolProbeScript prints the archive utilities present on the host.
const remoteToolProbeScript = `for t in tar zstd gzip xz; do command -v "$t" >/dev/null 2>&1 && echo "$t"; done`

type archiveProbeEntry struct {
	tools archive.Tools
	at    time.Time
}

// archiveToolCache memoizes per-host capability probes.
type archiveToolCache struct {
	mu      sync.Mutex
	entries map[string]archiveProbeEntry
}

func newArchiveToolCache() *archiveToolCache {
	return &archiveToolCache{entries: map[string]archiveProbeEntry{}}
}

func (c *archiveToolCache) get(key string) (archive.Tools, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || time.Since(entry.at) > archiveProbeTTL {
		return archive.Tools{}, false
	}
	return entry.tools, true
}

func (c *archiveToolCache) set(key string, tools archive.Tools) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = archiveProbeEntry{tools: tools, at: time.Now()}
}

// archiveToolsPayload mirrors the ChemSSH file manager so both UIs can share the
// same response shape.
type archiveToolsPayload struct {
	archive.Tools
	Formats []string `json:"formats"`
}

type archiveCompressRequest struct {
	Session         string   `json:"session"`
	TargetDirectory string   `json:"target_directory"`
	Paths           []string `json:"paths"`
	Format          string   `json:"format"`
}

type archiveExtractRequest struct {
	Session         string `json:"session"`
	Path            string `json:"path"`
	TargetDirectory string `json:"target_directory"`
}

type localArchiveCompressRequest struct {
	TargetDirectory string   `json:"target_directory"`
	Paths           []string `json:"paths"`
	Format          string   `json:"format"`
}

type localArchiveExtractRequest struct {
	Path            string `json:"path"`
	TargetDirectory string `json:"target_directory"`
}

func (s *Server) probeRemoteArchiveTools(sessionID string) (archive.Tools, error) {
	if tools, ok := s.archiveTools.get(sessionID); ok {
		return tools, nil
	}
	output, err := s.sftp.RunCommand(sessionID, ".", []string{"sh", "-c", remoteToolProbeScript})
	if err != nil {
		return archive.Tools{}, err
	}
	tools := archive.Tools{}
	for _, line := range strings.Split(output, "\n") {
		switch strings.TrimSpace(line) {
		case "tar":
			tools.Tar = true
		case "zstd":
			tools.Zstd = true
		case "gzip":
			tools.Gzip = true
		case "xz":
			tools.Xz = true
		}
	}
	s.archiveTools.set(sessionID, tools)
	return tools, nil
}

func (s *Server) probeLocalArchiveTools() archive.Tools {
	const key = "local"
	if tools, ok := s.archiveTools.get(key); ok {
		return tools
	}
	tools := archive.Tools{
		Tar:  hasLocalTool("tar"),
		Zstd: hasLocalTool("zstd"),
		Gzip: hasLocalTool("gzip"),
		Xz:   hasLocalTool("xz"),
	}
	s.archiveTools.set(key, tools)
	return tools
}

func hasLocalTool(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func newArchiveToken() string {
	var data [6]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "tmp"
	}
	return hex.EncodeToString(data[:])
}

// runRemoteArchivePlan executes every step in `dir` and always removes the
// intermediate tarballs, even when a step fails.
func (s *Server) runRemoteArchivePlan(sessionID, dir string, plan archive.Plan) error {
	var runErr error
	for _, step := range plan.Steps {
		if _, err := s.sftp.RunCommand(sessionID, dir, step.Argv); err != nil {
			runErr = fmt.Errorf("%s: %w", step.Argv[0], err)
			break
		}
	}
	for _, leftover := range plan.CleanupPaths() {
		_ = s.sftp.Delete(sessionID, remoteJoin(dir, leftover))
	}
	return runErr
}

// runLocalArchivePlan is runRemoteArchivePlan for the local pane.
func runLocalArchivePlan(dir string, plan archive.Plan) error {
	var runErr error
	for _, step := range plan.Steps {
		cmd := exec.Command(step.Argv[0], step.Argv[1:]...)
		cmd.Dir = dir
		if output, err := cmd.CombinedOutput(); err != nil {
			detail := strings.TrimSpace(string(output))
			if detail == "" {
				detail = err.Error()
			}
			runErr = fmt.Errorf("%s: %s", step.Argv[0], firstLine(detail))
			break
		}
	}
	for _, leftover := range plan.CleanupPaths() {
		_ = os.Remove(filepath.Join(dir, leftover))
	}
	return runErr
}

func firstLine(value string) string {
	if index := strings.IndexByte(value, '\n'); index >= 0 {
		value = value[:index]
	}
	if len(value) > 300 {
		value = value[:300]
	}
	return value
}

func remoteJoin(dir, name string) string {
	if dir == "" || dir == "." {
		return name
	}
	if strings.HasSuffix(dir, "/") {
		return dir + name
	}
	return dir + "/" + name
}

// remoteArchiveNames validates that every selected path sits directly inside
// `dir` and returns the names tar should pack.
func remoteArchiveNames(paths []string, dir string) ([]string, error) {
	cleanDir := strings.TrimSuffix(dir, "/")
	if cleanDir == "" {
		cleanDir = "/"
	}
	names := make([]string, 0, len(paths))
	seen := map[string]bool{}
	for _, rawPath := range paths {
		cleanPath := strings.TrimSuffix(rawPath, "/")
		if err := validateArchiveFileName(path.Base(cleanPath)); err != nil {
			return nil, err
		}
		if path.Dir(cleanPath) != cleanDir {
			return nil, fmt.Errorf("only entries of the current folder can be compressed: %s", rawPath)
		}
		name := path.Base(cleanPath)
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil, errors.New("no files to compress")
	}
	return names, nil
}

func localArchiveNames(paths []string, dir string) ([]string, error) {
	cleanDir := filepath.Clean(dir)
	names := make([]string, 0, len(paths))
	seen := map[string]bool{}
	for _, rawPath := range paths {
		cleanPath := filepath.Clean(rawPath)
		name := filepath.Base(cleanPath)
		if err := validateArchiveFileName(name); err != nil {
			return nil, err
		}
		if !strings.EqualFold(filepath.Dir(cleanPath), cleanDir) {
			return nil, fmt.Errorf("only entries of the current folder can be compressed: %s", rawPath)
		}
		key := strings.ToLower(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil, errors.New("no files to compress")
	}
	return names, nil
}

func validateArchiveFileName(name string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("invalid entry name: %q", name)
	}
	if strings.ContainsAny(name, "\r\n\x00") {
		return errors.New("entry name contains control characters")
	}
	return nil
}

func (s *Server) handleSFTPArchiveTools(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, errors.New("missing session"))
		return
	}
	tools, err := s.probeRemoteArchiveTools(sessionID)
	if err != nil {
		writeSFTPError(w, err)
		return
	}
	writeJSON(w, archiveToolsPayload{Tools: tools, Formats: archive.Formats(tools)}, nil)
}

func (s *Server) handleSFTPCompress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req archiveCompressRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Session == "" || req.TargetDirectory == "" {
		writeError(w, http.StatusBadRequest, errors.New("missing session or target directory"))
		return
	}
	names, err := remoteArchiveNames(req.Paths, req.TargetDirectory)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	tools, err := s.probeRemoteArchiveTools(req.Session)
	if err != nil {
		writeSFTPError(w, err)
		return
	}
	listing, err := s.sftp.List(req.Session, req.TargetDirectory)
	if err != nil {
		writeSFTPError(w, err)
		return
	}
	taken := map[string]bool{}
	for _, entry := range listing.Entries {
		taken[entry.Name] = true
	}
	archiveName, err := archive.NextArchiveName(req.Format, func(name string) bool { return taken[name] })
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	plan, err := archive.CompressPlan(tools, req.Format, archiveName, names, newArchiveToken())
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.runRemoteArchivePlan(req.Session, req.TargetDirectory, plan); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.logs.add("SFTP archive: " + remoteJoin(req.TargetDirectory, archiveName))
	writeJSON(w, map[string]any{"ok": true, "archive": archiveName}, nil)
}

func (s *Server) handleSFTPExtract(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req archiveExtractRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Session == "" || req.Path == "" {
		writeError(w, http.StatusBadRequest, errors.New("missing session or path"))
		return
	}
	target := strings.TrimSuffix(req.TargetDirectory, "/")
	if target == "" {
		target = path.Dir(strings.TrimSuffix(req.Path, "/"))
	}
	tools, err := s.probeRemoteArchiveTools(req.Session)
	if err != nil {
		writeSFTPError(w, err)
		return
	}
	plan, ok := archive.ExtractPlan(path.Base(strings.TrimSuffix(req.Path, "/")), tools, req.Path, newArchiveToken())
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("no extractor is available for this file"))
		return
	}
	if err := s.runRemoteArchivePlan(req.Session, target, plan); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.logs.add("SFTP extract: " + req.Path)
	writeJSON(w, map[string]any{"ok": true, "path": target}, nil)
}

func (s *Server) handleLocalArchiveTools(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	tools := s.probeLocalArchiveTools()
	writeJSON(w, archiveToolsPayload{Tools: tools, Formats: archive.Formats(tools)}, nil)
}

func (s *Server) handleLocalCompress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req localArchiveCompressRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.TargetDirectory == "" {
		writeError(w, http.StatusBadRequest, errors.New("missing target directory"))
		return
	}
	names, err := localArchiveNames(req.Paths, req.TargetDirectory)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	taken := map[string]bool{}
	if entries, readErr := os.ReadDir(req.TargetDirectory); readErr == nil {
		for _, entry := range entries {
			taken[entry.Name()] = true
		}
	}
	archiveName, err := archive.NextArchiveName(req.Format, func(name string) bool { return taken[name] })
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	plan, err := archive.CompressPlan(s.probeLocalArchiveTools(), req.Format, archiveName, names, newArchiveToken())
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := runLocalArchivePlan(req.TargetDirectory, plan); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.localLog.add("Local archive: " + filepath.Join(req.TargetDirectory, archiveName))
	writeJSON(w, map[string]any{"ok": true, "archive": archiveName}, nil)
}

func (s *Server) handleLocalExtract(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req localArchiveExtractRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, errors.New("missing path"))
		return
	}
	target := req.TargetDirectory
	if target == "" {
		target = filepath.Dir(req.Path)
	}
	plan, ok := archive.ExtractPlan(filepath.Base(req.Path), s.probeLocalArchiveTools(), req.Path, newArchiveToken())
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("no extractor is available for this file"))
		return
	}
	if err := runLocalArchivePlan(target, plan); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.localLog.add("Local extract: " + req.Path)
	writeJSON(w, map[string]any{"ok": true, "path": target}, nil)
}
