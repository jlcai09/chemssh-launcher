package sshclient

import (
	"context"
	"io"
	"log"
	"net"
	"sync"

	"chemweb-launcher/internal/config"

	"golang.org/x/crypto/ssh"
)

type Tunnel struct {
	listener net.Listener
	client   *ssh.Client
	remote   string
	cancel   context.CancelFunc
	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
}

func StartTunnel(ctx context.Context, client *ssh.Client, profile config.Profile) (*Tunnel, error) {
	ctx, cancel := context.WithCancel(ctx)
	ln, err := net.Listen("tcp", profile.LocalAddress())
	if err != nil {
		cancel()
		return nil, err
	}

	t := &Tunnel{
		listener: ln,
		client:   client,
		remote:   profile.RemoteAddress(),
		cancel:   cancel,
		conns:    make(map[net.Conn]struct{}),
	}
	t.wg.Add(1)
	go t.accept(ctx)
	return t, nil
}

func (t *Tunnel) Close() error {
	t.cancel()
	err := t.listener.Close()
	t.closeActiveConnections()
	t.wg.Wait()
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
				log.Printf("tunnel accept error: %v", err)
				continue
			}
		}
		t.addConn(local)
		t.wg.Add(1)
		go t.handle(local)
	}
}

func (t *Tunnel) handle(local net.Conn) {
	defer t.wg.Done()
	defer t.removeConn(local)
	defer local.Close()

	remote, err := t.client.Dial("tcp", t.remote)
	if err != nil {
		log.Printf("tunnel dial error: %v", err)
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
	_, _ = io.Copy(dst, src)
	_ = dst.Close()
	_ = src.Close()
	done <- struct{}{}
}
