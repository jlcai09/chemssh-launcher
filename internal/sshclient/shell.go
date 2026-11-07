package sshclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

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
	ProxyURL         string
	ProxyError       string
	X11Enabled       bool
	X11Error         string

	integration  *shellIntegration
	proxyChannel *LocalProxyChannel
	x11          *x11Forwarder
	outputWriter *io.PipeWriter
	writeMu      sync.Mutex
	closeOnce    sync.Once
	closeErr     error
}

func StartInteractiveShell(profile config.Profile, secrets secret.Store, policy HostKeyPolicy, cols, rows int) (*InteractiveShell, error) {
	shell, err := startInteractiveShellOnce(profile, secrets, policy, cols, rows)
	if errors.Is(err, errX11RequestTimeout) {
		profile.X11.Enabled = false
		shell, err = startInteractiveShellOnce(profile, secrets, policy, cols, rows)
		if shell != nil {
			shell.X11Error = "X11 forwarding timed out; reconnected this terminal without X11"
		}
	}
	return shell, err
}

func startInteractiveShellOnce(profile config.Profile, secrets secret.Store, policy HostKeyPolicy, cols, rows int) (*InteractiveShell, error) {
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

	var proxyChannel *LocalProxyChannel
	var proxyURL, proxyErrText string
	var proxyEnv []config.EnvPair
	if profile.ProxyActive() {
		channel, channelErr := StartTerminalProxyChannel(context.Background(), client, profile)
		if channelErr != nil {
			proxyErrText = "local network channel unavailable; this terminal started without it: " + channelErr.Error()
		} else if channel != nil {
			proxyChannel = channel
			proxyURL = channel.ProxyURL
			proxyEnv = channel.Env
		}
	}
	var x11 *x11Forwarder
	var x11Enabled bool
	var x11Error string
	if profile.X11Active() {
		local, localErr := prepareLocalXServer(profile.X11)
		if localErr != nil {
			x11Error = "X11 forwarding unavailable; terminal started without it: " + localErr.Error()
		} else {
			x11, localErr = newX11Forwarder(client, local)
			if localErr != nil {
				local.Close()
				x11Error = "X11 forwarding unavailable; terminal started without it: " + localErr.Error()
			}
		}
	}
	fail := func(err error) (*InteractiveShell, error) {
		_ = client.Close()
		if x11 != nil {
			x11.Close()
		}
		if proxyChannel != nil {
			proxyChannel.Close()
		}
		_ = client.Close()
		return nil, err
	}

	integration, integrationErr := prepareShellIntegration(client, proxyEnv)
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
		if x11 != nil {
			x11Err := x11.request(session)
			x11Enabled = x11Err == nil
			if x11Err != nil {
				x11Error = "X11 forwarding unavailable; terminal started without it: " + x11Err.Error()
				if errors.Is(x11Err, errX11RequestTimeout) {
					return nil, nil, x11Err
				}
			} else {
				x11Error = ""
			}
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
		return fail(err)
	}

	integrationStarted := false
	proxyEnvApplied := len(proxyEnv) == 0
	var proxyApplyErr error
	if integration != nil {
		if startErr := session.Start(interactiveShellCommand(integration, proxyEnv)); startErr == nil {
			integrationStarted = true
			proxyEnvApplied = true
		} else {
			integrationErr = fmt.Errorf("start shell integration: %w", startErr)
			_ = session.Close()
			session, stdin, err = openSession()
			if err != nil {
				_ = outputWriter.Close()
				return fail(err)
			}
		}
	}
	if !integrationStarted && len(proxyEnv) > 0 {
		// Without the integration rcfile the shell cannot be told about the
		// proxy after its profiles run, so at least export it before spawning.
		if startErr := session.Start(proxyLoginShellCommand(proxyEnv)); startErr == nil {
			integrationStarted = true
			proxyEnvApplied = true
		} else {
			proxyApplyErr = startErr
			_ = session.Close()
			session, stdin, err = openSession()
			if err != nil {
				_ = outputWriter.Close()
				return fail(err)
			}
		}
	}
	if !integrationStarted {
		if err := session.Shell(); err != nil {
			_ = outputWriter.Close()
			_ = session.Close()
			return fail(fmt.Errorf("start remote shell: %w", err))
		}
		if !proxyEnvApplied {
			proxyErrText = "the remote shell started without the local proxy environment"
			if proxyApplyErr != nil {
				proxyErrText += ": " + proxyApplyErr.Error()
			}
			// Nothing in this shell can use the reverse channel, so release the
			// remote loopback listener instead of keeping it until the tab closes.
			if proxyChannel != nil {
				proxyChannel.Close()
				proxyChannel = nil
				proxyURL = ""
			}
		}
	}

	// Keep a rejected request's handler draining and denying late opens until
	// this terminal transport closes; it must not close the working plain PTY.
	shell := &InteractiveShell{
		Client:           client,
		Session:          session,
		Stdin:            stdin,
		Output:           outputReader,
		IntegrationError: integrationErr,
		ProxyURL:         proxyURL,
		ProxyError:       proxyErrText,
		X11Enabled:       x11Enabled,
		X11Error:         x11Error,
		x11:              x11,
		integration:      integration,
		proxyChannel:     proxyChannel,
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
		if s.outputWriter != nil {
			_ = s.outputWriter.Close()
		}
		if s.Client != nil {
			// Cleanup is best-effort, and must not hold terminal shutdown if
			// the SSH peer ignores session/channel close packets.
			timer := time.AfterFunc(3*time.Second, func() { _ = s.Client.Close() })
			cleanupShellIntegration(s.Client, s.integration)
			timer.Stop()
			s.closeErr = s.Client.Close()
		}
		if s.x11 != nil {
			s.x11.Close()
		}
		if s.proxyChannel != nil {
			s.proxyChannel.Close()
		}
		if s.Stdin != nil {
			_ = s.Stdin.Close()
		}
		if s.Session != nil {
			_ = s.Session.Close()
		}

	})
	return s.closeErr
}
