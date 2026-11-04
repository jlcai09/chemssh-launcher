package sftpclient

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// contextSFTP owns a channel, never the shared SSH transport or a pooled client.
// The local pipes let cancellation unblock pkg/sftp even if the remote server
// does not acknowledge a channel close while a filesystem request is stuck.
type contextSFTP struct {
	client *sftp.Client
	cancel context.CancelFunc
}

func (c *contextSFTP) Close() error {
	c.cancel()
	return c.client.Close()
}

func (m *Manager) openContext(ctx context.Context, id string) (*contextSFTP, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	session, err := m.get(id)
	if err != nil {
		return nil, err
	}
	session.pool.mu.Lock()
	client, closed := session.pool.sshClient, session.pool.closed
	session.pool.mu.Unlock()
	if closed {
		return nil, errPoolClosed
	}
	if client == nil {
		return nil, errTransportUnavailable
	}
	return openContextSFTP(ctx, client)
}

func openContextSFTP(ctx context.Context, transport *ssh.Client) (*contextSFTP, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancel(ctx)
	type result struct {
		client *sftp.Client
		err    error
	}
	ready := make(chan result)
	go func() {
		channel, requests, err := transport.OpenChannel("session", nil)
		if err != nil {
			select {
			case ready <- result{err: err}:
			case <-lifetime.Done():
			}
			return
		}
		go ssh.DiscardRequests(requests)
		input, incoming := io.Pipe()
		outgoing, output := io.Pipe()
		var once sync.Once
		closeIO := func() {
			once.Do(func() {
				_ = input.CloseWithError(context.Canceled)
				_ = incoming.CloseWithError(context.Canceled)
				_ = outgoing.CloseWithError(context.Canceled)
				_ = output.CloseWithError(context.Canceled)
				// Closing a channel can itself wait for a transport write. The
				// local SFTP requests must not wait for that network operation.
				go channel.Close()
			})
		}
		stop := context.AfterFunc(lifetime, closeIO)
		go func() {
			_, err := io.Copy(incoming, channel)
			_ = incoming.CloseWithError(err)
		}()
		go func() {
			_, err := io.Copy(channel, outgoing)
			_ = outgoing.CloseWithError(err)
		}()
		go func() { _, _ = io.Copy(io.Discard, channel.Stderr()) }()
		// Subsystem setup may also block on a remote reply, so keep it in
		// this goroutine; cancellation still releases the caller immediately.
		ok, err := channel.SendRequest("subsystem", true, ssh.Marshal(struct{ Name string }{"sftp"}))
		if err == nil && !ok {
			err = errTransportUnavailable
		}
		var client *sftp.Client
		if err == nil && lifetime.Err() == nil {
			client, err = sftp.NewClientPipe(input, output)
		}
		select {
		case ready <- result{client: client, err: err}:
			if err == nil && client != nil {
				return
			}
		case <-lifetime.Done():
		}
		stop()
		closeIO()
		if client != nil {
			_ = client.Close()
		}
	}()
	timer := time.NewTimer(channelSetupTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	case <-timer.C:
		cancel()
		return nil, context.DeadlineExceeded
	case value := <-ready:
		if err := ctx.Err(); err != nil {
			cancel()
			if value.client != nil {
				_ = value.client.Close()
			}
			return nil, err
		}
		if value.err != nil {
			cancel()
			return nil, value.err
		}
		return &contextSFTP{client: value.client, cancel: cancel}, nil
	}
}

// StatContext is Stat on an independently cancellable SFTP channel.
func (m *Manager) StatContext(ctx context.Context, id, remotePath string) (FileInfo, error) {
	client, err := m.openContext(ctx, id)
	if err != nil {
		return FileInfo{}, err
	}
	defer client.Close()
	info, err := client.client.Stat(CleanPath(remotePath))
	if ctx.Err() != nil {
		return FileInfo{}, ctx.Err()
	}
	if err != nil {
		return FileInfo{}, err
	}
	return FileInfo{Name: info.Name(), Size: info.Size(), IsDir: info.IsDir(), ModTime: info.ModTime()}, nil
}

// ListContext is List on an independently cancellable SFTP channel.
func (m *Manager) ListContext(ctx context.Context, id, remotePath string) (ListResult, error) {
	client, err := m.openContext(ctx, id)
	if err != nil {
		return ListResult{}, err
	}
	defer client.Close()
	result, err := listDirectory(client.client, remotePath)
	if ctx.Err() != nil {
		return ListResult{}, ctx.Err()
	}
	return result, err
}

// DownloadContext closes only its own SFTP channel when ctx is cancelled.
// Closing the returned stream also releases that channel without returning an
// aborted connection to the pool or marking the shared transport as failed.
func (m *Manager) DownloadContext(ctx context.Context, id, remotePath string) (io.ReadCloser, FileInfo, error) {
	client, err := m.openContext(ctx, id)
	if err != nil {
		return nil, FileInfo{}, err
	}
	file, info, err := downloadFile(client.client, remotePath)
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		_ = client.Close()
		return nil, FileInfo{}, err
	}
	return &contextReadCloser{ctx: ctx, file: file, client: client}, info, nil
}

type contextReadCloser struct {
	ctx    context.Context
	file   *sftp.File
	client *contextSFTP
	once   sync.Once
	err    error
}

func (r *contextReadCloser) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.file.Read(p)
	if r.ctx.Err() != nil {
		err = r.ctx.Err()
	}
	return n, err
}

func (r *contextReadCloser) Close() error {
	r.once.Do(func() { r.err = r.client.Close() })
	return r.err
}
