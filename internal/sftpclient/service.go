package sftpclient

import (
	"errors"
	"io"
	"sync"
	"time"

	"chemssh-launcher/internal/config"
	"chemssh-launcher/internal/secret"
	"chemssh-launcher/internal/sshclient"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

var ErrSessionNotFound = errors.New("SFTP session is not connected")

type Session struct {
	ID        string    `json:"id"`
	ProfileID string    `json:"profile_id"`
	Profile   string    `json:"profile"`
	Path      string    `json:"path"`
	CreatedAt time.Time `json:"created_at"`
}

type Manager struct {
	mu        sync.Mutex
	sessions  map[string]*activeSession
	byProfile map[string]*activeSession
}

type activeSession struct {
	info       Session
	sshClient  *ssh.Client
	sftpClient *sftp.Client
	refs       int
}

func NewManager() *Manager {
	return &Manager{sessions: map[string]*activeSession{}, byProfile: map[string]*activeSession{}}
}

func (m *Manager) Connect(profile config.Profile, secrets secret.Store, policy sshclient.HostKeyPolicy) (Session, error) {
	m.mu.Lock()
	existing := m.byProfile[profile.ID]
	if existing != nil {
		existing.refs++
		info := existing.info
		m.mu.Unlock()
		return info, nil
	}
	m.mu.Unlock()

	sshClient, err := sshclient.DialWithHostKeyPolicy(profile, secrets, policy)
	if err != nil {
		return Session{}, err
	}
	sftpClient, err := sftp.NewClient(sshClient)
	if err != nil {
		_ = sshClient.Close()
		return Session{}, err
	}
	id, err := config.NewID()
	if err != nil {
		_ = sftpClient.Close()
		_ = sshClient.Close()
		return Session{}, err
	}
	info := Session{
		ID:        id,
		ProfileID: profile.ID,
		Profile:   profile.Name,
		Path:      ".",
		CreatedAt: time.Now(),
	}
	m.mu.Lock()
	session := &activeSession{
		info:       info,
		sshClient:  sshClient,
		sftpClient: sftpClient,
		refs:       1,
	}
	m.sessions[id] = session
	m.byProfile[profile.ID] = session
	m.mu.Unlock()
	return info, nil
}

func (m *Manager) Disconnect(id string) error {
	session, closeNow, err := m.release(id)
	if err != nil || !closeNow {
		return err
	}
	return session.close()
}

func (m *Manager) CloseAll() {
	m.mu.Lock()
	sessions := m.sessions
	m.sessions = map[string]*activeSession{}
	m.byProfile = map[string]*activeSession{}
	m.mu.Unlock()
	for _, session := range sessions {
		_ = session.close()
	}
}

func (m *Manager) List(id, remotePath string) (ListResult, error) {
	session, err := m.get(id)
	if err != nil {
		return ListResult{}, err
	}
	return listDirectory(session.sftpClient, remotePath)
}

func (m *Manager) Upload(id, remoteDir, fileName string, src io.Reader) error {
	session, err := m.get(id)
	if err != nil {
		return err
	}
	return uploadFile(session.sftpClient, remoteDir, fileName, src)
}

func (m *Manager) UploadFile(id, remotePath string, src io.Reader) error {
	session, err := m.get(id)
	if err != nil {
		return err
	}
	return uploadFilePath(session.sftpClient, remotePath, src)
}

func (m *Manager) Download(id, remotePath string) (io.ReadCloser, FileInfo, error) {
	session, err := m.get(id)
	if err != nil {
		return nil, FileInfo{}, err
	}
	return downloadFile(session.sftpClient, remotePath)
}

func (m *Manager) Mkdir(id, remotePath string) error {
	session, err := m.get(id)
	if err != nil {
		return err
	}
	return makeDirectory(session.sftpClient, remotePath)
}

func (m *Manager) CreateFile(id, remotePath string) error {
	session, err := m.get(id)
	if err != nil {
		return err
	}
	return createFile(session.sftpClient, remotePath)
}

func (m *Manager) Rename(id, oldPath, newPath string) error {
	session, err := m.get(id)
	if err != nil {
		return err
	}
	return renamePath(session.sftpClient, oldPath, newPath)
}

func (m *Manager) Delete(id, remotePath string) error {
	session, err := m.get(id)
	if err != nil {
		return err
	}
	return deletePath(session.sftpClient, remotePath)
}

func (m *Manager) get(id string) (*activeSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[id]
	if session == nil {
		return nil, ErrSessionNotFound
	}
	return session, nil
}

func (m *Manager) IsConnected(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id] != nil
}

func (m *Manager) release(id string) (*activeSession, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[id]
	if session == nil {
		return nil, false, ErrSessionNotFound
	}
	session.refs--
	if session.refs > 0 {
		return session, false, nil
	}
	delete(m.sessions, id)
	delete(m.byProfile, session.info.ProfileID)
	return session, true, nil
}

func (s *activeSession) close() error {
	var firstErr error
	if s.sftpClient != nil {
		if err := s.sftpClient.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.sshClient != nil {
		if err := s.sshClient.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
