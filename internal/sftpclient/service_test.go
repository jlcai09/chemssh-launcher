package sftpclient

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/pkg/sftp"
)

func newTestManager(t *testing.T) (*Manager, *connectionPool) {
	t.Helper()
	manager := NewManager()
	pool := newTestConnectionPool()
	conn := &pooledConn{client: newTestSFTPClient(t), lastUsed: time.Now(), lastProbe: time.Now()}
	pool.conns = []*pooledConn{conn}
	pool.available <- conn
	session := &activeSession{
		info: Session{ID: "session-1", ProfileID: "profile-1"},
		pool: pool,
		refs: 1,
	}
	manager.sessions[session.info.ID] = session
	manager.byProfile[session.info.ProfileID] = session
	t.Cleanup(func() {
		pool.mu.Lock()
		pool.closed = true
		pool.mu.Unlock()
	})
	return manager, pool
}

// Server-side status errors mean the channel answered, so retrying cannot help.
func TestWithConnDoesNotRetryServerErrors(t *testing.T) {
	manager, _ := newTestManager(t)

	calls := 0
	_, err := withConn(manager, "session-1", func(*sftp.Client) (int, error) {
		calls++
		return 0, &sftp.StatusError{Code: 2}
	})
	if err == nil {
		t.Fatal("withConn returned a nil error")
	}
	if calls != 1 {
		t.Fatalf("server-side error was retried: %d calls", calls)
	}
}

// An unknown session must not panic or dial anything.
func TestWithConnUnknownSession(t *testing.T) {
	manager := NewManager()
	if _, err := withConn(manager, "missing", func(*sftp.Client) (int, error) {
		t.Fatal("operation ran for an unknown session")
		return 0, nil
	}); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("error = %v, want ErrSessionNotFound", err)
	}
}

func TestIsTransportError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"closed pool", errPoolClosed, false},
		{"session gone", ErrSessionNotFound, false},
		{"cancelled", context.Canceled, false},
		{"sftp status", &sftp.StatusError{Code: 2}, false},
		{"eof", io.EOF, true},
		{"transport", errTransportUnavailable, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransportError(tc.err); got != tc.want {
				t.Fatalf("isTransportError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// A session whose transport cannot be recovered must leave both indexes so the
// next Connect dials a fresh one instead of returning the dead pool.
func TestDiscardDropsSessionIndexes(t *testing.T) {
	manager, pool := newTestManager(t)
	session := manager.sessions["session-1"]

	manager.discard(session)

	if len(manager.sessions) != 0 {
		t.Fatalf("sessions still holds the discarded session: %d", len(manager.sessions))
	}
	if len(manager.byProfile) != 0 {
		t.Fatalf("byProfile still holds the discarded session: %d", len(manager.byProfile))
	}
	if !pool.closed {
		t.Fatal("discard did not close the pool")
	}
}
