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
	keepAliveInterval   = 20 * time.Second // SSH 传输层保活间隔
	idleTimeout         = 5 * time.Minute  // 空闲超时
	probeTimeout        = 2 * time.Second  // 单次存活探测超时
	connProbeInterval   = 5 * time.Second  // 同一连接两次探测的最小间隔
	channelSetupTimeout = 5 * time.Second  // 打开 SFTP channel 的超时
	reconnectCooldown   = 2 * time.Second  // 两次重连尝试的最小间隔，避免断网时反复拨号
)

var (
	errPoolClosed           = errors.New("connection pool is closed")
	errTransportUnavailable = errors.New("SSH transport is not available")
	errConnUnusable         = errors.New("no usable SFTP connection in the pool")
	errReconnectCooldown    = errors.New("SSH reconnect is rate limited")
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
	lastDial    time.Time // 上一次重连拨号时间，用于冷却
}

// pooledConn represents a single SFTP connection in the pool
type pooledConn struct {
	client    *sftp.Client
	lastUsed  time.Time
	lastProbe time.Time // 上一次确认可用的时间，避免每次取用都探测
	inUse     bool
	failed    bool
	retired   bool
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
// it to the available queue. It is safe to call from any goroutine.
func (p *connectionPool) markFailed(conn *pooledConn) {
	if conn == nil {
		return
	}
	p.mu.Lock()
	conn.failed = true
	conn.lastProbe = time.Time{}
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
		return nil, errTransportUnavailable
	}
	sftpClient, err := openSFTPClient(sshClient)
	if err != nil {
		return nil, err
	}
	return &pooledConn{
		client:    sftpClient,
		lastUsed:  time.Now(),
		lastProbe: time.Now(),
	}, nil
}

// openSFTPClient opens an SFTP subsystem channel with a deadline. sftp.NewClient
// has no timeout of its own: on a half-open TCP connection it can block forever,
// and callers may hold the pool lock while probing, which would freeze the whole
// pool instead of reporting a broken transport.
func openSFTPClient(sshClient *ssh.Client) (*sftp.Client, error) {
	type openResult struct {
		client *sftp.Client
		err    error
	}
	done := make(chan openResult, 1)
	go func() {
		client, err := sftp.NewClient(sshClient)
		done <- openResult{client: client, err: err}
	}()

	select {
	case res := <-done:
		return res.client, res.err
	case <-time.After(channelSetupTimeout):
		// The channel request may still complete later; close it so a late
		// success does not leak a client nobody holds.
		go func() {
			res := <-done
			if res.client != nil {
				_ = res.client.Close()
			}
		}()
		return nil, fmt.Errorf("timed out opening SFTP channel after %s", channelSetupTimeout)
	}
}

// createConnection creates a new SFTP connection using the shared SSH client
func (p *connectionPool) createConnection() (*pooledConn, error) {
	p.mu.Lock()
	sshClient := p.sshClient
	p.mu.Unlock()
	return newPooledConn(sshClient)
}

// acquire gets a usable connection from the pool. Connections whose transport
// died while idle are discarded here instead of being handed to a caller, so a
// network drop surfaces as a retryable error rather than a raw socket failure.
func (p *connectionPool) acquire(ctx context.Context) (*pooledConn, error) {
	for attempt := 0; attempt < poolMaxSize; attempt++ {
		conn, err := p.takeConn(ctx)
		if err != nil {
			return nil, err
		}
		if p.connUsable(conn) {
			return conn, nil
		}
		p.markFailed(conn)
		p.release(conn)
	}
	return nil, errConnUnusable
}

// takeConn pulls a connection off the queue, creating one when the pool has room.
// Dialing happens outside p.mu so a slow transport cannot stall every operation.
func (p *connectionPool) takeConn(ctx context.Context) (*pooledConn, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errPoolClosed
	}
	p.mu.Unlock()

	select {
	case conn := <-p.available:
		if conn == nil {
			return nil, errPoolClosed
		}
		p.mu.Lock()
		conn.inUse = true
		conn.lastUsed = time.Now()
		p.mu.Unlock()
		return conn, nil
	default:
	}

	p.mu.Lock()
	room := len(p.conns) < poolMaxSize
	p.mu.Unlock()
	if room {
		conn, err := p.createConnection()
		if err != nil {
			return nil, err
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			_ = conn.client.Close()
			return nil, errPoolClosed
		}
		if len(p.conns) >= poolMaxSize {
			// Another goroutine filled the pool meanwhile; wait for a slot.
			p.mu.Unlock()
			_ = conn.client.Close()
			return p.waitConn(ctx)
		}
		p.conns = append(p.conns, conn)
		conn.inUse = true
		conn.lastUsed = time.Now()
		p.mu.Unlock()
		return conn, nil
	}

	return p.waitConn(ctx)
}

func (p *connectionPool) waitConn(ctx context.Context) (*pooledConn, error) {
	select {
	case conn := <-p.available:
		if conn == nil {
			return nil, errPoolClosed
		}
		p.mu.Lock()
		conn.inUse = true
		conn.lastUsed = time.Now()
		p.mu.Unlock()
		return conn, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// connUsable reports whether a queued connection still works. Probing costs a
// round trip, so repeated checks within connProbeInterval reuse the last result.
func (p *connectionPool) connUsable(conn *pooledConn) bool {
	p.mu.Lock()
	unusable := conn == nil || conn.client == nil || conn.failed
	fresh := !unusable && !conn.lastProbe.IsZero() && time.Since(conn.lastProbe) < connProbeInterval
	p.mu.Unlock()
	if unusable {
		return false
	}
	if fresh {
		return true
	}
	if !probeConn(conn) {
		return false
	}
	p.mu.Lock()
	conn.lastProbe = time.Now()
	p.mu.Unlock()
	return true
}

// probeConn checks liveness with a bounded wait so a dead socket cannot hang
// the caller.
func probeConn(conn *pooledConn) bool {
	if conn == nil || conn.client == nil {
		return false
	}
	client := conn.client
	done := make(chan error, 1)
	go func() {
		_, err := client.Stat(".")
		done <- err
	}()
	select {
	case err := <-done:
		return err == nil
	case <-time.After(probeTimeout):
		return false
	}
}

// release returns a connection to the pool
func (p *connectionPool) release(conn *pooledConn) {
	if conn == nil {
		return
	}
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
	p.offerLocked(conn)
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
			if queued != nil && queued != conn {
				kept = append(kept, queued)
			}
			continue
		default:
		}
		break
	}
	if p.closed {
		for _, queued := range kept {
			_ = queued.client.Close()
		}
	} else {
		for _, queued := range kept {
			p.available <- queued
		}
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

// healthCheckLoop periodically checks connection health and keeps the SSH
// transport warm so a dropped link is noticed without waiting for the next
// user operation to fail.
func (p *connectionPool) healthCheckLoop(ctx context.Context) {
	healthTicker := time.NewTicker(healthCheckInterval)
	keepAliveTicker := time.NewTicker(keepAliveInterval)
	defer healthTicker.Stop()
	defer keepAliveTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-keepAliveTicker.C:
			p.keepAlive()
		case <-healthTicker.C:
			p.healthCheck()
		}
	}
}

// keepAlive sends a transport-level keepalive request. A failure means the TCP
// connection is gone even though no transfer noticed it yet, so the pool
// reconnects in the background instead of handing dead clients to callers.
func (p *connectionPool) keepAlive() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	sshClient := p.sshClient
	p.mu.Unlock()
	if sshClient == nil {
		p.requestReconnect()
		return
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := sshClient.SendRequest("keepalive@openssh.com", true, nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			p.requestReconnect()
		}
	case <-time.After(probeTimeout):
		p.requestReconnect()
	}
}

// requestReconnect runs a reconnect attempt in the background. Concurrent
// attempts are serialised by reconnectMu and rate limited by the cooldown.
func (p *connectionPool) requestReconnect() {
	go func() {
		_ = p.reconnect()
	}()
}

// healthCheck checks and maintains connection health
func (p *connectionPool) healthCheck() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}

	now := time.Now()
	idle := make([]*pooledConn, 0, len(p.conns))
	expired := make([]*pooledConn, 0, len(p.conns))
	for _, conn := range p.conns {
		if conn.inUse {
			continue
		}
		if conn.failed {
			expired = append(expired, conn)
			continue
		}
		// 关闭空闲超时的连接
		if now.Sub(conn.lastUsed) > idleTimeout && len(p.conns)-len(expired) > poolMinSize {
			expired = append(expired, conn)
			continue
		}
		idle = append(idle, conn)
	}
	for _, conn := range expired {
		p.removeConnection(conn)
	}
	p.mu.Unlock()

	// Probes talk to the server, so they must stay outside p.mu.
	probeFailed := false
	for _, conn := range idle {
		if probeConn(conn) {
			p.mu.Lock()
			conn.lastProbe = time.Now()
			p.mu.Unlock()
			continue
		}
		probeFailed = true
		p.mu.Lock()
		if !p.closed {
			conn.failed = true
			conn.lastProbe = time.Time{}
			if !conn.inUse {
				p.removeConnection(conn)
			}
		}
		p.mu.Unlock()
	}

	// A probe only fails when the shared transport is gone, so rebuild it in the
	// background instead of leaving the pool empty until the next operation.
	if probeFailed {
		p.requestReconnect()
	}

	p.replenish()
}

// replenish tops the pool back up to its minimum size, dialing outside p.mu.
func (p *connectionPool) replenish() {
	p.mu.Lock()
	need := poolMinSize - len(p.conns)
	p.mu.Unlock()

	for i := 0; i < need; i++ {
		conn, err := p.createConnection()
		if err != nil {
			return
		}
		p.mu.Lock()
		if p.closed || len(p.conns) >= poolMaxSize {
			p.mu.Unlock()
			_ = conn.client.Close()
			return
		}
		p.conns = append(p.conns, conn)
		p.offerLocked(conn)
		p.mu.Unlock()
	}
}

// reconnect re-establishes the shared SSH connection and every pooled SFTP
// client when the transport is no longer usable. The new transport is fully
// built before the old one is swapped out, so a failure leaves the previous
// state intact for the next attempt instead of bricking the pool.
func (p *connectionPool) reconnect() error {
	// Serialize reconnects: two concurrent calls could otherwise interleave
	// teardown, dial, and install and leak one of the new SSH clients.
	p.reconnectMu.Lock()
	defer p.reconnectMu.Unlock()

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return errPoolClosed
	}
	sshClient := p.sshClient
	lastDial := p.lastDial
	p.mu.Unlock()

	// The probe opens a fresh channel on the existing SSH client. Keep it out
	// of the pool lock so a hung probe cannot stall the whole pool.
	if probeSSH(sshClient) == nil {
		p.mu.Lock()
		if !p.closed {
			p.dropFailedLocked()
		}
		p.mu.Unlock()
		p.replenish()
		return nil
	}

	if !lastDial.IsZero() && time.Since(lastDial) < reconnectCooldown {
		return fmt.Errorf("%w: attempted %s ago", errReconnectCooldown, time.Since(lastDial).Round(time.Millisecond))
	}

	dial := p.dial
	if dial == nil {
		return errors.New("no SSH dialer configured")
	}

	p.mu.Lock()
	p.lastDial = time.Now()
	p.mu.Unlock()

	newSSH, err := dial()
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
	if p.closed {
		p.mu.Unlock()
		_ = newSSH.Close()
		for _, conn := range conns {
			_ = conn.client.Close()
		}
		return errPoolClosed
	}
	oldConns := p.conns
	oldSSH := p.sshClient
	p.conns = conns
	p.sshClient = newSSH
	for {
		select {
		case <-p.available:
			continue
		default:
		}
		break
	}
	for _, conn := range conns {
		p.offerLocked(conn)
	}
	// Retire the old generation under the lock: release() reads these flags.
	// Connections still owned by a caller are closed by release() so an
	// in-flight transfer is not torn down twice.
	closeNow := make([]*pooledConn, 0, len(oldConns))
	for _, conn := range oldConns {
		conn.retired = true
		conn.failed = true
		if !conn.inUse {
			closeNow = append(closeNow, conn)
		}
	}
	p.mu.Unlock()

	for _, conn := range closeNow {
		_ = conn.client.Close()
	}
	if oldSSH != nil {
		_ = oldSSH.Close()
	}
	return nil
}

// dropFailedLocked removes idle connections that were flagged during a
// transfer. The caller must hold p.mu. Replenishing is left to replenish()
// because dialing must not happen under the pool lock.
func (p *connectionPool) dropFailedLocked() {
	// Collect first: removeConnection mutates p.conns, and removing while
	// ranging would skip entries.
	failed := make([]*pooledConn, 0, len(p.conns))
	for _, conn := range p.conns {
		// In-flight connections are dropped by release() once their owner is
		// done; closing them here would break the active transfer.
		if conn.failed && !conn.inUse {
			failed = append(failed, conn)
		}
	}
	for _, conn := range failed {
		p.removeConnection(conn)
	}
}

// probeSSH reports whether the given SSH connection can still open an SFTP
// channel. It must be called without p.mu held.
func probeSSH(sshClient *ssh.Client) error {
	if sshClient == nil {
		return errTransportUnavailable
	}
	client, err := openSFTPClient(sshClient)
	if err != nil {
		return err
	}
	return client.Close()
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
		p.sshClient = nil
	}
}
