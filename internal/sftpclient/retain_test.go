package sftpclient

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestRetainKeepsSessionAfterTabDisconnect(t *testing.T) {
	manager, pool := newTestManager(t)
	var dials atomic.Int32
	pool.dial = func() (*ssh.Client, error) {
		dials.Add(1)
		return nil, errors.New("unexpected reconnect")
	}
	release, err := manager.Retain("session-1")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if manager.sessions["session-1"].refs != 2 {
		t.Fatal("retain did not acquire a session reference")
	}
	if err := manager.Disconnect("session-1"); err != nil {
		t.Fatal(err)
	}
	if !manager.IsConnected("session-1") || manager.byProfile["profile-1"] == nil {
		t.Fatal("tab disconnect removed the retained session")
	}
	pool.mu.Lock()
	closed := pool.closed
	pool.mu.Unlock()
	if closed {
		t.Fatal("tab disconnect closed the retained pool")
	}
	if _, err := manager.Stat("session-1", "."); err != nil {
		t.Fatalf("retained session cannot serve operations: %v", err)
	}
	release()
	if manager.IsConnected("session-1") || manager.byProfile["profile-1"] != nil {
		t.Fatal("final release retained the session indexes")
	}
	pool.mu.Lock()
	closed = pool.closed
	pool.mu.Unlock()
	if !closed {
		t.Fatal("final release did not close the pool")
	}
	if dials.Load() != 0 {
		t.Fatal("retain or release reconnected the session")
	}
}

func TestRetainReleaseIsConcurrentAndIdempotent(t *testing.T) {
	manager, pool := newTestManager(t)
	release, err := manager.Retain("session-1")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var calls sync.WaitGroup
	for range 16 {
		calls.Add(1)
		go func() {
			defer calls.Done()
			release()
		}()
	}
	calls.Wait()
	if !manager.IsConnected("session-1") || manager.sessions["session-1"].refs != 1 {
		t.Fatal("duplicate release consumed the tab reference")
	}
	pool.mu.Lock()
	closed := pool.closed
	pool.mu.Unlock()
	if closed {
		t.Fatal("duplicate release closed a session still owned by the tab")
	}
	if err := manager.Disconnect("session-1"); err != nil {
		t.Fatal(err)
	}
	if manager.IsConnected("session-1") {
		t.Fatal("tab disconnect did not close the final reference")
	}
}

func TestRetainUnknownSession(t *testing.T) {
	manager := NewManager()
	release, err := manager.Retain("missing")
	if !errors.Is(err, ErrSessionNotFound) || release != nil {
		t.Fatalf("Retain = (%v, %v), want nil release and ErrSessionNotFound", release != nil, err)
	}
}
