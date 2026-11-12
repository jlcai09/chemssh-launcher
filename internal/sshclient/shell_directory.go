package sshclient

import (
	"context"
	"errors"
	"strings"
	"time"
)

const shellDirectoryTimeout = 3 * time.Second

// CurrentDirectory reads the cwd of this terminal's shell, not of a new SSH or
// SFTP session (both of which start in the remote home directory). It does not
// write commands to the PTY, so an editor or an unfinished command is untouched.
// On hosts without procfs, callers can fall back to the shell's OSC cwd reports.
func (s *InteractiveShell) CurrentDirectory(ctx context.Context) (string, error) {
	if s == nil || s.Client == nil || s.integration == nil {
		return "", errors.New("terminal working directory is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, shellDirectoryTimeout)
	defer cancel()
	type result struct {
		path string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		session, err := s.Client.NewSession()
		if err != nil {
			done <- result{err: err}
			return
		}
		defer session.Close()
		// Closing just this query channel also unblocks a stuck remote command.
		stop := context.AfterFunc(ctx, func() { _ = session.Close() })
		defer stop()
		if err := ctx.Err(); err != nil {
			done <- result{err: err}
			return
		}
		out, err := session.Output(shellDirectoryCommand(s.integration))
		if err != nil {
			done <- result{err: err}
			return
		}
		path, err := parseShellDirectoryOutput(string(out))
		done <- result{path: path, err: err}
	}()
	select {
	case res := <-done:
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return res.path, res.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func shellDirectoryCommand(integration *shellIntegration) string {
	// The private, per-terminal directory prevents two tabs on the same server
	// from sharing a PID. Only a numeric PID may reach the procfs path.
	script := "pid=$(cat -- " + shellQuote(integration.shimDir+"/shell.pid") + ") || exit 1\n" +
		"case \"$pid\" in ''|*[!0-9]*) exit 1 ;; esac\n" +
		"cwd=$(readlink -- \"/proc/$pid/cwd\") || exit 1\n" +
		"printf '\\000%s\\000' \"$cwd\""
	return "/bin/sh -c " + shellQuote(script)
}

func parseShellDirectoryOutput(output string) (string, error) {
	// Login banners must not become part of the path. NUL framing also keeps
	// spaces, Unicode, quotes and newlines distinct from protocol delimiters.
	start := strings.IndexByte(output, 0)
	if start >= 0 {
		if end := strings.IndexByte(output[start+1:], 0); end >= 0 {
			path := output[start+1 : start+1+end]
			if validRemoteAbsolutePath(path) {
				return path, nil
			}
		}
	}
	return "", errors.New("terminal returned an invalid working directory")
}
