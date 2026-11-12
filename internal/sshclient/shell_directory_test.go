package sshclient

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestParseShellDirectoryOutput(t *testing.T) {
	for _, tt := range []struct {
		name, output, want string
	}{
		{"framed", "\x00/home/user/project\x00", "/home/user/project"},
		{"login banners", "Welcome to example-server\r\n\x00/home/user/project\x00\nSession notice", "/home/user/project"},
		{"spaces", "\x00/home/user/project with spaces \x00", "/home/user/project with spaces "},
		{"unicode", "\x00/home/user/材料模拟/计算\x00", "/home/user/材料模拟/计算"},
		{"quotes", "\x00/home/user/a'b\"c\x00", "/home/user/a'b\"c"},
		{"root", "banner\x00/\x00notice", "/"},
		{"empty output", "", ""},
		{"unframed", "/home/user/project\n", ""},
		{"missing closing delimiter", "banner\x00/home/user/project", ""},
		{"empty path", "\x00\x00", ""},
		{"relative path", "\x00project/subdir\x00", ""},
		{"tilde path", "\x00~/project\x00", ""},
		{"windows path", "\x00C:\\Users\\user\\Desktop\x00", ""},
		{"newline", "\x00/home/user/a\nb\x00", ""},
		{"carriage return", "\x00/home/user/a\rb\x00", ""},
		{"invalid first frame", "\x00relative\x00\x00/home/user/project\x00", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseShellDirectoryOutput(tt.output)
			if tt.want == "" {
				if err == nil || got != "" {
					t.Fatalf("invalid response returned path %q, error %v", got, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("path = %q, error %v; want %q", got, err, tt.want)
			}
		})
	}
}

// Run the actual PID-preserving exec wrapper without any PROMPT_COMMAND hook.
func TestShellDirectoryCommandTracksIndependentShells(t *testing.T) {
	bash := shellDirectoryTestBash(t)
	base := t.TempDir()
	home := filepath.Join(base, "home")
	firstDir := filepath.Join(base, "terminal one 材料's cwd")
	secondDir := filepath.Join(base, "terminal two")
	for _, dir := range []string{home, firstDir, secondDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	posixPath := func(hostPath string) string {
		t.Helper()
		out, err := shellDirectoryTestRun(bash, base, "cd -- "+shellQuote(filepath.ToSlash(hostPath))+" && pwd -P")
		if err != nil {
			t.Fatalf("resolve test directory: %v", err)
		}
		return strings.TrimSuffix(string(out), "\n")
	}
	first := startShellDirectoryTestShell(t, bash, home, filepath.Join(base, "shim one '私有"), posixPath)
	second := startShellDirectoryTestShell(t, bash, home, filepath.Join(base, "shim two"), posixPath)
	if first.integration.shimDir == second.integration.shimDir || first.pid == second.pid {
		t.Fatal("terminals must have different private directories and shell PIDs")
	}
	query := func(shell *shellDirectoryTestShell, want string) {
		t.Helper()
		// Every query starts in home, independently of the terminal's cwd.
		out, err := shellDirectoryTestRun(bash, home, shellDirectoryCommand(shell.integration))
		if err != nil {
			t.Fatalf("query terminal directory: %v", err)
		}
		got, err := parseShellDirectoryOutput(string(out))
		if err != nil || got != want {
			t.Fatalf("terminal cwd = %q, error %v; want %q", got, err, want)
		}
	}
	homePath := posixPath(home)
	query(first, homePath)
	query(second, homePath)
	first.run(t, "cd -- "+shellQuote(posixPath(firstDir)))
	query(first, posixPath(firstDir))
	query(second, homePath)
	second.run(t, "cd -- "+shellQuote(posixPath(secondDir)))
	query(first, posixPath(firstDir))
	query(second, posixPath(secondDir))
	first.run(t, "cd /")
	query(first, "/")
	query(second, posixPath(secondDir))
	second.run(t, "PROMPT_COMMAND=':'; unset PROMPT_COMMAND; cd -- "+shellQuote(posixPath(firstDir)))
	query(second, posixPath(firstDir))
	second.stop(t)
	out, err := shellDirectoryTestRun(bash, home, shellDirectoryCommand(second.integration))
	if err == nil || len(out) != 0 {
		t.Fatalf("exited shell returned %q, error %v; want failure without home fallback", out, err)
	}
	for _, pid := range []string{"", "0", "-1", "999999999999999999999999", "1 2", "123\n456", "$(printf 1)", "1; printf /home/user"} {
		t.Run("invalid PID "+strconv.Quote(pid), func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(second.shimDir, "shell.pid"), []byte(pid), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := shellDirectoryTestRun(bash, home, shellDirectoryCommand(second.integration))
			if err == nil || len(out) != 0 {
				t.Fatalf("invalid PID returned %q, error %v; want failure without home fallback", out, err)
			}
		})
	}
	if err := os.Remove(filepath.Join(second.shimDir, "shell.pid")); err != nil {
		t.Fatal(err)
	}
	out, err = shellDirectoryTestRun(bash, home, shellDirectoryCommand(second.integration))
	if err == nil || len(out) != 0 {
		t.Fatalf("missing PID file returned %q, error %v", out, err)
	}
}

func shellDirectoryTestBash(t *testing.T) string {
	t.Helper()
	var candidates []string
	if bash, err := exec.LookPath("bash"); err == nil {
		candidates = append(candidates, bash)
	}
	if runtime.GOOS == "windows" {
		if git, err := exec.LookPath("git"); err == nil {
			candidates = append(candidates, filepath.Join(filepath.Dir(git), "..", "bin", "bash.exe"))
		}
		for _, root := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")} {
			if root != "" {
				candidates = append(candidates, filepath.Join(root, "Git", "bin", "bash.exe"))
			}
		}
	}
	for _, bash := range candidates {
		out, err := shellDirectoryTestRun(bash, "", "test -x /bin/sh && test -x /bin/bash && readlink -- /proc/$$/cwd")
		if err == nil && strings.HasPrefix(string(out), "/") {
			return bash
		}
	}
	t.Skip("no available Bash with /bin/sh and readable /proc shell cwd")
	return ""
}

func shellDirectoryTestRun(bash, dir, script string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bash, "--noprofile", "--norc", "-c", script)
	cmd.Dir = dir
	return cmd.Output()
}

type shellDirectoryTestShell struct {
	integration *shellIntegration
	shimDir     string
	pid         string
	stdin       io.WriteCloser
	lines       <-chan string
	done        <-chan error
	cmd         *exec.Cmd
	stopped     bool
}

func startShellDirectoryTestShell(t *testing.T, bash, home, shimDir string, posixPath func(string) string) *shellDirectoryTestShell {
	t.Helper()
	if err := os.Mkdir(shimDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shimDir, "init.sh"), []byte("unset PROMPT_COMMAND\nPS1=\nPS2=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &shellDirectoryTestShell{integration: &shellIntegration{shimDir: posixPath(shimDir), shellPath: "/bin/bash", bash: true}, shimDir: shimDir}
	s.cmd = exec.Command(bash, "--noprofile", "--norc", "-c", interactiveShellCommand(s.integration, nil))
	s.cmd.Dir = home
	var err error
	s.stdin, err = s.cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := s.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	s.cmd.Stderr = io.Discard // Non-PTY job-control warnings are expected.
	if err := s.cmd.Start(); err != nil {
		t.Fatalf("start terminal shell: %v", err)
	}
	lines := make(chan string, 16)
	s.lines = lines
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	done := make(chan error, 1)
	s.done = done
	go func() { done <- s.cmd.Wait() }()
	t.Cleanup(func() { s.stop(t) })
	s.run(t, "test -z \"${PROMPT_COMMAND:-}\"")
	pid, err := os.ReadFile(filepath.Join(shimDir, "shell.pid"))
	if err != nil {
		t.Fatalf("read terminal PID: %v", err)
	}
	if n, err := strconv.ParseUint(string(pid), 10, 64); err != nil || n == 0 {
		t.Fatalf("terminal wrote invalid PID %q", pid)
	}
	s.pid = string(pid)
	return s
}

func (s *shellDirectoryTestShell) run(t *testing.T, command string) {
	t.Helper()
	const marker = "CHEMSSH_DIRECTORY_TEST_READY"
	if _, err := fmt.Fprintf(s.stdin, "%s && printf '%%s\\n' '%s'\n", command, marker); err != nil {
		t.Fatalf("send terminal command: %v", err)
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case line, ok := <-s.lines:
			if !ok {
				t.Fatal("terminal exited before command acknowledgement")
			}
			if line == marker {
				return
			}
		case <-timer.C:
			t.Fatal("terminal command acknowledgement timed out")
		}
	}
}

func (s *shellDirectoryTestShell) stop(t *testing.T) {
	t.Helper()
	if s.stopped {
		return
	}
	s.stopped = true
	_, _ = io.WriteString(s.stdin, "exit\n")
	_ = s.stdin.Close()
	select {
	case err := <-s.done:
		if err != nil {
			t.Errorf("terminal exit: %v", err)
		}
	case <-time.After(3 * time.Second):
		_ = s.cmd.Process.Kill()
		<-s.done
		t.Error("terminal did not exit after stdin close")
	}
}

func TestShellCurrentDirectoryUnavailable(t *testing.T) {
	for _, shell := range []*InteractiveShell{nil, {}, {integration: &shellIntegration{shimDir: "/tmp/example-terminal"}}} {
		if got, err := shell.CurrentDirectory(context.Background()); err == nil || got != "" {
			t.Fatalf("unavailable terminal returned %q, error %v", got, err)
		}
	}
}

func TestShellCurrentDirectoryTimeoutClosesQuery(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	started := make(chan string, 1)
	closed := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		server, channels, requests, err := ssh.NewServerConn(conn, cfg)
		if err != nil {
			return
		}
		defer server.Close()
		go ssh.DiscardRequests(requests)
		for newChannel := range channels {
			channel, requests, err := newChannel.Accept()
			if err != nil {
				return
			}
			defer channel.Close()
			for req := range requests {
				if req.Type != "exec" {
					_ = req.Reply(false, nil)
					continue
				}
				var payload struct{ Command string }
				if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
					_ = req.Reply(false, nil)
					continue
				}
				_ = req.Reply(true, nil)
				started <- payload.Command
				// Output closes stdin immediately; wait for channel closure,
				// not stdin EOF, to model a stalled remote command.
				for range requests {
				}
				close(closed)
				return
			}
		}
	}()
	client, err := ssh.Dial("tcp", listener.Addr().String(), &ssh.ClientConfig{
		User: "user", HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	integration := &shellIntegration{shimDir: "/tmp/example-terminal"}
	shell := &InteractiveShell{Client: client, integration: integration}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	type result struct {
		path string
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		path, err := shell.CurrentDirectory(ctx)
		resultCh <- result{path, err}
	}()
	select {
	case command := <-started:
		if command != shellDirectoryCommand(integration) {
			t.Fatalf("query command = %q", command)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("directory query did not reach fake SSH server")
	}
	select {
	case got := <-resultCh:
		if got.path != "" || !errors.Is(got.err, context.DeadlineExceeded) {
			t.Fatalf("timed-out query returned %q, error %v", got.path, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("directory query ignored caller timeout")
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("timed-out directory query left SSH channel open")
	}
}
