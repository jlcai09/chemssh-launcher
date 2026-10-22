package sshclient

import (
	"fmt"
	"io"
	"sync"

	"chemssh-launcher/internal/config"
	"chemssh-launcher/internal/secret"

	"golang.org/x/crypto/ssh"
)

// InteractiveShell is an SSH session with a remote PTY running the user's
// login shell. It is independent of ChemSSH and exists so a server can be
// debugged before ChemSSH is installed.
type InteractiveShell struct {
	Client           *ssh.Client
	Session          *ssh.Session
	Stdin            io.WriteCloser
	Output           io.Reader
	IntegrationError error

	integration  *shellIntegration
	outputWriter *io.PipeWriter
	writeMu      sync.Mutex
	closeOnce    sync.Once
	closeErr     error
}

func StartInteractiveShell(profile config.Profile, secrets secret.Store, policy HostKeyPolicy, cols, rows int) (*InteractiveShell, error) {
	client, err := DialWithHostKeyPolicy(profile, secrets, policy)
	if err != nil {
		return nil, err
	}
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	integration, integrationErr := prepareShellIntegration(client)
	if integrationErr != nil {
		integration = nil
	}

	outputReader, outputWriter := io.Pipe()
	openSession := func() (*ssh.Session, io.WriteCloser, error) {
		session, openErr := client.NewSession()
		if openErr != nil {
			return nil, nil, openErr
		}
		modes := ssh.TerminalModes{
			ssh.ECHO:          1,
			ssh.TTY_OP_ISPEED: 14400,
			ssh.TTY_OP_OSPEED: 14400,
		}
		if ptyErr := session.RequestPty("xterm-256color", rows, cols, modes); ptyErr != nil {
			_ = session.Close()
			return nil, nil, fmt.Errorf("request remote pty: %w", ptyErr)
		}
		stdin, stdinErr := session.StdinPipe()
		if stdinErr != nil {
			_ = session.Close()
			return nil, nil, stdinErr
		}
		session.Stdout = outputWriter
		session.Stderr = outputWriter
		return session, stdin, nil
	}

	session, stdin, err := openSession()
	if err != nil {
		_ = outputWriter.Close()
		_ = client.Close()
		return nil, err
	}

	integrationStarted := false
	if integration != nil {
		if startErr := session.Start(interactiveShellCommand(integration)); startErr == nil {
			integrationStarted = true
		} else {
			integrationErr = fmt.Errorf("start shell integration: %w", startErr)
			_ = session.Close()
			session, stdin, err = openSession()
			if err != nil {
				_ = outputWriter.Close()
				_ = client.Close()
				return nil, err
			}
		}
	}
	if !integrationStarted {
		if err := session.Shell(); err != nil {
			_ = outputWriter.Close()
			_ = session.Close()
			_ = client.Close()
			return nil, fmt.Errorf("start remote shell: %w", err)
		}
	}

	shell := &InteractiveShell{
		Client:           client,
		Session:          session,
		Stdin:            stdin,
		Output:           outputReader,
		IntegrationError: integrationErr,
		integration:      integration,
		outputWriter:     outputWriter,
	}
	go func() {
		_ = session.Wait()
		_ = shell.Close()
	}()
	return shell, nil
}

func (s *InteractiveShell) WriteInput(data []byte) error {
	if s == nil || s.Stdin == nil {
		return fmt.Errorf("ssh shell is not available")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.Stdin.Write(data)
	return err
}

func (s *InteractiveShell) Resize(cols, rows int) error {
	if s == nil || s.Session == nil {
		return fmt.Errorf("ssh shell is not available")
	}
	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("invalid terminal size %dx%d", cols, rows)
	}
	return s.Session.WindowChange(rows, cols)
}

func (s *InteractiveShell) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		if s.Stdin != nil {
			_ = s.Stdin.Close()
		}
		if s.Session != nil {
			_ = s.Session.Close()
		}
		if s.Client != nil {
			cleanupShellIntegration(s.Client, s.integration)
			s.closeErr = s.Client.Close()
		}
		if s.outputWriter != nil {
			_ = s.outputWriter.Close()
		}
	})
	return s.closeErr
}
