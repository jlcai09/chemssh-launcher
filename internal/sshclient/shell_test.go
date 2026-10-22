package sshclient

import (
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"chemssh-launcher/internal/config"
	"chemssh-launcher/internal/secret"

	"golang.org/x/crypto/ssh"
)

func TestInteractiveShellStreamsPTYAndResizes(t *testing.T) {
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("AppData", dir)
	} else {
		t.Setenv("XDG_CONFIG_HOME", dir)
	}

	address, windowChanges := startTestSSHServer(t)
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	profile := config.NewProfileDefaults()
	profile.ID = "shell-test"
	profile.Name = "shell test"
	profile.SSHHost = host
	profile.SSHPort = port
	profile.SSHUser = "tester"
	profile.AuthMethod = config.AuthPassword
	secrets := secret.NewMemoryStore()
	if err := secrets.Set(profile.ID, secret.KeyPassword, "secret"); err != nil {
		t.Fatal(err)
	}

	shell, err := StartInteractiveShell(profile, secrets, HostKeyAcceptNew, 100, 30)
	if err != nil {
		t.Fatalf("start interactive shell: %v", err)
	}
	defer shell.Close()

	output := make(chan string, 64)
	go func() {
		defer close(output)
		buffer := make([]byte, 4096)
		for {
			n, readErr := shell.Output.Read(buffer)
			if n > 0 {
				output <- string(buffer[:n])
			}
			if readErr != nil {
				return
			}
		}
	}()

	readUntil(t, output, "test-shell-ready", 5*time.Second)
	if err := shell.WriteInput([]byte("hello")); err != nil {
		t.Fatalf("write input: %v", err)
	}
	readUntil(t, output, "echo:hello", 5*time.Second)

	if err := shell.Resize(120, 40); err != nil {
		t.Fatalf("resize: %v", err)
	}
	select {
	case size := <-windowChanges:
		if size != [2]uint32{120, 40} {
			t.Fatalf("window change = %v, want [120 40]", size)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for window-change request")
	}
}

func readUntil(t *testing.T, output <-chan string, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	var received strings.Builder
	for {
		select {
		case chunk, ok := <-output:
			if !ok {
				t.Fatalf("output closed before %q arrived; received %q", want, received.String())
			}
			received.WriteString(chunk)
			if strings.Contains(received.String(), want) {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q; received %q", want, received.String())
		}
	}
}

func startTestSSHServer(t *testing.T) (string, <-chan [2]uint32) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	serverConfig := &ssh.ServerConfig{
		PasswordCallback: func(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if conn.User() == "tester" && string(password) == "secret" {
				return nil, nil
			}
			return nil, fmt.Errorf("authentication failed")
		},
	}
	serverConfig.AddHostKey(signer)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	windowChanges := make(chan [2]uint32, 8)
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go serveTestSSHConn(conn, serverConfig, windowChanges)
		}
	}()
	return listener.Addr().String(), windowChanges
}

func serveTestSSHConn(conn net.Conn, serverConfig *ssh.ServerConfig, windowChanges chan<- [2]uint32) {
	_, channels, requests, err := ssh.NewServerConn(conn, serverConfig)
	if err != nil {
		_ = conn.Close()
		return
	}
	go ssh.DiscardRequests(requests)
	for newChannel := range channels {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "unknown channel type")
			continue
		}
		channel, channelRequests, err := newChannel.Accept()
		if err != nil {
			continue
		}
		go handleTestSession(channel, channelRequests, windowChanges)
	}
}

func handleTestSession(channel ssh.Channel, requests <-chan *ssh.Request, windowChanges chan<- [2]uint32) {
	defer channel.Close()
	started := false
	for request := range requests {
		switch request.Type {
		case "pty-req", "shell":
			_ = request.Reply(true, nil)
			if request.Type == "shell" && !started {
				started = true
				go runTestShell(channel)
			}
		case "window-change":
			var size struct {
				Columns uint32
				Rows    uint32
				Width   uint32
				Height  uint32
			}
			if err := ssh.Unmarshal(request.Payload, &size); err == nil {
				select {
				case windowChanges <- [2]uint32{size.Columns, size.Rows}:
				default:
				}
			}
			_ = request.Reply(true, nil)
		case "signal":
			_ = request.Reply(true, nil)
			return
		default:
			_ = request.Reply(false, nil)
		}
	}
}

func runTestShell(channel ssh.Channel) {
	_, _ = channel.Write([]byte("test-shell-ready\r\n"))
	buffer := make([]byte, 1024)
	for {
		n, err := channel.Read(buffer)
		if err != nil {
			return
		}
		data := buffer[:n]
		if strings.Contains(string(data), "exit") {
			_ = channel.Close()
			return
		}
		_, _ = channel.Write([]byte("echo:" + string(data)))
	}
}
