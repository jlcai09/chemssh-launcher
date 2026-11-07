package sshclient

// X11 forwarding grants remote applications trusted access to the local X
// server. This implements SSH cookie substitution, not ssh -X's SECURITY
// extension untrusted authorization.
import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"golang.org/x/crypto/ssh"
	"io"
	"net"
	"sync"
	"time"
)

const x11AuthProtocol = "MIT-MAGIC-COOKIE-1"
const x11IOTimeout = 5 * time.Second
const x11MaxConnections = 32

var errX11RequestTimeout = errors.New("X11 forwarding request timed out")

type x11Grant struct {
	done    chan struct{}
	allowed bool // Written before done closes; readers wait for done.
}

type x11Forwarder struct {
	client  *ssh.Client
	local   *localXServer
	fake    []byte
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	grant   *x11Grant
	active  map[io.Closer]struct{}
	slots   chan struct{}
	workers sync.WaitGroup
	stopped chan struct{}
}

func newX11Forwarder(client *ssh.Client, local *localXServer) (*x11Forwarder, error) {
	fake := make([]byte, 16)
	if _, err := rand.Read(fake); err != nil {
		return nil, fmt.Errorf("generate X11 authorization: %w", err)
	}
	incoming := client.HandleChannelOpen("x11")
	if incoming == nil {
		return nil, fmt.Errorf("X11 channel handler is already registered")
	}
	f := &x11Forwarder{client: client, local: local, fake: fake, done: make(chan struct{}), stopped: make(chan struct{}), active: make(map[io.Closer]struct{}), slots: make(chan struct{}, x11MaxConnections)}
	go f.serve(incoming)
	return f, nil
}

// request runs after PTY allocation and before starting each new shell.
// A timeout hard-closes the dedicated transport; the caller can redial a plain terminal.
func (f *x11Forwarder) request(session *ssh.Session) (requestErr error) {
	grant := &x11Grant{done: make(chan struct{})}
	f.mu.Lock()
	f.grant = grant
	f.mu.Unlock()
	defer func() { grant.allowed = requestErr == nil; close(grant.done) }()
	payload := ssh.Marshal(struct {
		SingleConnection bool
		AuthProtocol     string
		AuthCookie       string
		Screen           uint32
	}{false, x11AuthProtocol, hex.EncodeToString(f.fake), uint32(f.local.display.screen)})
	result := make(chan error, 1)
	go func() {
		ok, err := session.SendRequest("x11-req", true, payload)
		if err == nil && !ok {
			err = errors.New("SSH server rejected X11 forwarding (check X11Forwarding and remote xauth)")
		}
		result <- err
	}()
	timer := time.NewTimer(x11IOTimeout)
	defer timer.Stop()
	select {
	case err := <-result:
		return err
	case <-timer.C:
		f.Close()
		return errX11RequestTimeout
	case <-f.done:
		return errors.New("X11 forwarding closed")
	}
}
func (f *x11Forwarder) serve(incoming <-chan ssh.NewChannel) {
	// Drain until transport closure: late opens must not block SSH dispatch.
	for next := range incoming {
		select {
		case <-f.done:
			_ = next.Reject(ssh.Prohibited, "X11 forwarding closed")
			continue
		default:
		}
		f.mu.Lock()
		grant := f.grant
		f.mu.Unlock()
		if grant == nil {
			_ = next.Reject(ssh.Prohibited, "X11 forwarding was not requested")
			continue
		}
		select {
		case <-grant.done:
			if !grant.allowed {
				_ = next.Reject(ssh.Prohibited, "X11 forwarding was not granted")
				continue
			}
		default:
		}
		var origin struct {
			Address string
			Port    uint32
		}
		if ssh.Unmarshal(next.ExtraData(), &origin) != nil || origin.Port > 65535 {
			_ = next.Reject(ssh.Prohibited, "invalid X11 channel origin")
			continue
		}
		select {
		case f.slots <- struct{}{}:
			f.workers.Add(1)
			go func() { defer f.workers.Done(); defer func() { <-f.slots }(); f.forward(next, grant) }()
		default:
			_ = next.Reject(ssh.ResourceShortage, "too many X11 connections")
		}
	}
	f.Close()
	f.workers.Wait()
	close(f.stopped)
}
func (f *x11Forwarder) track(c io.Closer) bool {
	f.mu.Lock()
	select {
	case <-f.done:
		f.mu.Unlock()
		_ = c.Close()
		return false
	default:
	}
	f.active[c] = struct{}{}
	f.mu.Unlock()
	return true
}
func (f *x11Forwarder) untrack(c io.Closer) { f.mu.Lock(); delete(f.active, c); f.mu.Unlock() }
func (f *x11Forwarder) forward(next ssh.NewChannel, grant *x11Grant) {
	remote, requests, err := next.Accept()
	if err != nil {
		return
	}
	if !f.track(remote) {
		return
	}
	defer f.untrack(remote)
	defer f.closeRemote(remote)
	go ssh.DiscardRequests(requests)
	// Channel.Close only sends a packet; hard-close the transport on a stuck handshake.
	timer := time.AfterFunc(x11IOTimeout, f.Close)
	defer timer.Stop()
	setup, err := readX11Setup(remote, f.fake, f.local.cookie)
	if err != nil {
		return
	}
	// An early channel may be accepted while x11-req is pending, but it must
	// not reach the local X server until that request has actually succeeded.
	select {
	case <-grant.done:
		if !grant.allowed {
			return
		}
	case <-f.done:
		return
	}
	local, err := net.DialTimeout("tcp", f.local.display.address, x11IOTimeout)
	if err != nil {
		return
	}
	if !f.track(local) {
		return
	}
	defer f.untrack(local)
	defer local.Close()
	_ = local.SetDeadline(time.Now().Add(x11IOTimeout))
	if err := writeX11All(local, setup); err != nil {
		return
	}
	if !timer.Stop() {
		return
	}
	_ = local.SetDeadline(time.Time{})
	copied := make(chan struct{}, 1)
	go func() { _, _ = io.Copy(local, remote); _ = local.Close(); f.closeRemote(remote); copied <- struct{}{} }()
	_, _ = io.Copy(remote, local)
	_ = local.Close()
	f.closeRemote(remote)
	<-copied
}

// closeRemote bounds CHANNEL_CLOSE writes when a peer stops reading.
func (f *x11Forwarder) closeRemote(remote ssh.Channel) {
	timer := time.AfterFunc(x11IOTimeout, f.Close)
	defer timer.Stop()
	_ = remote.Close()
}
func (f *x11Forwarder) Close() {
	if f == nil {
		return
	}
	f.once.Do(func() {
		close(f.done)
		// Each launcher terminal has a dedicated transport. Closing its net.Conn
		// unblocks SSH Read, Write and SendRequest even without a peer close reply.
		_ = f.client.Close()
		f.mu.Lock()
		active := make([]io.Closer, 0, len(f.active))
		for c := range f.active {
			active = append(active, c)
		}
		f.mu.Unlock()
		for _, c := range active {
			_ = c.Close()
		}
		f.local.Close()
	})
}
func x11Order(b byte) (binary.ByteOrder, error) {
	switch b {
	case 'l':
		return binary.LittleEndian, nil
	case 'B':
		return binary.BigEndian, nil
	}
	return nil, errors.New("invalid X11 byte order")
}
func x11Padded(n int) int { return (n + 3) &^ 3 }

// Validate bounded auth data before dialing the local server; never forward
// arbitrary unauthenticated input to the local display.
func readX11Setup(r io.Reader, fake, real []byte) ([]byte, error) {
	header := make([]byte, 12)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	order, err := x11Order(header[0])
	if err != nil {
		return nil, err
	}
	if order.Uint16(header[2:4]) != 11 || order.Uint16(header[4:6]) != 0 {
		return nil, errors.New("unsupported X11 protocol")
	}
	nameLen, dataLen := int(order.Uint16(header[6:8])), int(order.Uint16(header[8:10]))
	if nameLen != len(x11AuthProtocol) || dataLen != len(fake) || len(fake) != 16 || (len(real) != 0 && len(real) != 16) {
		return nil, errors.New("invalid X11 authorization lengths")
	}
	auth := make([]byte, x11Padded(nameLen)+x11Padded(dataLen))
	if _, err := io.ReadFull(r, auth); err != nil {
		return nil, err
	}
	if string(auth[:nameLen]) != x11AuthProtocol || subtle.ConstantTimeCompare(auth[x11Padded(nameLen):x11Padded(nameLen)+dataLen], fake) != 1 {
		return nil, errors.New("invalid X11 authorization")
	}
	return x11Setup(real, order), nil
}
func x11Setup(cookie []byte, order binary.ByteOrder) []byte {
	name := x11AuthProtocol
	if len(cookie) == 0 {
		name = ""
	}
	packet := make([]byte, 12+x11Padded(len(name))+x11Padded(len(cookie)))
	packet[0] = 'l'
	if order == binary.BigEndian {
		packet[0] = 'B'
	}
	order.PutUint16(packet[2:4], 11)
	order.PutUint16(packet[6:8], uint16(len(name)))
	order.PutUint16(packet[8:10], uint16(len(cookie)))
	copy(packet[12:], name)
	copy(packet[12+x11Padded(len(name)):], cookie)
	return packet
}
func writeX11All(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
