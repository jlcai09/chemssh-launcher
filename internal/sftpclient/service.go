package sftpclient

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"sync"
	"time"

	"chemssh-launcher/internal/config"
	"chemssh-launcher/internal/secret"
	"chemssh-launcher/internal/sshclient"

	"github.com/pkg/sftp"
)

var ErrSessionNotFound = errors.New("SFTP session is not connected")

// sftpOperationTimeout bounds how long an operation may wait for a pooled
// connection before reporting failure.
const sftpOperationTimeout = 30 * time.Second

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
	info Session
	pool *connectionPool // 使用连接池替代单个连接
	refs int
}

func NewManager() *Manager {
	return &Manager{sessions: map[string]*activeSession{}, byProfile: map[string]*activeSession{}}
}

// Connect returns a session for the profile, reusing an existing one only when
// its transport still works. A cached session whose SSH connection died during
// a network drop is replaced instead of being handed back, which is what made
// reconnecting appear to do nothing after the link came back.
func (m *Manager) Connect(profile config.Profile, secrets secret.Store, policy sshclient.HostKeyPolicy) (Session, error) {
	m.mu.Lock()
	existing := m.byProfile[profile.ID]
	if existing == nil {
		m.mu.Unlock()
		return m.connect(profile, secrets, policy, true)
	}
	info := existing.info
	m.mu.Unlock()

	if err := existing.pool.reconnect(); err == nil || errors.Is(err, errReconnectCooldown) {
		// A cooldown means another attempt just failed moments ago; keep the
		// session so a concurrent open does not tear down a pool that a peer is
		// still using.
		m.mu.Lock()
		if m.sessions[info.ID] == existing {
			existing.refs++
			m.mu.Unlock()
			return info, nil
		}
		m.mu.Unlock()
		return m.connect(profile, secrets, policy, true)
	}

	// The cached session cannot recover; drop it so the caller gets a fresh
	// transport instead of a permanently dead pool.
	m.discard(existing)
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

// discard unregisters a session that can no longer serve requests and closes
// its transport.
func (m *Manager) discard(session *activeSession) {
	if session == nil {
		return
	}
	m.mu.Lock()
	if m.sessions[session.info.ID] == session {
		delete(m.sessions, session.info.ID)
	}
	if m.byProfile[session.info.ProfileID] == session {
		delete(m.byProfile, session.info.ProfileID)
	}
	session.refs = 0
	m.mu.Unlock()
	_ = session.close()
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
	return withConn(m, id, func(client *sftp.Client) (ListResult, error) {
		return listDirectory(client, remotePath)
	})
}

func (m *Manager) RealPath(id, remotePath string) (string, error) {
	return withConn(m, id, func(client *sftp.Client) (string, error) {
		return client.RealPath(CleanPath(remotePath))
	})
}

func (m *Manager) Upload(id, remoteDir, fileName string, src io.Reader) error {
	_, err := withConn(m, id, func(client *sftp.Client) (struct{}, error) {
		return struct{}{}, uploadFile(client, remoteDir, fileName, src)
	})
	return err
}

func (m *Manager) UploadFile(id, remotePath string, src io.Reader) error {
	_, err := withConn(m, id, func(client *sftp.Client) (struct{}, error) {
		return struct{}{}, uploadFilePath(client, remotePath, src)
	})
	return err
}

func (m *Manager) Download(id, remotePath string) (io.ReadCloser, FileInfo, error) {
	return m.openReadStream(id, func(client *sftp.Client) (*sftp.File, FileInfo, error) {
		return downloadFile(client, remotePath)
	})
}

func (m *Manager) Reconnect(id string) error {
	session, err := m.get(id)
	if err != nil {
		return err
	}
	return session.pool.reconnect()
}

func (m *Manager) Stat(id, remotePath string) (FileInfo, error) {
	return withConn(m, id, func(client *sftp.Client) (FileInfo, error) {
		info, err := client.Stat(CleanPath(remotePath))
		if err != nil {
			return FileInfo{}, err
		}
		return FileInfo{Name: info.Name(), Size: info.Size(), ModTime: info.ModTime()}, nil
	})
}

func (m *Manager) OpenReadAt(id, remotePath string, offset int64) (io.ReadCloser, FileInfo, error) {
	return m.openReadStream(id, func(client *sftp.Client) (*sftp.File, FileInfo, error) {
		return downloadFileAt(client, remotePath, offset)
	})
}

func (m *Manager) OpenWriteAt(id, remotePath string, offset int64) (io.WriteCloser, error) {
	session, err := m.get(id)
	if err != nil {
		return nil, err
	}

	for attempt := 0; ; attempt++ {
		conn, err := session.pool.acquireWithTimeout()
		if err != nil {
			if attempt == 0 && isTransportError(err) {
				if reconnectErr := session.pool.reconnect(); reconnectErr == nil {
					continue
				}
			}
			return nil, err
		}

		writer, opErr := uploadFileAt(conn.client, remotePath, offset)
		if opErr == nil {
			return &pooledWriteCloser{WriteCloser: writer, pool: session.pool, conn: conn}, nil
		}
		session.pool.markFailed(conn)
		session.pool.release(conn)
		if attempt == 0 && isTransportError(opErr) {
			if reconnectErr := session.pool.reconnect(); reconnectErr == nil {
				continue
			}
		}
		return nil, opErr
	}
}

func (m *Manager) Mkdir(id, remotePath string) error {
	_, err := withConn(m, id, func(client *sftp.Client) (struct{}, error) {
		return struct{}{}, makeDirectory(client, remotePath)
	})
	return err
}

func (m *Manager) CreateFile(id, remotePath string) error {
	_, err := withConn(m, id, func(client *sftp.Client) (struct{}, error) {
		return struct{}{}, createFile(client, remotePath)
	})
	return err
}

func (m *Manager) Rename(id, oldPath, newPath string) error {
	_, err := withConn(m, id, func(client *sftp.Client) (struct{}, error) {
		return struct{}{}, renamePath(client, oldPath, newPath)
	})
	return err
}

func (m *Manager) Delete(id, remotePath string) error {
	_, err := withConn(m, id, func(client *sftp.Client) (struct{}, error) {
		return struct{}{}, deletePath(client, remotePath)
	})
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

// withConn acquires a connection, runs fn, and retries once after rebuilding
// the transport when the failure looks like a broken connection rather than a
// server-side rejection. Directory browsing therefore recovers on its own after
// a network drop instead of failing until the tab is closed and reopened.
func withConn[T any](m *Manager, id string, fn func(*sftp.Client) (T, error)) (T, error) {
	var zero T
	session, err := m.get(id)
	if err != nil {
		return zero, err
	}

	for attempt := 0; ; attempt++ {
		conn, err := session.pool.acquireWithTimeout()
		if err != nil {
			if attempt == 0 && isTransportError(err) {
				if reconnectErr := session.pool.reconnect(); reconnectErr == nil {
					continue
				}
			}
			return zero, err
		}

		value, opErr := fn(conn.client)
		if opErr == nil {
			session.pool.release(conn)
			return value, nil
		}

		session.pool.markFailed(conn)
		session.pool.release(conn)
		if attempt == 0 && isTransportError(opErr) {
			if reconnectErr := session.pool.reconnect(); reconnectErr == nil {
				continue
			}
		}
		return zero, opErr
	}
}

// openReadStream is withConn for operations that hand a streaming reader to the
// caller: the connection stays checked out until the reader is closed.
func (m *Manager) openReadStream(id string, open func(*sftp.Client) (*sftp.File, FileInfo, error)) (io.ReadCloser, FileInfo, error) {
	session, err := m.get(id)
	if err != nil {
		return nil, FileInfo{}, err
	}

	for attempt := 0; ; attempt++ {
		conn, err := session.pool.acquireWithTimeout()
		if err != nil {
			if attempt == 0 && isTransportError(err) {
				if reconnectErr := session.pool.reconnect(); reconnectErr == nil {
					continue
				}
			}
			return nil, FileInfo{}, err
		}

		reader, info, opErr := open(conn.client)
		if opErr == nil {
			return &pooledReadCloser{ReadCloser: reader, pool: session.pool, conn: conn}, info, nil
		}
		session.pool.markFailed(conn)
		session.pool.release(conn)
		if attempt == 0 && isTransportError(opErr) {
			if reconnectErr := session.pool.reconnect(); reconnectErr == nil {
				continue
			}
		}
		return nil, FileInfo{}, opErr
	}
}

// acquireWithTimeout bounds the wait for a pooled connection.
func (p *connectionPool) acquireWithTimeout() (*pooledConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sftpOperationTimeout)
	defer cancel()
	return p.acquire(ctx)
}

// IsTransportError reports whether err indicates a broken SSH transport that a
// reconnect can fix. Callers use it to tell "the link died" apart from "the
// server refused this operation".
func IsTransportError(err error) bool {
	return isTransportError(err)
}

// isTransportError reports whether err points at a broken SSH/SFTP transport
// that a reconnect can fix. Server-side SFTP status errors and local filesystem
// errors are answered by a healthy channel, so they are not retried.
func isTransportError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errPoolClosed) || errors.Is(err, ErrSessionNotFound) || errors.Is(err, context.Canceled) {
		return false
	}
	var status *sftp.StatusError
	if errors.As(err, &status) {
		return false
	}
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		return false
	}
	return true
}
