package sshclient

import (
	"errors"
	"strings"
	"testing"
	"time"

	"chemssh-launcher/internal/config"
	"golang.org/x/crypto/ssh"
)

func TestX11RequestTimeoutKeepsTerminal(t *testing.T) {
	configureX11TestHome(t)
	xserver := startFakeXServer(t)
	fixture := startX11SSHFixture(t, false, false)
	fixture.withholdX11.Store(true)
	fixture.profile.X11 = config.X11Config{Enabled: true, Display: xserver.display, XAuthority: fakeXAuthority(t, xserver)}
	start := time.Now()
	shell, err := StartInteractiveShell(fixture.profile, fixture.secrets, HostKeyAcceptNew, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer shell.Close()
	if shell.X11Enabled || !strings.Contains(shell.X11Error, "timed out") {
		t.Fatalf("timeout state: enabled=%v reason=%q", shell.X11Enabled, shell.X11Error)
	}
	if time.Since(start) > x11IOTimeout+3*time.Second {
		t.Fatal("X11 timeout did not bound terminal startup")
	}
	if err := shell.WriteInput([]byte("hello")); err != nil {
		t.Fatalf("terminal did not survive X11 timeout: %v", err)
	}
	_ = waitX11Request(t, fixture)
	old, replacement := <-fixture.server, <-fixture.server
	if old == replacement {
		t.Fatal("timeout reused the unresponsive transport")
	}
	closed := make(chan struct{})
	go func() { _ = old.Wait(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("timed-out transport remained open")
	}
}

func TestX11ConnectionBoundAndInvalidOrigin(t *testing.T) {
	configureX11TestHome(t)
	xserver := startFakeXServer(t)
	fixture := startX11SSHFixture(t, false, false)
	fixture.profile.X11 = config.X11Config{Enabled: true, Display: xserver.display, XAuthority: fakeXAuthority(t, xserver)}
	shell, err := StartInteractiveShell(fixture.profile, fixture.secrets, HostKeyAcceptNew, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer shell.Close()
	_ = waitX11Request(t, fixture)
	server := <-fixture.server
	type result struct {
		channel ssh.Channel
		err     error
	}
	tryOpen := func(payload []byte) error {
		done := make(chan result, 1)
		go func() {
			channel, requests, err := server.OpenChannel("x11", payload)
			if err == nil {
				go ssh.DiscardRequests(requests)
			}
			done <- result{channel, err}
		}()
		select {
		case got := <-done:
			if got.channel != nil {
				_ = got.channel.Close()
			}
			return got.err
		case <-time.After(3 * time.Second):
			t.Fatal("channel rejection blocked SSH transport")
			return nil
		}
	}
	err = tryOpen([]byte{1})
	var channelErr *ssh.OpenChannelError
	if !errors.As(err, &channelErr) || channelErr.Reason != ssh.Prohibited {
		t.Fatalf("invalid origin: %v", err)
	}
	for i := 0; i < x11MaxConnections; i++ {
		_ = openTestX11(t, server)
	}
	err = tryOpen(ssh.Marshal(struct {
		Address string
		Port    uint32
	}{"127.0.0.1", 4321}))
	if !errors.As(err, &channelErr) || channelErr.Reason != ssh.ResourceShortage {
		t.Fatalf("unbounded X11 connections: %v", err)
	}
	if err := shell.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-shell.x11.stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("bounded X11 workers leaked on close")
	}
}
