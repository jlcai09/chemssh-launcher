package sftpclient

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"chemssh-launcher/internal/config"
	"chemssh-launcher/internal/secret"
	"chemssh-launcher/internal/sshclient"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

const (
	poolMinSize         = 2                 // 最小连接数
	poolMaxSize         = 3                 // 最大连接数
	healthCheckInterval = 30 * time.Second  // 健康检查间隔
	idleTimeout         = 5 * time.Minute   // 空闲超时
)

// connectionPool manages a pool of SFTP connections for a single profile
type connectionPool struct {
	profile    config.Profile
	secrets    secret.Store
	policy     sshclient.HostKeyPolicy
	mu         sync.Mutex
	sshClient  *ssh.Client        // 共享的 SSH 连接
	conns      []*pooledConn      // 连接池
	available  chan *pooledConn   // 可用连接队列
	refs       int                // 引用计数
	closed     bool
	healthStop context.CancelFunc
}

// pooledConn represents a single SFTP connection in the pool
type pooledConn struct {
	client     *sftp.Client
	lastUsed   time.Time
	inUse      bool
	failed     bool
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

// createConnection creates a new SFTP connection using the shared SSH client
func (p *connectionPool) createConnection() (*pooledConn, error) {
	// 复用同一个 SSH 连接创建新的 SFTP Client
	sftpClient, err := sftp.NewClient(p.sshClient)
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

// removeConnection removes and closes a connection from the pool
func (p *connectionPool) removeConnection(conn *pooledConn) {
	// 从连接列表中移除
	for i, c := range p.conns {
		if c == conn {
			p.conns = append(p.conns[:i], p.conns[i+1:]...)
			break
		}
	}
	// 关闭 SFTP 客户端
	_ = conn.client.Close()
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

	// 检查所有连接
	for _, conn := range p.conns {
		// 跳过正在使用的连接
		if conn.inUse {
			continue
		}

		// 关闭空闲超时的连接
		if now.Sub(conn.lastUsed) > idleTimeout && len(p.conns) > poolMinSize {
			p.removeConnection(conn)
			continue
		}

		// 健康检查
		if !p.isConnectionAlive(conn) {
			conn.failed = true
			p.removeConnection(conn)

			// 如果低于最小连接数，尝试重新创建
			if len(p.conns) < poolMinSize {
				newConn, err := p.createConnection()
				if err == nil {
					p.conns = append(p.conns, newConn)
					p.available <- newConn
				}
			}
		}
	}
}

// isConnectionAlive checks if a connection is still alive
func (p *connectionPool) isConnectionAlive(conn *pooledConn) bool {
	_, err := conn.client.Stat(".")
	return err == nil
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
