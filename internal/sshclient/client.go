package sshclient

import (
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"chemssh-launcher/internal/config"
	"chemssh-launcher/internal/secret"

	"golang.org/x/crypto/ssh"
)

func Dial(profile config.Profile, secrets secret.Store) (*ssh.Client, error) {
	return DialWithHostKeyPolicy(profile, secrets, HostKeyStrict)
}

func DialWithHostKeyPolicy(profile config.Profile, secrets secret.Store, policy HostKeyPolicy) (*ssh.Client, error) {
	cfg, err := ClientConfigWithHostKeyPolicy(profile, secrets, policy)
	if err != nil {
		return nil, err
	}
	address := fmt.Sprintf("%s:%d", profile.SSHHost, profile.SSHPort)
	dialer := &net.Dialer{
		Timeout:       15 * time.Second,
		FallbackDelay: 300 * time.Millisecond,
	}
	conn, err := dialer.Dial("tcp", address)
	if err != nil {
		return nil, err
	}
	clientConn, chans, reqs, err := ssh.NewClientConn(conn, address, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return ssh.NewClient(clientConn, chans, reqs), nil
}

type RemoteProcess struct {
	Session *ssh.Session
	Client  *ssh.Client
	Profile config.Profile
	done    chan error
	closed  chan struct{}
}

func StartRemoteCommand(client *ssh.Client, profile config.Profile, stdout, stderr io.Writer) (*RemoteProcess, error) {
	session, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	session.Stdout = stdout
	session.Stderr = stderr

	stdin, err := session.StdinPipe()
	if err != nil {
		session.Close()
		return nil, err
	}
	if err := session.Start("bash -s"); err != nil {
		session.Close()
		return nil, err
	}
	if _, err := io.WriteString(stdin, AssembleRemoteCommand(profile)); err != nil {
		session.Close()
		return nil, err
	}
	stdin.Close()

	proc := &RemoteProcess{Session: session, Client: client, Profile: profile, done: make(chan error, 1), closed: make(chan struct{})}
	go func() {
		proc.done <- session.Wait()
		close(proc.closed)
	}()
	return proc, nil
}

type CheckPortResult struct {
	Reusable bool
	Output   string
}

func RunCheckPortCommand(client *ssh.Client, profile config.Profile, stdout, stderr io.Writer) (CheckPortResult, error) {
	session, err := client.NewSession()
	if err != nil {
		return CheckPortResult{}, err
	}
	defer session.Close()
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	var output strings.Builder
	writer := io.MultiWriter(&output, stdout)
	session.Stdout = writer
	session.Stderr = io.MultiWriter(&output, stderr)

	stdin, err := session.StdinPipe()
	if err != nil {
		return CheckPortResult{}, err
	}
	if err := session.Start("bash -s"); err != nil {
		return CheckPortResult{}, err
	}
	if _, err := io.WriteString(stdin, AssembleCheckPortCommand(profile)); err != nil {
		_ = stdin.Close()
		return CheckPortResult{}, err
	}
	_ = stdin.Close()
	err = session.Wait()
	text := output.String()
	return CheckPortResult{
		Reusable: strings.Contains(text, "used by a reusable ChemSSH server"),
		Output:   text,
	}, err
}

func (p *RemoteProcess) Done() <-chan error {
	return p.done
}

func (p *RemoteProcess) Stop() error {
	if p == nil || p.Session == nil {
		return nil
	}
	_ = p.Session.Signal(ssh.SIGTERM)
	select {
	case <-p.closed:
		return nil
	case <-time.After(7 * time.Second):
	}
	return p.Session.Close()
}

func StopRemotePID(client *ssh.Client, pid int) error {
	if client == nil {
		return nil
	}
	if pid <= 0 {
		return fmt.Errorf("invalid remote pid %d", pid)
	}
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()
	return session.Run(AssembleKillPIDCommand(pid))
}
