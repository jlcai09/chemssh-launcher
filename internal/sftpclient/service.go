package sftpclient

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"chemssh-launcher/internal/config"
	"chemssh-launcher/internal/secret"
	"chemssh-launcher/internal/sshclient"
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
	pool       *connectionPool  // 使用连接池替代单个连接
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

	return m.connect(profile, secrets, policy, true)
}

func (m *Manager) ConnectDedicated(profile config.Profile, secrets secret.Store, policy sshclient.HostKeyPolicy) (Session, error) {
	return m.connect(profile, secrets, policy, false)
}

func (m *Manager) connect(profile config.Profile, secrets secret.Store, policy sshclient.HostKeyPolicy, reuseByProfile bool) (Session, error) {
	// 创建连接池
	pool, err := newConnectionPool(profile, secrets, policy)
	if err != nil {
		return Session{}, err
	}

	id, err := config.NewID()
	if err != nil {
		pool.closeAll()
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
		info: info,
		pool: pool,
		refs: 1,
	}
	m.sessions[id] = session
	if reuseByProfile {
		m.byProfile[profile.ID] = session
	}
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := session.pool.acquire(ctx)
	if err != nil {
		return ListResult{}, err
	}
	defer session.pool.release(conn)

	result, err := listDirectory(conn.client, remotePath)
	if err != nil {
		conn.failed = true
	}
	return result, err
}

func (m *Manager) Upload(id, remoteDir, fileName string, src io.Reader) error {
	session, err := m.get(id)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := session.pool.acquire(ctx)
	if err != nil {
		return err
	}
	defer session.pool.release(conn)

	err = uploadFile(conn.client, remoteDir, fileName, src)
	if err != nil {
		conn.failed = true
	}
	return err
}

func (m *Manager) UploadFile(id, remotePath string, src io.Reader) error {
	session, err := m.get(id)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := session.pool.acquire(ctx)
	if err != nil {
		return err
	}
	defer session.pool.release(conn)

	err = uploadFilePath(conn.client, remotePath, src)
	if err != nil {
		conn.failed = true
	}
	return err
}

func (m *Manager) Download(id, remotePath string) (io.ReadCloser, FileInfo, error) {
	session, err := m.get(id)
	if err != nil {
		return nil, FileInfo{}, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := session.pool.acquire(ctx)
	if err != nil {
		return nil, FileInfo{}, err
	}
	defer session.pool.release(conn)

	reader, info, err := downloadFile(conn.client, remotePath)
	if err != nil {
		conn.failed = true
	}
	return reader, info, err
}

func (m *Manager) Mkdir(id, remotePath string) error {
	session, err := m.get(id)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := session.pool.acquire(ctx)
	if err != nil {
		return err
	}
	defer session.pool.release(conn)

	err = makeDirectory(conn.client, remotePath)
	if err != nil {
		conn.failed = true
	}
	return err
}

func (m *Manager) CreateFile(id, remotePath string) error {
	session, err := m.get(id)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := session.pool.acquire(ctx)
	if err != nil {
		return err
	}
	defer session.pool.release(conn)

	err = createFile(conn.client, remotePath)
	if err != nil {
		conn.failed = true
	}
	return err
}

func (m *Manager) Rename(id, oldPath, newPath string) error {
	session, err := m.get(id)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := session.pool.acquire(ctx)
	if err != nil {
		return err
	}
	defer session.pool.release(conn)

	err = renamePath(conn.client, oldPath, newPath)
	if err != nil {
		conn.failed = true
	}
	return err
}

func (m *Manager) Delete(id, remotePath string) error {
	session, err := m.get(id)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := session.pool.acquire(ctx)
	if err != nil {
		return err
	}
	defer session.pool.release(conn)

	err = deletePath(conn.client, remotePath)
	if err != nil {
		conn.failed = true
	}
	return err
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
	if m.byProfile[session.info.ProfileID] == session {
		delete(m.byProfile, session.info.ProfileID)
	}
	return session, true, nil
}

func (s *activeSession) close() error {
	if s.pool != nil {
		s.pool.closeAll()
	}
	return nil
}
