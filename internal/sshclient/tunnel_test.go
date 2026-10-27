package sshclient

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestStartReverseTunnelForwardsToLocalTarget(t *testing.T) {
	address := startForwardingTestSSHServer(t)
	client, err := ssh.Dial("tcp", address, &ssh.ClientConfig{
		User:            "tester",
		Auth:            []ssh.AuthMethod{ssh.Password("secret")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial test server: %v", err)
	}
	defer client.Close()

	echoAddr := startEchoServer(t)
	tunnel, proxyAddr, err := StartReverseTunnel(context.Background(), client, echoAddr)
	if err != nil {
		t.Fatalf("start reverse tunnel: %v", err)
	}
	defer tunnel.Close()

	host, portText, err := net.SplitHostPort(proxyAddr)
	if err != nil {
		t.Fatalf("reverse address %q: %v", proxyAddr, err)
	}
	if host != "127.0.0.1" {
		t.Fatalf("reverse listener bound on %q, want loopback", host)
	}
	if port, err := strconv.Atoi(portText); err != nil || port == 0 {
		t.Fatalf("reverse listener port = %q, want a server-assigned port", portText)
	}

	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial reverse listener: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write through reverse tunnel: %v", err)
	}
	reply := make([]byte, 4)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("read through reverse tunnel: %v", err)
	}
	if string(reply) != "ping" {
		t.Fatalf("reply = %q", reply)
	}
}

func TestStartReverseTunnelRequiresClient(t *testing.T) {
	if _, _, err := StartReverseTunnel(context.Background(), nil, "127.0.0.1:7890"); err == nil {
		t.Fatal("expected an error without an ssh client")
	}
}

func startEchoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	return ln.Addr().String()
}

func startForwardingTestSSHServer(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if conn.User() == "tester" && string(password) == "secret" {
				return nil, nil
			}
			return nil, fmt.Errorf("authentication failed")
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveForwardingTestSSHConn(conn, cfg)
		}
	}()
	return ln.Addr().String()
}

func serveForwardingTestSSHConn(conn net.Conn, cfg *ssh.ServerConfig) {
	serverConn, channels, requests, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		_ = conn.Close()
		return
	}
	go func() {
		for newChannel := range channels {
			_ = newChannel.Reject(ssh.UnknownChannelType, "unsupported")
		}
	}()

	var mu sync.Mutex
	forwards := make(map[string]net.Listener)
	for request := range requests {
		switch request.Type {
		case "tcpip-forward":
			var payload struct {
				Addr string
				Port uint32
			}
			if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
				_ = request.Reply(false, nil)
				continue
			}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				_ = request.Reply(false, nil)
				continue
			}
			port := uint32(ln.Addr().(*net.TCPAddr).Port)
			key := net.JoinHostPort(payload.Addr, strconv.Itoa(int(port)))
			mu.Lock()
			forwards[key] = ln
			mu.Unlock()
			_ = request.Reply(true, ssh.Marshal(struct{ Port uint32 }{Port: port}))
			go serveForwardedTestConns(serverConn, ln, payload.Addr, port)
		case "cancel-tcpip-forward":
			var payload struct {
				Addr string
				Port uint32
			}
			_ = ssh.Unmarshal(request.Payload, &payload)
			key := net.JoinHostPort(payload.Addr, strconv.Itoa(int(payload.Port)))
			mu.Lock()
			ln := forwards[key]
			delete(forwards, key)
			mu.Unlock()
			if ln != nil {
				_ = ln.Close()
			}
			_ = request.Reply(true, nil)
		default:
			_ = request.Reply(false, nil)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	for _, ln := range forwards {
		_ = ln.Close()
	}
}

func serveForwardedTestConns(serverConn *ssh.ServerConn, ln net.Listener, bindAddr string, bindPort uint32) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			payload := struct {
				Addr       string
				Port       uint32
				OriginAddr string
				OriginPort uint32
			}{Addr: bindAddr, Port: bindPort, OriginAddr: "127.0.0.1", OriginPort: 45678}
			channel, requests, err := serverConn.OpenChannel("forwarded-tcpip", ssh.Marshal(&payload))
			if err != nil {
				return
			}
			go ssh.DiscardRequests(requests)
			done := make(chan struct{}, 2)
			go func() {
				_, _ = io.Copy(channel, c)
				_ = channel.CloseWrite()
				done <- struct{}{}
			}()
			go func() {
				_, _ = io.Copy(c, channel)
				done <- struct{}{}
			}()
			<-done
		}(conn)
	}
}

func TestProxyBridgePathUsesPortAndHome(t *testing.T) {
	got := ProxyBridgePath("/home/user", 8888)
	want := "/home/user/.chemssh-launcher/terminal-proxy-8888.json"
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

func TestProxyBridgePayloadMatchesContract(t *testing.T) {
	env := newProxyBridgeFile("http://127.0.0.1:41000", "localhost,127.0.0.1")
	if env.Version != proxyBridgeVersion {
		t.Fatalf("version = %d", env.Version)
	}
	if env.ProxyURL != "http://127.0.0.1:41000" || env.NoProxy != "localhost,127.0.0.1" {
		t.Fatalf("payload = %+v", env)
	}
	if env.Env["http_proxy"] != "http://127.0.0.1:41000" || env.Env["HTTPS_PROXY"] != "http://127.0.0.1:41000" {
		t.Fatalf("env = %v", env.Env)
	}
	if env.Env["no_proxy"] != "localhost,127.0.0.1" || env.Env["NO_PROXY"] != "localhost,127.0.0.1" {
		t.Fatalf("no_proxy env = %v", env.Env)
	}
}

func TestWriteProxyBridgeRequiresClient(t *testing.T) {
	if _, err := WriteProxyBridge(nil, 8888, "http://127.0.0.1:41000", ""); err == nil {
		t.Fatal("expected an error without an ssh client")
	}
	if err := RemoveProxyBridge(nil, "/home/user/.chemssh-launcher/terminal-proxy-8888.json"); err != nil {
		t.Fatalf("nil client removal should be a no-op: %v", err)
	}
}
