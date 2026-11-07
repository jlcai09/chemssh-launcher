package sshclient

import (
	"bytes"
	"chemssh-launcher/internal/config"
	"chemssh-launcher/internal/secret"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"golang.org/x/crypto/ssh"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestX11CookieHandshake(t *testing.T) {
	fake := bytes.Repeat([]byte{0x11}, 16)
	real := bytes.Repeat([]byte{0x22}, 16)
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		packet := x11Setup(fake, order)
		got, err := readX11Setup(bytes.NewReader(packet), fake, real)
		if err != nil || !bytes.Equal(got, x11Setup(real, order)) {
			t.Fatalf("rewrite %T: %v", order, err)
		}
		for _, kind := range []string{"cookie", "name", "order", "version", "length", "truncated"} {
			bad := append([]byte(nil), packet...)
			switch kind {
			case "cookie":
				bad[len(bad)-1] ^= 1
			case "name":
				bad[12] = '!'
			case "order":
				bad[0] = '?'
			case "version":
				order.PutUint16(bad[2:4], 12)
			case "length":
				order.PutUint16(bad[8:10], 65535)
			case "truncated":
				bad = bad[:len(bad)-1]
			}
			if _, err := readX11Setup(bytes.NewReader(bad), fake, real); err == nil {
				t.Fatalf("accepted invalid %s (%T)", kind, order)
			}
		}
	}
}

func TestXAuthoritySelectionAndDisplay(t *testing.T) {
	d, err := parseX11Display("127.0.0.1:2.1")
	if err != nil || d.address != "127.0.0.1:6002" || d.screen != 1 {
		t.Fatalf("display: %+v %v", d, err)
	}
	for _, bad := range []string{"example.com:0", "192.0.2.1:0", "127.0.0.1:-1", ":59536", ":0.-1", ":0.1.2", "0"} {
		if _, err := parseX11Display(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	cookie := bytes.Repeat([]byte{0x42}, 16)
	wrong := bytes.Repeat([]byte{0x24}, 16)
	data := encodeXAuthority(65535, nil, "2", wrong)
	data = append(data, encodeXAuthority(0, []byte{127, 0, 0, 1}, "2", cookie)...)
	data = append(data, encodeXAuthority(0, []byte{127, 0, 0, 1}, "3", wrong)...)
	got, err := selectXAuthority(data, d)
	if err != nil || !bytes.Equal(got, cookie) {
		t.Fatalf("authority selection: %v", err)
	}
	if _, err := selectXAuthority(data[:len(data)-1], d); err == nil {
		t.Fatal("accepted truncated authority")
	}
	if _, err := selectXAuthority(encodeXAuthority(0, []byte{192, 0, 2, 1}, "2", cookie), d); err == nil {
		t.Fatal("accepted unrelated address")
	}
}

type x11TestServer struct {
	display     string
	cookie      []byte
	forwarded   chan []byte
	connections chan net.Conn
}

func startFakeXServer(t *testing.T) *x11TestServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if port < 6000 {
		_ = listener.Close()
		t.Fatal("ephemeral X server port below 6000")
	}
	s := &x11TestServer{display: fmt.Sprintf("127.0.0.1:%d", port-6000), cookie: bytes.Repeat([]byte{0x52}, 16), forwarded: make(chan []byte, 16), connections: make(chan net.Conn, 64)}
	var all sync.Map
	t.Cleanup(func() { _ = listener.Close(); all.Range(func(k, v any) bool { _ = k.(net.Conn).Close(); return true }) })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			all.Store(conn, true)
			go func() {
				defer all.Delete(conn)
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				packet, err := readX11Setup(conn, s.cookie, s.cookie)
				if err != nil {
					return
				}
				s.forwarded <- packet
				// Minimal successful X11 setup reply, sufficient for auth verification.
				_, _ = conn.Write([]byte{1, 0, 11, 0, 0, 0, 0, 0})
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return s
}
func fakeXAuthority(t *testing.T, s *x11TestServer) string {
	t.Helper()
	d, err := parseX11Display(s.display)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "Xauthority")
	if err := os.WriteFile(path, encodeXAuthority(0, []byte{127, 0, 0, 1}, strconv.Itoa(d.number), s.cookie), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

type x11Request struct {
	SingleConnection bool
	AuthProtocol     string
	AuthCookie       string
	Screen           uint32
}

// Hold inbound packets after an unanswered request, including CHANNEL_CLOSE.
// Closing the client's raw transport still produces EOF without peer cooperation.
type stalledX11Peer struct {
	net.Conn
	blocked atomic.Bool
	resume  chan struct{}
}

func (p *stalledX11Peer) Read(b []byte) (int, error) {
	n, err := p.Conn.Read(b)
	if n > 0 && err == nil && p.blocked.Load() {
		<-p.resume
	}
	return n, err
}

type x11SSHFixture struct {
	profile     config.Profile
	secrets     secret.Store
	server      chan *ssh.ServerConn
	requests    chan x11Request
	events      chan string
	earlyOpen   chan ssh.Channel
	withholdX11 atomic.Bool
	peers       chan *stalledX11Peer
}

func startX11SSHFixture(t *testing.T, rejectX11 bool, integrationRetry bool) *x11SSHFixture {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) { return nil, nil }}
	cfg.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	fixture := &x11SSHFixture{server: make(chan *ssh.ServerConn, 4), peers: make(chan *stalledX11Peer, 4), requests: make(chan x11Request, 8), events: make(chan string, 32), earlyOpen: make(chan ssh.Channel, 8)}
	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	profile := config.NewProfileDefaults()
	profile.ID = "x11-test"
	profile.Name = "X11 test"
	profile.SSHHost = host
	profile.SSHPort = port
	profile.SSHUser = "user"
	profile.AuthMethod = config.AuthPassword
	fixture.profile = profile
	fixture.secrets = secret.NewMemoryStore()
	if err := fixture.secrets.Set(profile.ID, secret.KeyPassword, "password"); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				peer := &stalledX11Peer{Conn: conn, resume: make(chan struct{})}
				t.Cleanup(func() { close(peer.resume); _ = conn.Close() })
				server, channels, requests, err := ssh.NewServerConn(peer, cfg)
				if err != nil {
					_ = conn.Close()
					return
				}
				defer server.Close()
				fixture.server <- server
				fixture.peers <- peer
				go ssh.DiscardRequests(requests)
				for next := range channels {
					if next.ChannelType() != "session" {
						_ = next.Reject(ssh.UnknownChannelType, "unsupported")
						continue
					}
					channel, requests, err := next.Accept()
					if err != nil {
						continue
					}
					go func() {
						defer channel.Close()
						pty := false
						for req := range requests {
							switch req.Type {
							case "pty-req":
								pty = true
								fixture.events <- "pty"
								_ = req.Reply(true, nil)
							case "x11-req":
								var payload x11Request
								valid := ssh.Unmarshal(req.Payload, &payload) == nil && pty
								if valid {
									fixture.requests <- payload
									fixture.events <- "x11"
								}
								if valid && !rejectX11 && integrationRetry {
									// Open before replying: handler registration must precede x11-req.
									xchannel, xrequests, openErr := server.OpenChannel("x11", ssh.Marshal(struct {
										Address string
										Port    uint32
									}{"127.0.0.1", 4321}))
									if openErr == nil {
										go ssh.DiscardRequests(xrequests)
										fixture.earlyOpen <- xchannel
									}
								}
								if fixture.withholdX11.Load() {
									peer.blocked.Store(true)
								} else {
									_ = req.Reply(valid && !rejectX11, nil)
								}
							case "exec":
								var command struct{ Command string }
								_ = ssh.Unmarshal(req.Payload, &command)
								if strings.Contains(command.Command, "base64 -d | /bin/sh") && integrationRetry {
									_ = req.Reply(true, nil)
									_, _ = io.WriteString(channel, "CHEMSSH_LAUNCHER_SHELL=/bin/bash\nCHEMSSH_LAUNCHER_SHIM=/tmp/example-shim\n")
									_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
									return
								}
								if pty {
									fixture.events <- "exec"
									_ = req.Reply(false, nil)
								} else if strings.HasPrefix(command.Command, "rm -rf -- ") {
									_ = req.Reply(true, nil)
									_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
									return
								} else {
									_ = req.Reply(false, nil)
								}
							case "shell":
								fixture.events <- "shell"
								_ = req.Reply(true, nil)
							default:
								_ = req.Reply(false, nil)
							}
						}
					}()
				}
			}(conn)
		}
	}()
	return fixture
}
func configureX11TestHome(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Setenv("AppData", t.TempDir())
	} else {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	}
}
func waitX11Request(t *testing.T, fixture *x11SSHFixture) x11Request {
	t.Helper()
	select {
	case req := <-fixture.requests:
		return req
	case <-time.After(3 * time.Second):
		t.Fatal("X11 request missing")
		return x11Request{}
	}
}
func openTestX11(t *testing.T, server *ssh.ServerConn) ssh.Channel {
	t.Helper()
	type result struct {
		channel ssh.Channel
		err     error
	}
	ch := make(chan result, 1)
	go func() {
		channel, requests, err := server.OpenChannel("x11", ssh.Marshal(struct {
			Address string
			Port    uint32
		}{"127.0.0.1", 4321}))
		if err == nil {
			go ssh.DiscardRequests(requests)
		}
		ch <- result{channel, err}
	}()
	select {
	case got := <-ch:
		if got.err != nil {
			t.Fatal(got.err)
		}
		return got.channel
	case <-time.After(3 * time.Second):
		t.Fatal("X11 channel open blocked")
		return nil
	}
}
func TestInteractiveShellX11ProtocolRetryAndCleanup(t *testing.T) {
	configureX11TestHome(t)
	xserver := startFakeXServer(t)
	fixture := startX11SSHFixture(t, false, true)
	fixture.profile.X11 = config.X11Config{Enabled: true, Display: xserver.display, XAuthority: fakeXAuthority(t, xserver)}
	shell, err := StartInteractiveShell(fixture.profile, fixture.secrets, HostKeyAcceptNew, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer shell.Close()
	if !shell.X11Enabled || shell.X11Error != "" {
		t.Fatalf("X11 state: %v %q", shell.X11Enabled, shell.X11Error)
	}
	first := waitX11Request(t, fixture)
	second := waitX11Request(t, fixture)
	if first.AuthProtocol != x11AuthProtocol || first.SingleConnection || first.AuthCookie != second.AuthCookie {
		t.Fatal("invalid x11-req payload or missing retry")
	}
	fake, err := hex.DecodeString(second.AuthCookie)
	if err != nil || len(fake) != 16 || bytes.Equal(fake, xserver.cookie) {
		t.Fatal("SSH request did not use synthetic cookie")
	}
	// Both initial and retry PTYs request forwarding before attempting a shell.
	want := []string{"pty", "x11", "exec", "pty", "x11", "shell"}
	for _, event := range want {
		select {
		case got := <-fixture.events:
			if got != event {
				t.Fatalf("event %q want %q", got, event)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("missing shell event")
		}
	}
	server := <-fixture.server
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		var channel ssh.Channel
		select {
		case channel = <-fixture.earlyOpen:
		case <-time.After(3 * time.Second):
			t.Fatal("incoming X11 before request reply was rejected")
		}
		if err := writeX11All(channel, x11Setup(fake, order)); err != nil {
			t.Fatal(err)
		}
		response := make([]byte, 8)
		done := make(chan error, 1)
		go func() { _, err := io.ReadFull(channel, response); done <- err }()
		select {
		case err := <-done:
			if err != nil || response[0] != 1 {
				t.Fatalf("local X11 setup: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("X11 forwarding blocked")
		}
		// Verify traffic after the handshake remains bidirectional.
		if _, err := channel.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		echoed := make([]byte, 4)
		if _, err := io.ReadFull(channel, echoed); err != nil || string(echoed) != "ping" {
			t.Fatalf("echo: %q %v", echoed, err)
		}
		_ = channel.Close()
	}
	invalid := openTestX11(t, server)
	_ = writeX11All(invalid, x11Setup(bytes.Repeat([]byte{0x33}, 16), binary.LittleEndian))
	invalidClosed := make(chan error, 1)
	go func() { var one [1]byte; _, err := invalid.Read(one[:]); invalidClosed <- err }()
	select {
	case err := <-invalidClosed:
		if err == nil {
			t.Fatal("invalid cookie accepted")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("invalid cookie channel was not closed")
	}
	pending := openTestX11(t, server) // Deliberately leave setup incomplete.
	if err := shell.Close(); err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { var one [1]byte; _, err := pending.Read(one[:]); closed <- err }()
	select {
	case err := <-closed:
		if err == nil {
			t.Fatal("pending X11 channel stayed open")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("session close leaked X11 channel")
	}
	select {
	case <-shell.x11.stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("X11 workers did not exit after session close")
	}
	shell.x11.mu.Lock()
	remaining := len(shell.x11.active)
	shell.x11.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("leaked %d active X11 resources", remaining)
	}
	if shell.x11.track(&x11TestCloser{}) {
		t.Fatal("closed controller accepted a resource")
	}
}

type x11TestCloser struct{}

func (*x11TestCloser) Close() error { return nil }

func TestInteractiveShellX11Degrades(t *testing.T) {
	for _, mode := range []string{"rejected", "bad-cookie", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			configureX11TestHome(t)
			xserver := startFakeXServer(t)
			fixture := startX11SSHFixture(t, mode == "rejected", false)
			path := fakeXAuthority(t, xserver)
			if mode == "bad-cookie" {
				d, _ := parseX11Display(xserver.display)
				if err := os.WriteFile(path, encodeXAuthority(0, []byte{127, 0, 0, 1}, strconv.Itoa(d.number), bytes.Repeat([]byte{0x33}, 16)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			fixture.profile.X11 = config.X11Config{Enabled: mode != "disabled", Display: xserver.display, XAuthority: path}
			shell, err := StartInteractiveShell(fixture.profile, fixture.secrets, HostKeyAcceptNew, 80, 24)
			if err != nil {
				t.Fatal(err)
			}
			defer shell.Close()
			if shell.X11Enabled {
				t.Fatal("degraded X11 reported enabled")
			}
			if mode == "disabled" {
				if shell.X11Error != "" {
					t.Fatal(shell.X11Error)
				}
			} else if shell.X11Error == "" {
				t.Fatal("missing degradation reason")
			}
			if mode == "rejected" {
				_ = waitX11Request(t, fixture)
			} else {
				select {
				case <-fixture.requests:
					t.Fatal("unexpected x11-req without usable local auth")
				default:
				}
			}
		})
	}
}
