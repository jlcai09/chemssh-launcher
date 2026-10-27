package gui

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"chemssh-launcher/internal/config"
	appruntime "chemssh-launcher/internal/runtime"
	"chemssh-launcher/internal/secret"

	"golang.org/x/crypto/ssh"
)

func TestStatusReportsFailureWhenRemotePortCheckFails(t *testing.T) {
	useTempConfigHome(t)
	sshAddress, closeSSH := startScriptedSSHServer(t, func(int) (bool, string) { return false, "" })
	defer closeSSH()

	profile := phaseTestRemoteProfile(t, sshAddress)
	profile.LocalPort = freeLocalPort(t)

	profiles := config.NewFileStore(filepath.Join(t.TempDir(), "profiles.json"))
	if err := profiles.Save(profile); err != nil {
		t.Fatal(err)
	}
	secrets := secret.NewMemoryStore()
	if err := secrets.Set(profile.ID, secret.KeyPassword, "password"); err != nil {
		t.Fatal(err)
	}
	server := NewServer(&appruntime.Runtime{Profiles: profiles, Secrets: secrets})

	body := bytes.NewBufferString(`{"id":"` + profile.ID + `","accept_host_key":true}`)
	rec := httptest.NewRecorder()
	server.handleStart(rec, httptest.NewRequest(http.MethodPost, "/api/session/start", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("start status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	status := waitForStatusPhase(t, server, profile.ID, "failed")
	if status["running"] != false || status["forwarding"] != false || status["ready"] != false {
		t.Fatalf("failed start still reported an active session: %+v", status)
	}
	if status["pending_start"] != false {
		t.Fatalf("pending_start = %v after failure, want false", status["pending_start"])
	}
	failureMessage := statusFailureMessage(t, status)
	if !strings.Contains(failureMessage, "port check") {
		t.Fatalf("last_failure.message = %q, want port check failure", failureMessage)
	}
}

func TestStatusReportsFailureWhenRemoteCommandStartFails(t *testing.T) {
	sshAddress, closeSSH := startScriptedSSHServer(t, func(index int) (bool, string) {
		if index == 0 {
			return true, "remote port is available"
		}
		return false, ""
	})
	defer closeSSH()

	profile := phaseTestRemoteProfile(t, sshAddress)
	profile.LocalPort = freeLocalPort(t)
	server := NewServer(&appruntime.Runtime{})

	server.markStartPending(profile.ID)
	server.startRemoteSessionAsync(profile, dialPhaseTestSSH(t, sshAddress), io.Discard)

	status := waitForStatusPhase(t, server, profile.ID, "failed")
	if status["running"] != false || status["ready"] != false {
		t.Fatalf("failed start still reported an active session: %+v", status)
	}
	failureMessage := statusFailureMessage(t, status)
	if !strings.Contains(failureMessage, "remote command") {
		t.Fatalf("last_failure.message = %q, want remote command failure", failureMessage)
	}
}

func TestStatusReportsFailureWhenTunnelStartFails(t *testing.T) {
	// The port check reports a reusable ChemSSH server, so no remote command is
	// started and only the tunnel creation can fail here.
	sshAddress, closeSSH := startScriptedSSHServer(t, func(int) (bool, string) {
		return true, "port is used by a reusable ChemSSH server"
	})
	defer closeSSH()

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	profile := phaseTestRemoteProfile(t, sshAddress)
	profile.LocalPort = occupied.Addr().(*net.TCPAddr).Port
	server := NewServer(&appruntime.Runtime{})

	server.markStartPending(profile.ID)
	server.startRemoteSessionAsync(profile, dialPhaseTestSSH(t, sshAddress), io.Discard)

	status := waitForStatusPhase(t, server, profile.ID, "failed")
	if status["running"] != false || status["ready"] != false {
		t.Fatalf("failed start still reported an active session: %+v", status)
	}
	failureMessage := statusFailureMessage(t, status)
	if !strings.Contains(failureMessage, "tunnel") {
		t.Fatalf("last_failure.message = %q, want tunnel failure", failureMessage)
	}
}

func TestStatusPhaseMovesFromStartingToReady(t *testing.T) {
	server := NewServer(&appruntime.Runtime{})
	profile := config.NewProfileDefaults()
	profile.ID = "success"
	profile.Name = "Success"

	server.markStartPending(profile.ID)
	status := fetchSessionStatus(t, server, profile.ID)
	if status["phase"] != "starting" || status["pending_start"] != true || status["ready"] != false {
		t.Fatalf("pending start status = %+v, want phase starting", status)
	}

	session := &activeSession{id: profile.ID, name: profile.Name, profile: profile, forwarding: true}
	server.mu.Lock()
	server.setSessionLocked(session)
	server.mu.Unlock()
	server.clearStartPending(profile.ID)

	status = fetchSessionStatus(t, server, profile.ID)
	if status["phase"] != "starting" || status["ready"] != false || status["pending_start"] != false {
		t.Fatalf("starting status = %+v, want phase starting without pending_start", status)
	}

	server.recordSessionFailure(profile.ID, "previous failure")
	server.markSessionReady(session)

	status = fetchSessionStatus(t, server, profile.ID)
	if status["phase"] != "ready" || status["ready"] != true {
		t.Fatalf("ready status = %+v, want phase ready", status)
	}
	if status["last_failure"] != nil {
		t.Fatalf("last_failure = %v after ready, want null", status["last_failure"])
	}
}

func TestStatusPhaseIsPausedAfterStopForwarding(t *testing.T) {
	server := NewServer(&appruntime.Runtime{})
	profile := config.NewProfileDefaults()
	profile.ID = "paused"
	profile.Name = "Paused"
	session := &activeSession{
		id:         profile.ID,
		name:       profile.Name,
		profile:    profile,
		forwarding: true,
		ready:      true,
		remotePID:  4242,
	}
	server.mu.Lock()
	server.setSessionLocked(session)
	server.mu.Unlock()

	status := fetchSessionStatus(t, server, profile.ID)
	if status["phase"] != "ready" {
		t.Fatalf("phase = %v before stop forward, want ready", status["phase"])
	}

	server.stopForwarding(profile.ID)

	status = fetchSessionStatus(t, server, profile.ID)
	if status["running"] != true || status["forwarding"] != false || status["ready"] != false {
		t.Fatalf("paused status = %+v, want running session without forwarding", status)
	}
	if status["phase"] != "paused" {
		t.Fatalf("phase = %v after stop forward, want paused", status["phase"])
	}
	if status["url"] != "" {
		t.Fatalf("url = %v while forwarding is stopped, want empty", status["url"])
	}
}

func TestStatusReportsFailureWhenLocalCommandCrashes(t *testing.T) {
	spec := localExitFailureCommand()
	if _, err := exec.LookPath(spec[0]); err != nil {
		t.Skip("no shell available to spawn a failing process")
	}
	server := NewServer(&appruntime.Runtime{})
	profile := config.NewProfileDefaults()
	profile.ID = "local-crash"
	profile.Name = "Local Crash"
	profile.Kind = config.ProfileKindLocal

	cmd := exec.Command(spec[0], spec[1:]...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start failing command: %v", err)
	}
	session := &activeSession{
		id:           profile.ID,
		name:         profile.Name,
		profile:      profile,
		localProcess: cmd,
		forwarding:   true,
		ready:        true,
	}
	server.mu.Lock()
	server.setSessionLocked(session)
	server.mu.Unlock()

	server.watchLocalProcess(session, cmd)

	status := fetchSessionStatus(t, server, profile.ID)
	if status["running"] != false {
		t.Fatalf("status = %+v after local crash, want no session", status)
	}
	if status["phase"] != "failed" {
		t.Fatalf("phase = %v after local crash, want failed; status: %+v", status["phase"], status)
	}
	if message := statusFailureMessage(t, status); !strings.Contains(message, "local ChemSSH command stopped with error") {
		t.Fatalf("last_failure.message = %q, want local command failure", message)
	}
}

// localExitFailureCommand returns a command that exits with a non-zero status.
func localExitFailureCommand() []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd", "/c", "exit 1"}
	}
	return []string{"/bin/sh", "-c", "exit 1"}
}

func TestStatusesReportsEveryProfileWithoutPromoting(t *testing.T) {
	server := NewServer(&appruntime.Runtime{})
	newProfile := func(id string) config.Profile {
		profile := config.NewProfileDefaults()
		profile.ID = id
		profile.Name = id
		return profile
	}

	readyProfile := newProfile("ready-profile")
	readySession := &activeSession{id: readyProfile.ID, name: readyProfile.Name, profile: readyProfile, forwarding: true, ready: true}
	pausedProfile := newProfile("paused-profile")
	pausedSession := &activeSession{id: pausedProfile.ID, name: pausedProfile.Name, profile: pausedProfile, forwarding: false, remotePID: 4242}

	server.mu.Lock()
	server.setSessionLocked(readySession)
	server.setSessionLocked(pausedSession)
	server.mu.Unlock()
	server.markStartPending("starting-profile")
	server.recordSessionFailure("failed-profile", "remote command failed")

	// Polling must not promote anything into the proxy slot.
	server.mu.Lock()
	server.proxyProfileID = "untouched"
	server.mu.Unlock()

	rec := httptest.NewRecorder()
	server.handleStatuses(rec, httptest.NewRequest(http.MethodGet, "/api/session/statuses", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("statuses = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body struct {
		Statuses map[string]struct {
			Running bool   `json:"running"`
			Phase   string `json:"phase"`
			Name    string `json:"name"`
		} `json:"statuses"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode statuses: %v", err)
	}
	want := map[string]string{
		"ready-profile":    "ready",
		"paused-profile":   "paused",
		"starting-profile": "starting",
		"failed-profile":   "failed",
	}
	if len(body.Statuses) != len(want) {
		t.Fatalf("statuses = %+v, want %d entries", body.Statuses, len(want))
	}
	for id, phase := range want {
		entry, ok := body.Statuses[id]
		if !ok {
			t.Fatalf("statuses missing %q: %+v", id, body.Statuses)
		}
		if entry.Phase != phase {
			t.Fatalf("%s phase = %q, want %q", id, entry.Phase, phase)
		}
	}
	if body.Statuses["ready-profile"].Running != true || body.Statuses["failed-profile"].Running != false {
		t.Fatalf("running flags are wrong: %+v", body.Statuses)
	}

	server.mu.Lock()
	promoted := server.proxyProfileID
	server.mu.Unlock()
	if promoted != "untouched" {
		t.Fatalf("proxyProfileID = %q after polling, want it untouched", promoted)
	}
}

func TestStatusWithoutProfileOrSessionIsStopped(t *testing.T) {
	server := NewServer(&appruntime.Runtime{})

	status := fetchSessionStatus(t, server, "")

	if status["phase"] != "stopped" || status["ready"] != false || status["pending_start"] != false || status["last_failure"] != nil {
		t.Fatalf("empty status = %+v, want stopped with empty extras", status)
	}
}

func useTempConfigHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("AppData", dir)
	} else {
		t.Setenv("XDG_CONFIG_HOME", dir)
	}
}

func phaseTestRemoteProfile(t *testing.T, sshAddress string) config.Profile {
	t.Helper()
	host, portText, err := net.SplitHostPort(sshAddress)
	if err != nil {
		t.Fatal(err)
	}
	sshPort, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	profile := config.NewProfileDefaults()
	profile.ID = "phase-remote"
	profile.Name = "Phase Remote"
	profile.SSHHost = host
	profile.SSHPort = sshPort
	profile.SSHUser = "user"
	profile.AuthMethod = config.AuthPassword
	profile.HasPassword = true
	profile.OpenBrowser = false
	return profile
}

func dialPhaseTestSSH(t *testing.T, sshAddress string) *ssh.Client {
	t.Helper()
	cfg := &ssh.ClientConfig{
		User:            "user",
		Auth:            []ssh.AuthMethod{ssh.Password("password")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}
	conn, err := net.DialTimeout("tcp", sshAddress, 5*time.Second)
	if err != nil {
		t.Fatalf("dial test ssh server: %v", err)
	}
	clientConn, chans, reqs, err := ssh.NewClientConn(conn, sshAddress, cfg)
	if err != nil {
		_ = conn.Close()
		t.Fatalf("handshake with test ssh server: %v", err)
	}
	client := ssh.NewClient(clientConn, chans, reqs)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func fetchSessionStatus(t *testing.T, server *Server, profileID string) map[string]any {
	t.Helper()
	target := "/api/session/status"
	if profileID != "" {
		target += "?profile_id=" + url.QueryEscape(profileID)
	}
	rec := httptest.NewRecorder()
	server.handleStatus(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode status body: %v", err)
	}
	return body
}

func waitForStatusPhase(t *testing.T, server *Server, profileID, phase string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var status map[string]any
	for {
		status = fetchSessionStatus(t, server, profileID)
		if status["phase"] == phase {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("phase = %v, want %q; status: %+v", status["phase"], phase, status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func statusFailureMessage(t *testing.T, status map[string]any) string {
	t.Helper()
	failure, ok := status["last_failure"].(map[string]any)
	if !ok {
		t.Fatalf("last_failure = %v, want an object", status["last_failure"])
	}
	message, _ := failure["message"].(string)
	if message == "" {
		t.Fatalf("last_failure.message is empty: %+v", failure)
	}
	if at, _ := failure["at"].(string); at == "" {
		t.Fatalf("last_failure.at is empty: %+v", failure)
	}
	return message
}

// startScriptedSSHServer starts a minimal SSH server that accepts any password
// and lets the test decide whether the exec request of each session succeeds and
// what it prints.
func startScriptedSSHServer(t *testing.T, script func(index int) (bool, string)) (string, func()) {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			return nil, nil
		},
	}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var sessions atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveScriptedSSHConn(conn, cfg, &sessions, script)
		}
	}()
	return ln.Addr().String(), func() {
		_ = ln.Close()
		<-done
	}
}

func serveScriptedSSHConn(conn net.Conn, cfg *ssh.ServerConfig, sessions *atomic.Int32, script func(int) (bool, string)) {
	sshConn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		_ = conn.Close()
		return
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(reqs)
	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "test server only accepts session channels")
			continue
		}
		channel, channelRequests, err := newChannel.Accept()
		if err != nil {
			continue
		}
		index := int(sessions.Add(1)) - 1
		go serveScriptedSSHSession(channel, channelRequests, index, script)
	}
}

func serveScriptedSSHSession(channel ssh.Channel, requests <-chan *ssh.Request, index int, script func(int) (bool, string)) {
	defer channel.Close()
	execAccepted := false
	for req := range requests {
		switch req.Type {
		case "exec":
			accept, output := script(index)
			if execAccepted || !accept {
				_ = req.Reply(false, nil)
				continue
			}
			execAccepted = true
			_ = req.Reply(true, nil)
			if output != "" {
				_, _ = channel.Write([]byte(output))
			}
			// Let the launcher treat the remote command as running, then exit
			// cleanly so RemoteProcess.Stop returns without waiting for a timeout.
			time.AfterFunc(150*time.Millisecond, func() {
				_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
				_ = channel.Close()
			})
		case "signal":
			_ = req.Reply(true, nil)
		default:
			_ = req.Reply(false, nil)
		}
	}
}
