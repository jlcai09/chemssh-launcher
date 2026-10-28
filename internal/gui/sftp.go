package gui

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"

	"chemssh-launcher/internal/browser"
	"chemssh-launcher/internal/config"
	"chemssh-launcher/internal/sftpclient"
)

type sftpSessionRequest struct {
	ID             string `json:"id"`
	Session        string `json:"session"`
	Path           string `json:"path"`
	NewPath        string `json:"new_path"`
	TransferID     string `json:"transfer_id"`
	AcceptHostKey  bool   `json:"accept_host_key"`
	ReplaceHostKey bool   `json:"replace_host_key"`
}

func (s *Server) handleSFTPConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req sftpSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	profile, err := s.rt.Profiles.Get(req.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if profile.IsLocal() {
		writeError(w, http.StatusBadRequest, errors.New("local ChemSSH profiles do not provide SFTP"))
		return
	}
	policy := hostKeyPolicyForRequest(req.AcceptHostKey, req.ReplaceHostKey)
	s.logs.add("SFTP connection requested for " + profile.Name)
	session, err := s.sftp.Connect(profile, s.rt.Secrets, policy)
	if err != nil {
		s.logs.add("SFTP connection failed for " + profile.Name + ": " + err.Error())
		writeSSHError(w, err)
		return
	}
	s.logs.add("SFTP connection OK for " + profile.Name)
	writeJSON(w, session, nil)
}

func (s *Server) handleSFTPDisconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req sftpSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.sftp.Disconnect(req.Session); err != nil {
		writeSFTPError(w, err)
		return
	}
	if !s.sftp.IsConnected(req.Session) {
		s.sftpSync.UnregisterSession(req.Session)
	}
	s.logs.add("SFTP disconnected")
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) handleSFTPReconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req sftpSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.sftp.Reconnect(req.Session); err != nil {
		s.logs.add("SFTP reconnect failed: " + err.Error())
		writeSFTPError(w, err)
		return
	}
	s.logs.add("SFTP reconnected")
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) handleSFTPList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	result, err := s.sftp.List(r.URL.Query().Get("session"), r.URL.Query().Get("path"))
	if err != nil {
		writeSFTPError(w, err)
		return
	}
	writeJSON(w, result, nil)
}

func (s *Server) handleSFTPRealPath(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	resolved, err := s.sftp.RealPath(r.URL.Query().Get("session"), r.URL.Query().Get("path"))
	if err != nil {
		writeSFTPError(w, err)
		return
	}
	writeJSON(w, map[string]string{"path": resolved}, nil)
}

func (s *Server) handleSFTPMkdir(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req sftpSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.sftp.Mkdir(req.Session, req.Path); err != nil {
		writeSFTPError(w, err)
		return
	}
	s.logs.add("SFTP mkdir: " + req.Path)
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) handleSFTPCreateFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req sftpSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.sftp.CreateFile(req.Session, req.Path); err != nil {
		writeSFTPError(w, err)
		return
	}
	s.logs.add("SFTP create file: " + req.Path)
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) handleSFTPRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req sftpSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.sftp.Rename(req.Session, req.Path, req.NewPath); err != nil {
		writeSFTPError(w, err)
		return
	}
	s.logs.add("SFTP rename: " + req.Path + " -> " + req.NewPath)
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) handleSFTPDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req sftpSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.sftp.Delete(req.Session, req.Path); err != nil {
		writeSFTPError(w, err)
		return
	}
	s.logs.add("SFTP delete: " + req.Path)
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) handleSFTPUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	sessionID := r.FormValue("session")
	remotePath := r.FormValue("path")
	uploadName := r.FormValue("relative_path")
	transferID := r.FormValue("transfer_id")
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	defer file.Close()
	if uploadName == "" {
		uploadName = header.Filename
	}
	src := s.progressReader(transferID, header.Size, file)
	if err := s.sftp.Upload(sessionID, remotePath, uploadName, src); err != nil {
		s.finishProgress(transferID, err)
		writeSFTPError(w, err)
		return
	}
	s.finishProgress(transferID, nil)
	s.logs.add("SFTP upload: " + uploadName + " -> " + remotePath)
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) handleSFTPDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	file, info, err := s.sftp.Download(r.URL.Query().Get("session"), r.URL.Query().Get("path"))
	if err != nil {
		writeSFTPError(w, err)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": info.Name}))
	buf := make([]byte, copyBufferSize)
	if _, err := io.CopyBuffer(w, file, buf); err != nil {
		s.logs.add("SFTP download failed while streaming: " + err.Error())
		return
	}
	s.logs.add("SFTP download: " + info.Name)
}

func (s *Server) handleSFTPOpen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req sftpSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	remote, info, err := s.sftp.Download(req.Session, req.Path)
	if err != nil {
		writeSFTPError(w, err)
		return
	}
	defer remote.Close()
	if err := checkSFTPOpenSyncFileSize(info.Size); err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	cachePath, err := sftpOpenCachePath(req.Session, req.Path, info.Name)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	localFile, err := os.Create(cachePath)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	buf := make([]byte, copyBufferSize)
	if _, err := io.CopyBuffer(localFile, remote, buf); err != nil {
		_ = localFile.Close()
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if err := localFile.Close(); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if err := s.sftpSync.Register(req.Session, req.Path, cachePath); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if err := browser.OpenFile(cachePath); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.logs.add("SFTP open cached file: " + req.Path + " -> " + cachePath)
	writeJSON(w, map[string]string{"path": cachePath}, nil)
}

func (s *Server) handleSFTPOpenText(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req sftpSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	cachePath, err := s.cacheSFTPFile(req.Session, req.Path)
	if err != nil {
		writeSFTPError(w, err)
		return
	}
	if err := s.sftpSync.Register(req.Session, req.Path, cachePath); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if err := browser.OpenTextFile(cachePath); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.logs.add("SFTP open text cached file: " + req.Path + " -> " + cachePath)
	writeJSON(w, map[string]string{"path": cachePath}, nil)
}

func (s *Server) handleSFTPOpenSyncEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	writeJSON(w, map[string][]sftpOpenSyncEvent{"events": s.sftpSync.EventsSince(after)}, nil)
}

func sftpOpenCachePath(sessionID, remotePath, fileName string) (string, error) {
	root, err := config.DefaultSFTPOpenCacheDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(sessionID + "\n" + remotePath))
	name := filepath.Base(fileName)
	if name == "." || name == string(filepath.Separator) || name == "" {
		name = "remote-file"
	}
	return filepath.Join(root, fmt.Sprintf("%x", sum[:8]), name), nil
}

func (s *Server) cacheSFTPFile(sessionID, remotePath string) (string, error) {
	remote, info, err := s.sftp.Download(sessionID, remotePath)
	if err != nil {
		return "", err
	}
	defer remote.Close()
	if err := checkSFTPOpenSyncFileSize(info.Size); err != nil {
		return "", err
	}
	cachePath, err := sftpOpenCachePath(sessionID, remotePath, info.Name)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		return "", err
	}
	localFile, err := os.Create(cachePath)
	if err != nil {
		return "", err
	}
	buf := make([]byte, copyBufferSize)
	if _, err := io.CopyBuffer(localFile, remote, buf); err != nil {
		_ = localFile.Close()
		return "", err
	}
	if err := localFile.Close(); err != nil {
		return "", err
	}
	return cachePath, nil
}

func (s *Server) handleSFTPUploadLocal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req struct {
		Session      string `json:"session"`
		Path         string `json:"path"`
		LocalPath    string `json:"local_path"`
		RelativePath string `json:"relative_path"`
		TransferID   string `json:"transfer_id"`
		Resume       bool   `json:"resume"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	localPath := localCleanPath(req.LocalPath)
	info, err := os.Stat(localPath)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if info.IsDir() {
		writeError(w, http.StatusBadRequest, errors.New("directory transfer is not supported yet"))
		return
	}
	uploadName := req.RelativePath
	if uploadName == "" {
		uploadName = filepath.Base(localPath)
	}
	targetPath, err := sftpclient.UploadTargetPath(req.Path, uploadName)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	src := newLocalFileSource(localPath)
	dst := &sftpFileTarget{manager: s.sftp, session: req.Session, path: targetPath}
	if err := s.runResumableTransfer(req.TransferID, req.Resume, filepath.Base(localPath), src, dst); err != nil {
		writeSFTPError(w, err)
		return
	}
	s.logs.add("SFTP upload local: " + localPath + " -> " + req.Path)
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) handleSFTPDownloadLocal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req struct {
		Session      string `json:"session"`
		Path         string `json:"path"`
		LocalPath    string `json:"local_path"`
		RelativePath string `json:"relative_path"`
		TransferID   string `json:"transfer_id"`
		Resume       bool   `json:"resume"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	targetDir := localCleanPath(req.LocalPath)
	targetInfo, err := os.Stat(targetDir)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if !targetInfo.IsDir() {
		writeError(w, http.StatusBadRequest, errors.New("local target is not a directory"))
		return
	}
	targetName, err := cleanLocalTransferName(req.RelativePath, path.Base(req.Path))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	targetPath := filepath.Join(targetDir, targetName)
	src := &sftpFileSource{manager: s.sftp, session: req.Session, path: req.Path}
	dst := newLocalFileTarget(targetPath)
	if err := s.runResumableTransfer(req.TransferID, req.Resume, targetName, src, dst); err != nil {
		writeSFTPError(w, err)
		return
	}
	s.logs.add("SFTP download local: " + req.Path + " -> " + targetDir)
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

func (s *Server) handleSFTPCopyRemote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req struct {
		SourceSession string `json:"source_session"`
		SourcePath    string `json:"source_path"`
		TargetSession string `json:"target_session"`
		TargetPath    string `json:"target_path"`
		RelativePath  string `json:"relative_path"`
		TransferID    string `json:"transfer_id"`
		Resume        bool   `json:"resume"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	uploadName := req.RelativePath
	if uploadName == "" {
		uploadName = path.Base(req.SourcePath)
	}
	targetPath, err := sftpclient.UploadTargetPath(req.TargetPath, uploadName)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	src := &sftpFileSource{manager: s.sftp, session: req.SourceSession, path: req.SourcePath}
	dst := &sftpFileTarget{manager: s.sftp, session: req.TargetSession, path: targetPath}
	if err := s.runResumableTransfer(req.TransferID, req.Resume, path.Base(req.SourcePath), src, dst); err != nil {
		writeSFTPError(w, err)
		return
	}
	s.logs.add("SFTP remote copy: " + req.SourcePath + " -> " + req.TargetPath)
	writeJSON(w, map[string]bool{"ok": true}, nil)
}

// sftpTransportLostHeader marks responses whose failure was caused by a dead
// SSH transport, so the UI knows a reconnect can recover instead of treating it
// as a permanent server-side rejection.
const sftpTransportLostHeader = "X-ChemSSH-SFTP-Transport"

func writeSFTPError(w http.ResponseWriter, err error) {
	if sftpclient.IsTransportError(err) {
		w.Header().Set(sftpTransportLostHeader, "lost")
	}
	if errors.Is(err, sftpclient.ErrSessionNotFound) {
		writeError(w, http.StatusGone, err)
		return
	}
	if errors.Is(err, errSFTPOpenFileTooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	writeError(w, http.StatusBadGateway, fmt.Errorf("SFTP operation failed: %w", err))
}
