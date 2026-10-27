package sftpclient

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type failingReadCloser struct{}

func (failingReadCloser) Read([]byte) (int, error) {
	return 0, errors.New("simulated read failure")
}

func (failingReadCloser) Close() error { return nil }

type failingWriteCloser struct{}

func (failingWriteCloser) Write([]byte) (int, error) {
	return 0, errors.New("simulated write failure")
}

func (failingWriteCloser) Close() error { return nil }

func newTestConnectionPool() *connectionPool {
	return &connectionPool{available: make(chan *pooledConn, poolMaxSize)}
}

// newTestSFTPClient returns a client backed by an in-process server so pool
// code that closes a connection has a real *sftp.Client to work with.
func newTestSFTPClient(t *testing.T) *sftp.Client {
	t.Helper()
	local, remote := net.Pipe()
	server, err := sftp.NewServer(remote)
	if err != nil {
		t.Fatalf("new test SFTP server: %v", err)
	}
	go func() { _ = server.Serve() }()
	client, err := sftp.NewClientPipe(local, local)
	if err != nil {
		t.Fatalf("new test SFTP client: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
		_ = local.Close()
		_ = remote.Close()
	})
	return client
}

func assertConnectionDropped(t *testing.T, pool *connectionPool) {
	t.Helper()
	select {
	case conn := <-pool.available:
		t.Fatalf("failed connection was returned to the available queue: %p", conn)
	default:
	}
	if len(pool.conns) != 0 {
		t.Fatalf("failed connection remained in the pool: %d entries", len(pool.conns))
	}
}

func TestPooledReadCloserDropsFailedConnection(t *testing.T) {
	pool := newTestConnectionPool()
	conn := &pooledConn{client: newTestSFTPClient(t)}
	pool.conns = []*pooledConn{conn}
	reader := &pooledReadCloser{ReadCloser: failingReadCloser{}, pool: pool, conn: conn}

	if _, err := reader.Read(make([]byte, 8)); err == nil {
		t.Fatal("Read returned a nil error")
	}
	if !conn.failed {
		t.Fatal("a failed Read did not mark the connection as failed")
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	assertConnectionDropped(t, pool)
}

func TestPooledWriteCloserDropsFailedConnection(t *testing.T) {
	pool := newTestConnectionPool()
	conn := &pooledConn{client: newTestSFTPClient(t)}
	pool.conns = []*pooledConn{conn}
	writer := &pooledWriteCloser{WriteCloser: failingWriteCloser{}, pool: pool, conn: conn}

	if _, err := writer.Write([]byte("payload")); err == nil {
		t.Fatal("Write returned a nil error")
	}
	if !conn.failed {
		t.Fatal("a failed Write did not mark the connection as failed")
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	assertConnectionDropped(t, pool)
}

// A failed idle connection lives in the available queue, so dropping it must
// purge that entry; otherwise acquire() hands the caller a closed client.
func TestDropFailedLockedPurgesAvailableQueue(t *testing.T) {
	pool := newTestConnectionPool()
	dead := &pooledConn{client: newTestSFTPClient(t), failed: true, lastUsed: time.Now()}
	healthy := &pooledConn{client: newTestSFTPClient(t), lastUsed: time.Now()}
	pool.conns = []*pooledConn{dead, healthy}
	pool.available <- dead
	pool.available <- healthy

	pool.mu.Lock()
	pool.dropFailedLocked()
	pool.mu.Unlock()

	if len(pool.conns) == 2 {
		t.Fatal("dropFailedLocked kept the failed connection in the pool")
	}
	got := drainAvailable(pool)
	for _, conn := range got {
		if conn == dead {
			t.Fatal("the failed connection is still queued for acquire()")
		}
	}
	if len(got) != 1 || got[0] != healthy {
		t.Fatalf("available queue lost a healthy connection: %d entries", len(got))
	}
}

// The queue has a fixed capacity, so a saturated pool must refuse the extra
// connection instead of blocking while it holds the pool lock.
func TestOfferLockedDropsWhenQueueIsFull(t *testing.T) {
	pool := newTestConnectionPool()
	var queued []*pooledConn
	for i := 0; i < poolMaxSize; i++ {
		conn := &pooledConn{client: newTestSFTPClient(t), lastUsed: time.Now()}
		pool.conns = append(pool.conns, conn)
		pool.available <- conn
		queued = append(queued, conn)
	}
	extra := &pooledConn{client: newTestSFTPClient(t), lastUsed: time.Now()}
	pool.conns = append(pool.conns, extra)

	done := make(chan struct{})
	go func() {
		pool.mu.Lock()
		pool.offerLocked(extra)
		pool.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("offerLocked blocked on a full available queue")
	}

	got := drainAvailable(pool)
	if len(got) != poolMaxSize {
		t.Fatalf("available queue changed size: %d entries", len(got))
	}
	for _, conn := range got {
		if conn == extra {
			t.Fatal("the rejected connection stayed in the queue")
		}
	}
	if len(pool.conns) != poolMaxSize {
		t.Fatalf("rejected connection stayed in the pool: %d entries", len(pool.conns))
	}
	for _, conn := range queued {
		_ = conn.client.Close()
	}
}

func drainAvailable(pool *connectionPool) []*pooledConn {
	var got []*pooledConn
	for {
		select {
		case conn := <-pool.available:
			got = append(got, conn)
			continue
		default:
		}
		return got
	}
}

func TestReconnectDialsWithoutHoldingPoolLock(t *testing.T) {
	pool := newTestConnectionPool()

	dialStarted := make(chan struct{})
	releaseDial := make(chan struct{})
	pool.dial = func() (*ssh.Client, error) {
		close(dialStarted)
		<-releaseDial
		return nil, errors.New("simulated dial failure")
	}

	reconnected := make(chan error, 1)
	go func() { reconnected <- pool.reconnect() }()

	select {
	case <-dialStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("reconnect never started dialing")
	}

	// The pool lock must be free while the dial is blocked.
	acquired := make(chan error, 1)
	go func() {
		_, err := pool.acquire(context.Background())
		acquired <- err
	}()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("acquire blocked while reconnect was dialing")
	}

	close(releaseDial)
	if err := <-reconnected; err == nil {
		t.Fatal("reconnect succeeded with a failing dial")
	}
}
