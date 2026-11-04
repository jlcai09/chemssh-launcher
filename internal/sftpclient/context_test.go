package sftpclient

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type contextTestFS struct {
	block   string
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (f *contextTestFS) wait(method string) {
	if f.block == method {
		f.once.Do(func() { close(f.started) })
		<-f.release
	}
}
func (f *contextTestFS) Fileread(*sftp.Request) (io.ReaderAt, error) { return f, nil }
func (f *contextTestFS) ReadAt(p []byte, off int64) (int, error) {
	f.wait("read")
	return strings.NewReader("contents").ReadAt(p, off)
}
func (f *contextTestFS) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	f.wait(r.Method)
	return contextTestList{}, nil
}

type contextTestList struct{}

func (contextTestList) ListAt(infos []os.FileInfo, off int64) (int, error) {
	if off != 0 {
		return 0, io.EOF
	}
	infos[0] = contextTestInfo{}
	return 1, io.EOF
}

type contextTestInfo struct{}

func (contextTestInfo) Name() string       { return "file.txt" }
func (contextTestInfo) Size() int64        { return 8 }
func (contextTestInfo) Mode() os.FileMode  { return 0o600 }
func (contextTestInfo) ModTime() time.Time { return time.Unix(0, 0) }
func (contextTestInfo) IsDir() bool        { return false }
func (contextTestInfo) Sys() any           { return nil }

func newContextTestManager(t *testing.T, fs *contextTestFS) (*Manager, *connectionPool, *atomic.Int32) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var channelsOpened atomic.Int32
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		server, channels, requests, err := ssh.NewServerConn(conn, config)
		if err != nil {
			_ = conn.Close()
			return
		}
		defer server.Close()
		go func() {
			for r := range requests {
				_ = r.Reply(true, nil)
			}
		}()
		for pending := range channels {
			channel, requests, err := pending.Accept()
			if err != nil {
				continue
			}
			channelsOpened.Add(1)
			go func() {
				defer channel.Close()
				for request := range requests {
					if request.Type != "subsystem" {
						_ = request.Reply(false, nil)
						continue
					}
					_ = request.Reply(true, nil)
					server := sftp.NewRequestServer(channel, sftp.Handlers{FileGet: fs, FileList: fs})
					_ = server.Serve()
					_ = server.Close()
					return
				}
			}()
		}
	}()
	client, err := ssh.Dial("tcp", listener.Addr().String(), &ssh.ClientConfig{
		User: "user", HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	pool := newTestConnectionPool()
	pool.sshClient = client
	manager := NewManager()
	manager.sessions["session"] = &activeSession{pool: pool}
	return manager, pool, &channelsOpened
}

func TestContextSFTPCancelsBlockedOperationsWithoutClosingSSH(t *testing.T) {
	for _, method := range []string{"read", "Stat", "List"} {
		t.Run(method, func(t *testing.T) {
			fs := &contextTestFS{block: method, started: make(chan struct{}), release: make(chan struct{})}
			defer close(fs.release)
			manager, pool, _ := newContextTestManager(t, fs)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				switch method {
				case "read":
					reader, _, err := manager.DownloadContext(ctx, "session", "/file.txt")
					if err == nil {
						_, err = reader.Read(make([]byte, 8))
						_ = reader.Close()
					}
					result <- err
				case "Stat":
					_, err := manager.StatContext(ctx, "session", "/file.txt")
					result <- err
				case "List":
					_, err := manager.ListContext(ctx, "session", "/")
					result <- err
				}
			}()
			select {
			case <-fs.started:
			case <-time.After(2 * time.Second):
				t.Fatal("request did not reach blocking server handler")
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error = %v, want context.Canceled", err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation waited for blocked server handler")
			}
			if _, _, err := pool.sshClient.SendRequest("keepalive@test", true, nil); err != nil {
				t.Fatalf("cancel closed shared SSH transport: %v", err)
			}
			if pool.closed || len(pool.conns) != 0 {
				t.Fatal("context operation changed the pool")
			}
		})
	}
}

func TestContextSFTPPreCancelledDoesNotOpenChannel(t *testing.T) {
	manager, _, opened := newContextTestManager(t, &contextTestFS{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := manager.DownloadContext(ctx, "session", "/file.txt"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := manager.StatContext(ctx, "session", "/file.txt"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := manager.ListContext(ctx, "session", "/"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if got := opened.Load(); got != 0 {
		t.Fatalf("pre-cancel opened %d channels", got)
	}
}

func TestContextSFTPNormalReadAndCloseKeepsTransport(t *testing.T) {
	manager, pool, _ := newContextTestManager(t, &contextTestFS{})
	reader, info, err := manager.DownloadContext(context.Background(), "session", "/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != "contents" || info.Size != 8 {
		t.Fatalf("data=%q info=%+v err=%v", data, info, err)
	}
	_ = reader.Close()
	_ = reader.Close()
	if _, _, err := pool.sshClient.SendRequest("keepalive@test", true, nil); err != nil {
		t.Fatal(err)
	}
}
