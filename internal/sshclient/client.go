package sshclient

import (
	"fmt"
	"io"
	"os"
	"time"

	"chemweb-launcher/internal/config"
	"chemweb-launcher/internal/secret"

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
	return ssh.Dial("tcp", fmt.Sprintf("%s:%d", profile.SSHHost, profile.SSHPort), cfg)
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

func (p *RemoteProcess) Done() <-chan error {
	return p.done
}

func (p *RemoteProcess) Stop() error {
	if p == nil || p.Session == nil {
		return nil
	}
	_ = StopRemoteCommand(p.Client, p.Profile)
	_ = p.Session.Signal(ssh.SIGTERM)
	select {
	case <-p.closed:
		return nil
	case <-time.After(7 * time.Second):
	}
	return p.Session.Close()
}

func StopRemoteCommand(client *ssh.Client, profile config.Profile) error {
	if client == nil {
		return nil
	}
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()
	return session.Run(AssembleStopCommand(profile))
}
