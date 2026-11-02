package sshclient

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	remoteCommandTimeout = 10 * time.Minute
	remoteOutputLimit    = 64 * 1024
)

// RunCommand runs argv on the remote host inside `dir` and returns the combined
// stdout/stderr. Every argument is shell-quoted, so a file name can never be
// interpreted by the remote shell.
func RunCommand(client *ssh.Client, dir string, argv []string) (string, error) {
	return RunCommandTimeout(client, dir, argv, remoteCommandTimeout)
}

// RunCommandTimeout is RunCommand with an explicit timeout. A timeout of zero or
// less waits for the command to finish, which server-side copies need because a
// large tree can take far longer than any fixed limit.
func RunCommandTimeout(client *ssh.Client, dir string, argv []string, timeout time.Duration) (string, error) {
	if client == nil {
		return "", errors.New("ssh client is not available")
	}
	if len(argv) == 0 || argv[0] == "" {
		return "", errors.New("empty remote command")
	}
	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()

	quoted := make([]string, 0, len(argv))
	for _, arg := range argv {
		quoted = append(quoted, shellQuote(arg))
	}
	command := strings.Join(quoted, " ")
	if dir != "" {
		command = "cd " + shellQuote(dir) + " && " + command
	}

	output := &limitedBuffer{limit: remoteOutputLimit}
	session.Stdout = output
	session.Stderr = output

	done := make(chan error, 1)
	go func() {
		done <- session.Run(command)
	}()
	if timeout <= 0 {
		// The command is allowed to run to completion, so reading the buffer
		// afterwards is safe.
		err := <-done
		return output.String(), err
	}
	select {
	case err := <-done:
		return output.String(), err
	case <-time.After(timeout):
		// Never read the buffer here: the goroutine above may still be writing.
		_ = session.Close()
		<-done
		return "", errors.New("remote command timed out")
	}
}

// limitedBuffer keeps the first `limit` bytes of a command's output.
type limitedBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if remaining := b.limit - b.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			b.buf.Write(p[:remaining])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(b.buf.String())
}
