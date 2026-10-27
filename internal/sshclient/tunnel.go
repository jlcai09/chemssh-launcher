package sshclient

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"chemssh-launcher/internal/config"

	"golang.org/x/crypto/ssh"
)

// tunnelBufferSize is the per-connection copy buffer for the tunnel.
// 256 KB reduces syscall overhead on high-latency SSH links.
const tunnelBufferSize = 256 * 1024

type Tunnel struct {
	listener   net.Listener
	dialTarget func(context.Context) (net.Conn, error)
	label      string
	cancel     context.CancelFunc
	mu         sync.Mutex
	conns      map[net.Conn]struct{}
	wg         sync.WaitGroup
}

func StartTunnel(ctx context.Context, client *ssh.Client, profile config.Profile) (*Tunnel, error) {
	ctx, cancel := context.WithCancel(ctx)
	ln, err := net.Listen("tcp", profile.LocalAddress())
	if err != nil {
		cancel()
		return nil, err
	}

	t := newTunnel(ctx, cancel, ln, "tunnel", func(context.Context) (net.Conn, error) {
		return client.Dial("tcp", profile.RemoteAddress())
	})
	return t, nil
}

// StartReverseTunnel asks the SSH server to listen on its loopback on a free
// port and forwards each accepted connection to target on the launcher machine.
// It returns the server-side address that remote programs can dial.
func StartReverseTunnel(ctx context.Context, client *ssh.Client, target string) (*Tunnel, string, error) {
	if client == nil {
		return nil, "", errors.New("ssh client is required for a reverse tunnel")
	}
	ctx, cancel := context.WithCancel(ctx)
	ln, err := client.ListenTCP(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		cancel()
		return nil, "", err
	}

	dial := func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", target)
	}
	t := newTunnel(ctx, cancel, ln, "reverse tunnel", dial)
	return t, ln.Addr().String(), nil
}

func newTunnel(ctx context.Context, cancel context.CancelFunc, ln net.Listener, label string, dial func(context.Context) (net.Conn, error)) *Tunnel {
	t := &Tunnel{
		listener:   ln,
		dialTarget: dial,
		label:      label,
		cancel:     cancel,
		conns:      make(map[net.Conn]struct{}),
	}
	t.wg.Add(1)
	go t.accept(ctx)
	return t
}

func (t *Tunnel) Close() error {
	t.cancel()
	err := t.listener.Close()
	t.closeActiveConnections()
	done := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		log.Printf("%s close timed out; background connections will finish asynchronously", t.label)
	}
	return err
}

func (t *Tunnel) accept(ctx context.Context) {
	defer t.wg.Done()
	for {
		local, err := t.listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				log.Printf("%s accept error: %v", t.label, err)
				continue
			}
		}
		t.addConn(local)
		t.wg.Add(1)
		go t.handle(ctx, local)
	}
}

func (t *Tunnel) handle(ctx context.Context, local net.Conn) {
	defer t.wg.Done()
	defer t.removeConn(local)
	defer local.Close()

	remote, err := t.dialTarget(ctx)
	if err != nil {
		log.Printf("%s dial error: %v", t.label, err)
		return
	}
	t.addConn(remote)
	defer t.removeConn(remote)
	defer remote.Close()

	done := make(chan struct{}, 2)
	go copyAndClose(local, remote, done)
	go copyAndClose(remote, local, done)
	<-done
}

func (t *Tunnel) addConn(conn net.Conn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.conns[conn] = struct{}{}
}

func (t *Tunnel) removeConn(conn net.Conn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.conns, conn)
}

func (t *Tunnel) closeActiveConnections() {
	t.mu.Lock()
	conns := make([]net.Conn, 0, len(t.conns))
	for conn := range t.conns {
		conns = append(conns, conn)
	}
	t.mu.Unlock()

	for _, conn := range conns {
		_ = conn.Close()
	}
}

func copyAndClose(dst, src net.Conn, done chan<- struct{}) {
	buf := make([]byte, tunnelBufferSize)
	_, _ = io.CopyBuffer(dst, src, buf)
	_ = dst.Close()
	_ = src.Close()
	done <- struct{}{}
}
