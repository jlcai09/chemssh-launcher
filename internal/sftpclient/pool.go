package sftpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"chemssh-launcher/internal/config"
	"chemssh-launcher/internal/secret"
	"chemssh-launcher/internal/sshclient"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

const (
	poolMinSize         = 2                // 最小连接数
	poolMaxSize         = 3                // 最大连接数
	healthCheckInterval = 30 * time.Second // 健康检查间隔
	idleTimeout         = 5 * time.Minute  // 空闲超时
)

// connectionPool manages a pool of SFTP connections for a single profile
type connectionPool struct {
	profile     config.Profile
	secrets     secret.Store
	policy      sshclient.HostKeyPolicy
	mu          sync.Mutex
	reconnectMu sync.Mutex                  // serializes reconnect calls without holding mu while dialing
	sshClient   *ssh.Client                 // 共享的 SSH 连接
	dial        func() (*ssh.Client, error) // establishes the shared SSH connection; replaceable in tests
	conns       []*pooledConn               // 连接池
	available   chan *pooledConn            // 可用连接队列
	refs        int                         // 引用计数
	closed      bool
	healthStop  context.CancelFunc
}

// pooledConn represents a single SFTP connection in the pool
type pooledConn struct {
	client   *sftp.Client
	lastUsed time.Time
	inUse    bool
	failed   bool
	retired  bool
}

// pooledReadCloser wraps an io.ReadCloser and releases the pool connection when closed.
// This is necessary because Download() returns a streaming reader that outlives the
// function call; releasing the connection too early would allow another goroutine to
// reuse the same SFTP client concurrently, causing protocol-level data corruption.
type pooledReadCloser struct {
	io.ReadCloser
	pool *connectionPool
	conn *pooledConn
	once sync.Once
}

func (p *pooledReadCloser) Read(b []byte) (int, error) {
	n, err := p.ReadCloser.Read(b)
	if err != nil && !errors.Is(err, io.EOF) {
		p.pool.markFailed(p.conn)
	}
	return n, err
}

func (p *pooledReadCloser) Close() error {
	err := p.ReadCloser.Close()
	if err != nil {
		// A failed close leaves the channel in an unknown state, so never
		// return it to the pool.
		p.pool.markFailed(p.conn)
	}
	p.once.Do(func() {
		p.conn.lastUsed = time.Now()
		p.pool.release(p.conn)
	})
	return err
}

type pooledWriteCloser struct {
	io.WriteCloser
	pool *connectionPool
	conn *pooledConn
	once sync.Once
}

func (p *pooledWriteCloser) Write(b []byte) (int, error) {
	n, err := p.WriteCloser.Write(b)
	if err != nil {
		p.pool.markFailed(p.conn)
	}
	return n, err
}

func (p *pooledWriteCloser) Close() error {
	err := p.WriteCloser.Close()
	if err != nil {
		// A failed close leaves the channel in an unknown state, so never
		// return it to the pool.
		p.pool.markFailed(p.conn)
	}
	p.once.Do(func() {
		p.conn.lastUsed = time.Now()
		p.pool.release(p.conn)
	})
	return err
}

// markFailed flags a connection so release() removes it instead of returning
// it to the available queue.
func (p *connectionPool) markFailed(conn *pooledConn) {
	p.mu.Lock()
	conn.failed = true
	p.mu.Unlock()
}

// newConnectionPool creates a new connection pool for the given profile
func newConnectionPool(profile config.Profile, secrets secret.Store, policy sshclient.HostKeyPolicy) (*connectionPool, error) {
	// 建立 SSH 连接
	sshClient, err := sshclient.DialWithHostKeyPolicy(profile, secrets, policy)
	if err != nil {
		return nil, fmt.Errorf("failed to establish SSH connection: %w", err)
	}

	pool := &connectionPool{
		profile:   profile,
		secrets:   secrets,
		policy:    policy,
		sshClient: sshClient,
		conns:     make([]*pooledConn, 0, poolMaxSize),
		available: make(chan *pooledConn, poolMaxSize),
		refs:      1,
	}
	pool.dial = func() (*ssh.Client, error) {
		return sshclient.DialWithHostKeyPolicy(pool.profile, pool.secrets, pool.policy)
	}

	// 预创建最小数量的连接
	for i := 0; i < poolMinSize; i++ {
		conn, err := pool.createConnection()
		if err != nil {
			pool.closeAll()
			return nil, fmt.Errorf("failed to create initial connection %d: %w", i+1, err)
		}
		pool.conns = append(pool.conns, conn)
		pool.available <- conn
	}

	// 启动健康检查
	healthCtx, healthStop := context.WithCancel(context.Background())
	pool.healthStop = healthStop
	go pool.healthCheckLoop(healthCtx)

	return pool, nil
}

// newPooledConn opens an SFTP client over the given SSH connection without
// touching pool state, so callers can dial outside the pool lock.
func newPooledConn(sshClient *ssh.Client) (*pooledConn, error) {
	if sshClient == nil {
		return nil, errors.New("SSH connection is not available")
	}
	// 复用同一个 SSH 连接创建新的 SFTP Client
	sftpClient, err := sftp.NewClient(sshClient)
	if err != nil {
		return nil, err
	}

	return &pooledConn{
		client:   sftpClient,
		lastUsed: time.Now(),
		inUse:    false,
		failed:   false,
	}, nil
}

// createConnection creates a new SFTP connection using the shared SSH client
func (p *connectionPool) createConnection() (*pooledConn, error) {
	return newPooledConn(p.sshClient)
}

// acquire gets an available connection from the pool
func (p *connectionPool) acquire(ctx context.Context) (*pooledConn, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errors.New("connection pool is closed")
	}
	p.mu.Unlock()

	// 尝试从可用队列获取连接
	select {
	case conn := <-p.available:
		p.mu.Lock()
		conn.inUse = true
		conn.lastUsed = time.Now()
		p.mu.Unlock()
		return conn, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		// 没有可用连接，尝试创建新连接
		p.mu.Lock()
		if len(p.conns) < poolMaxSize {
			conn, err := p.createConnection()
			if err != nil {
				p.mu.Unlock()
				return nil, err
			}
			p.conns = append(p.conns, conn)
			conn.inUse = true
			conn.lastUsed = time.Now()
			p.mu.Unlock()
			return conn, nil
		}
		p.mu.Unlock()

		// 已达到最大连接数，等待可用连接
		select {
		case conn := <-p.available:
			p.mu.Lock()
			conn.inUse = true
			conn.lastUsed = time.Now()
			p.mu.Unlock()
			return conn, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// release returns a connection to the pool
func (p *connectionPool) release(conn *pooledConn) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return
	}

	if conn.retired {
		_ = conn.client.Close()
		return
	}

	conn.inUse = false
	conn.lastUsed = time.Now()

	// 如果连接失败，不放回池中
	if conn.failed {
		p.removeConnection(conn)
		return
	}

	// 放回可用队列
	select {
	case p.available <- conn:
	default:
		// 队列已满，关闭连接
		p.removeConnection(conn)
	}
}

// removeConnection removes and closes a connection from the pool.
// The caller must hold p.mu. Idle connections live in the available queue, so
// dropping one must also purge that entry; otherwise acquire() hands out a
// closed client and the queue keeps losing slots to dead connections.
func (p *connectionPool) removeConnection(conn *pooledConn) {
	// 从连接列表中移除
	for i, c := range p.conns {
		if c == conn {
			p.conns = append(p.conns[:i], p.conns[i+1:]...)
			break
		}
	}
	// 从可用队列中移除
	kept := make([]*pooledConn, 0, cap(p.available))
	for {
		select {
		case queued := <-p.available:
			if queued != conn {
				kept = append(kept, queued)
			}
			continue
		default:
		}
		break
	}
	for _, queued := range kept {
		p.available <- queued
	}
	// 关闭 SFTP 客户端
	_ = conn.client.Close()
}

// offerLocked returns an idle connection to the available queue. The caller
// must hold p.mu. A full queue means the pool is already saturated, so the
// connection is closed instead of blocking every other pool operation.
func (p *connectionPool) offerLocked(conn *pooledConn) {
	select {
	case p.available <- conn:
	default:
		p.removeConnection(conn)
	}
}

// healthCheckLoop periodically checks connection health
func (p *connectionPool) healthCheckLoop(ctx context.Context) {
	ticker := time.NewTicker(healthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.healthCheck()
		}
	}
}

// healthCheck checks and maintains connection health
func (p *connectionPool) healthCheck() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return
	}

	now := time.Now()

	// Collect connections to remove in a first pass to avoid mutating the slice
	// while iterating over it. Modifying p.conns during a range loop causes
	// elements to shift and be skipped.
	var toRemove []*pooledConn
	for _, conn := range p.conns {
		if conn.inUse {
			continue
		}

		// 关闭空闲超时的连接
		if now.Sub(conn.lastUsed) > idleTimeout && len(p.conns)-len(toRemove) > poolMinSize {
			toRemove = append(toRemove, conn)
			continue
		}

		// 健康检查
		if !p.isConnectionAlive(conn) {
			conn.failed = true
			toRemove = append(toRemove, conn)
		}
	}

	// Remove all unhealthy/idle connections after the iteration completes.
	for _, conn := range toRemove {
		p.removeConnection(conn)
	}

	// Replenish if the pool dropped below the minimum size.
	for len(p.conns) < poolMinSize {
		newConn, err := p.createConnection()
		if err != nil {
			break
		}
		p.conns = append(p.conns, newConn)
		p.offerLocked(newConn)
	}
}

// isConnectionAlive checks if a connection is still alive
func (p *connectionPool) isConnectionAlive(conn *pooledConn) bool {
	_, err := conn.client.Stat(".")
	return err == nil
}

// reconnect replaces the shared SSH connection and every pooled SFTP client
// when the transport is no longer usable. Healthy pools are left untouched, so
// callers can invoke it after any transfer failure without disrupting peers.
// Dialing happens outside p.mu so acquire/release/healthCheck keep working
// while a new transport is being established.
func (p *connectionPool) reconnect() error {
	// Serialize reconnects: two concurrent calls could otherwise interleave
	// teardown, dial, and install and leak one of the new SSH clients.
	p.reconnectMu.Lock()
	defer p.reconnectMu.Unlock()

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return errors.New("connection pool is closed")
	}
	sshClient := p.sshClient
	p.mu.Unlock()

	// The probe opens a fresh channel on the existing SSH client. Keep it out
	// of the pool lock so a hung probe cannot stall the whole pool.
	if probeSSH(sshClient) == nil {
		p.mu.Lock()
		if !p.closed {
			p.dropFailedLocked()
		}
		p.mu.Unlock()
		return nil
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return errors.New("connection pool is closed")
	}
	for _, conn := range p.conns {
		conn.retired = true
		conn.failed = true
		_ = conn.client.Close()
	}
	p.conns = nil
	for {
		select {
		case <-p.available:
			continue
		default:
		}
		break
	}
	oldSSH := p.sshClient
	p.sshClient = nil
	p.mu.Unlock()

	if oldSSH != nil {
		_ = oldSSH.Close()
	}

	newSSH, err := p.dial()
	if err != nil {
		return fmt.Errorf("failed to reconnect SSH: %w", err)
	}

	conns := make([]*pooledConn, 0, poolMinSize)
	for i := 0; i < poolMinSize; i++ {
		conn, err := newPooledConn(newSSH)
		if err != nil {
			_ = newSSH.Close()
			for _, created := range conns {
				_ = created.client.Close()
			}
			return fmt.Errorf("failed to recreate SFTP connection %d: %w", i+1, err)
		}
		conns = append(conns, conn)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		_ = newSSH.Close()
		for _, conn := range conns {
			_ = conn.client.Close()
		}
		return errors.New("connection pool is closed")
	}
	p.sshClient = newSSH
	p.conns = append(p.conns, conns...)
	for _, conn := range conns {
		p.offerLocked(conn)
	}
	return nil
}

// dropFailedLocked removes idle connections that were flagged during a
// transfer and replenishes the pool. The caller must hold p.mu.
func (p *connectionPool) dropFailedLocked() {
	var failed []*pooledConn
	for _, conn := range p.conns {
		// In-flight connections are dropped by release() once their owner is
		// done; closing them here would break the active transfer.
		if conn.failed && !conn.inUse {
			failed = append(failed, conn)
		}
	}
	if len(failed) > 0 {
		for _, conn := range failed {
			p.removeConnection(conn)
		}
	}
	// Top the pool back up so a failed connection that release() already
	// dropped does not leave the pool permanently below its minimum.
	for len(p.conns) < poolMinSize {
		conn, err := p.createConnection()
		if err != nil {
			return
		}
		p.conns = append(p.conns, conn)
		p.offerLocked(conn)
	}
}

// probeSSH reports whether the given SSH connection can still open an SFTP
// channel. It must be called without p.mu held.
func probeSSH(sshClient *ssh.Client) error {
	if sshClient == nil {
		return errors.New("SSH connection is not available")
	}
	done := make(chan error, 1)
	go func() {
		client, err := sftp.NewClient(sshClient)
		if err == nil {
			err = client.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		return errors.New("SSH connection probe timed out")
	}
}

// closeAll closes all connections in the pool
func (p *connectionPool) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return
	}

	p.closed = true

	if p.healthStop != nil {
		p.healthStop()
	}

	// 清空可用队列
	close(p.available)
	for range p.available {
		// drain channel
	}

	// 关闭所有连接
	for _, conn := range p.conns {
		_ = conn.client.Close()
	}
	p.conns = nil

	// 关闭 SSH 连接
	if p.sshClient != nil {
		_ = p.sshClient.Close()
	}
}
