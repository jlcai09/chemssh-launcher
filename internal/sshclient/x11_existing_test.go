package sshclient

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"chemssh-launcher/internal/config"
	"golang.org/x/crypto/ssh"
)

func existingXServerWithoutCookie(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var all sync.Map
	t.Cleanup(func() { _ = listener.Close(); all.Range(func(k, v any) bool { _ = k.(net.Conn).Close(); return true }) })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			all.Store(conn, true)
			go func() {
				defer all.Delete(conn)
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				header := make([]byte, 12)
				if _, err := io.ReadFull(conn, header); err != nil {
					return
				}
				order, err := x11Order(header[0])
				if err != nil || order.Uint16(header[6:8]) != 0 || order.Uint16(header[8:10]) != 0 {
					return
				}
				reply := make([]byte, 8)
				reply[0] = 1
				order.PutUint16(reply[2:4], 11)
				if _, err := conn.Write(reply); err != nil {
					return
				}
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return "127.0.0.1:" + strconv.Itoa(listener.Addr().(*net.TCPAddr).Port-6000)
}

func TestX11ForwardsToExistingServerWithoutAuthority(t *testing.T) {
	configureX11TestHome(t)
	t.Setenv("XAUTHORITY", filepath.Join(t.TempDir(), "missing"))
	display := existingXServerWithoutCookie(t)
	fixture := startX11SSHFixture(t, false, false)
	fixture.profile.X11 = config.X11Config{Enabled: true, Display: display}
	shell, err := StartInteractiveShell(fixture.profile, fixture.secrets, HostKeyAcceptNew, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer shell.Close()
	if !shell.X11Enabled || len(shell.x11.local.cookie) != 0 {
		t.Fatalf("existing server state: %v %q", shell.X11Enabled, shell.X11Error)
	}
	req := waitX11Request(t, fixture)
	fake, err := hex.DecodeString(req.AuthCookie)
	if err != nil || len(fake) != 16 {
		t.Fatal("missing synthetic SSH cookie")
	}
	remote := openTestX11(t, <-fixture.server)
	if err := writeX11All(remote, x11Setup(fake, binary.BigEndian)); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 8)
	done := make(chan error, 1)
	go func() { _, err := io.ReadFull(remote, reply); done <- err }()
	select {
	case err := <-done:
		if err != nil || reply[0] != 1 {
			t.Fatalf("forwarded setup: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("existing server forwarding stalled")
	}
	if _, err := remote.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(remote, echo); err != nil || string(echo) != "ping" {
		t.Fatalf("echo: %q %v", echo, err)
	}
	_ = remote.Close()
	_ = shell.Close()
	d, _ := parseX11Display(display)
	if err := verifyXServer(d, nil); err != nil {
		t.Fatalf("terminal close affected external server: %v", err)
	}
	// Omitting local authentication must never skip validation of the SSH cookie.
	if _, err := readX11Setup(bytes.NewReader(x11Setup(bytes.Repeat([]byte{0xff}, 16), binary.LittleEndian)), fake, nil); err == nil {
		t.Fatal("accepted a wrong SSH cookie for an unauthenticated local server")
	}
}

func TestX11RejectedRequestRejectsIncomingChannel(t *testing.T) {
	configureX11TestHome(t)
	xserver := startFakeXServer(t)
	fixture := startX11SSHFixture(t, true, false)
	fixture.profile.X11 = config.X11Config{Enabled: true, Display: xserver.display, XAuthority: fakeXAuthority(t, xserver)}
	shell, err := StartInteractiveShell(fixture.profile, fixture.secrets, HostKeyAcceptNew, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer shell.Close()
	server := <-fixture.server
	done := make(chan error, 1)
	go func() {
		channel, _, err := server.OpenChannel("x11", ssh.Marshal(struct {
			Address string
			Port    uint32
		}{"127.0.0.1", 4321}))
		if channel != nil {
			_ = channel.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		var denied *ssh.OpenChannelError
		if !errors.As(err, &denied) || denied.Reason != ssh.Prohibited {
			t.Fatalf("rejected request left forwarding open: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("incoming channel after failed request blocked transport")
	}
}

func TestX11HandshakeTimeoutHardClosesUnresponsivePeer(t *testing.T) {
	configureX11TestHome(t)
	xserver := startFakeXServer(t)
	fixture := startX11SSHFixture(t, false, false)
	fixture.profile.X11 = config.X11Config{Enabled: true, Display: xserver.display, XAuthority: fakeXAuthority(t, xserver)}
	shell, err := StartInteractiveShell(fixture.profile, fixture.secrets, HostKeyAcceptNew, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer shell.Close()
	server, peer := <-fixture.server, <-fixture.peers
	remote := openTestX11(t, server)
	peer.blocked.Store(true) // Stop processing client packets, including CHANNEL_CLOSE.
	if _, err := remote.Write([]byte{'l'}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-shell.x11.stopped:
	case <-time.After(x11IOTimeout + 2*time.Second):
		t.Fatal("handshake timeout did not stop workers against an unresponsive peer")
	}
	shell.x11.mu.Lock()
	remaining := len(shell.x11.active)
	shell.x11.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("handshake timeout leaked %d active resources", remaining)
	}
}
