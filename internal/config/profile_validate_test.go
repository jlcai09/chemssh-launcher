package config

import (
	"errors"
	"strings"
	"testing"
)

func TestProfileValidateAcceptsCompleteRemoteProfile(t *testing.T) {
	p := NewProfileDefaults()
	p.Name = "Example Server"
	p.SSHHost = "example.com"
	p.SSHUser = "user"
	if err := p.Validate(); err != nil {
		t.Fatalf("valid remote profile rejected: %v", err)
	}
}

func TestProfileValidateAcceptsPrivateKeyProfile(t *testing.T) {
	p := NewProfileDefaults()
	p.Name = "Example Server"
	p.SSHHost = "example.com"
	p.SSHUser = "user"
	p.AuthMethod = AuthPrivateKey
	p.PrivateKeyPath = "/home/user/.ssh/id_ed25519"
	if err := p.Validate(); err != nil {
		t.Fatalf("valid private key profile rejected: %v", err)
	}
}

func TestProfileValidateAcceptsLocalProfileWithDefaults(t *testing.T) {
	p := NewLocalProfileDefaults()
	if err := p.Validate(); err != nil {
		t.Fatalf("default local profile rejected: %v", err)
	}
}

func TestProfileValidateIgnoresSSHFieldsForLocalProfile(t *testing.T) {
	p := NewLocalProfileDefaults()
	p.SSHHost = ""
	p.SSHUser = ""
	p.SSHPort = 0
	p.AuthMethod = "unsupported"
	p.PrivateKeyPath = ""
	if err := p.Validate(); err != nil {
		t.Fatalf("local profile should not require SSH fields: %v", err)
	}
}

func TestProfileValidateRejectsMissingFields(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Profile)
		wantSub string
	}{
		{
			name:    "missing name",
			mutate:  func(p *Profile) { p.Name = "   " },
			wantSub: "name is required",
		},
		{
			name:    "missing ssh host",
			mutate:  func(p *Profile) { p.SSHHost = "" },
			wantSub: "SSH host is required",
		},
		{
			name:    "ssh port too low",
			mutate:  func(p *Profile) { p.SSHPort = 0 },
			wantSub: "SSH port must be between 1 and 65535",
		},
		{
			name:    "ssh port too high",
			mutate:  func(p *Profile) { p.SSHPort = 70000 },
			wantSub: "SSH port must be between 1 and 65535",
		},
		{
			name:    "missing ssh user",
			mutate:  func(p *Profile) { p.SSHUser = "" },
			wantSub: "SSH user is required",
		},
		{
			name:    "unknown auth method",
			mutate:  func(p *Profile) { p.AuthMethod = "agent" },
			wantSub: "auth method must be password or private_key",
		},
		{
			name: "private key without path",
			mutate: func(p *Profile) {
				p.AuthMethod = AuthPrivateKey
				p.PrivateKeyPath = ""
			},
			wantSub: "private key path is required",
		},
		{
			name:    "missing remote host",
			mutate:  func(p *Profile) { p.RemoteHost = "" },
			wantSub: "remote ChemSSH host is required",
		},
		{
			name:    "wildcard remote host",
			mutate:  func(p *Profile) { p.RemoteHost = "0.0.0.0" },
			wantSub: "not a wildcard bind address",
		},
		{
			name:    "remote port out of range",
			mutate:  func(p *Profile) { p.RemotePort = 0 },
			wantSub: "remote ChemSSH port must be between 1 and 65535",
		},
		{
			name:    "missing local bind host",
			mutate:  func(p *Profile) { p.LocalHost = "" },
			wantSub: "local bind host is required",
		},
		{
			name:    "local bind port out of range",
			mutate:  func(p *Profile) { p.LocalPort = -1 },
			wantSub: "local bind port must be between 1 and 65535",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewProfileDefaults()
			p.Name = "Example Server"
			p.SSHHost = "example.com"
			p.SSHUser = "user"
			tc.mutate(&p)
			err := p.Validate()
			if err == nil {
				t.Fatalf("expected validation error")
			}
			if !errors.Is(err, ErrProfileInvalid) {
				t.Fatalf("error does not wrap ErrProfileInvalid: %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestProfileValidateRejectsIncompleteLocalProfile(t *testing.T) {
	p := NewLocalProfileDefaults()
	p.LocalHost = ""
	if err := p.Validate(); !errors.Is(err, ErrProfileInvalid) {
		t.Fatalf("expected ErrProfileInvalid, got %v", err)
	}
	p = NewLocalProfileDefaults()
	p.LocalPort = 0
	if err := p.Validate(); !errors.Is(err, ErrProfileInvalid) {
		t.Fatalf("expected ErrProfileInvalid, got %v", err)
	}
}

func TestProfileWarningsAreSilentForTheDefaultTunnel(t *testing.T) {
	p := NewProfileDefaults()
	p.Name = "Example Server"
	p.SSHHost = "example.com"
	p.SSHUser = "user"
	if warnings := p.Warnings(); len(warnings) != 0 {
		t.Fatalf("default tunnel profile produced warnings: %v", warnings)
	}
}

func TestProfileWarningsReportsExposedBind(t *testing.T) {
	p := NewProfileDefaults()
	p.Name = "Example Server"
	p.SSHHost = "example.com"
	p.SSHUser = "user"
	p.LocalHost = "0.0.0.0"
	warnings := p.Warnings()
	if len(warnings) != 1 || !strings.Contains(warnings[0], "not loopback") {
		t.Fatalf("warnings = %v, want a non-loopback bind warning", warnings)
	}
}

func TestProfileWarningsReportsBindThatMatchesTheSSHEndpoint(t *testing.T) {
	p := NewProfileDefaults()
	p.Name = "Example Server"
	p.SSHHost = "127.0.0.1"
	p.SSHPort = 2222
	p.SSHUser = "user"
	p.LocalHost = "127.0.0.1"
	p.LocalPort = 2222
	warnings := p.Warnings()
	if len(warnings) != 1 || !strings.Contains(warnings[0], "SSH endpoint") {
		t.Fatalf("warnings = %v, want an SSH endpoint collision warning", warnings)
	}
}

func TestProfileWarningsReportsIgnoredRemoteFieldsOnLocalProfile(t *testing.T) {
	p := NewLocalProfileDefaults()
	p.RemoteHost = "127.0.0.1"
	p.RemotePort = 9999
	warnings := p.Warnings()
	if len(warnings) != 1 || !strings.Contains(warnings[0], "ignores the remote host") {
		t.Fatalf("warnings = %v, want an ignored remote host warning", warnings)
	}

	p = NewLocalProfileDefaults()
	p.RemoteHost = p.LocalHost
	p.RemotePort = p.LocalPort
	if warnings := p.Warnings(); len(warnings) != 0 {
		t.Fatalf("matching local and remote values produced warnings: %v", warnings)
	}
}
