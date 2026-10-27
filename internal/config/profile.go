package config

import (
	"errors"
	"fmt"
	"strings"
)

const (
	ProfileKindRemote = "remote"
	ProfileKindLocal  = "local"

	AuthPassword   = "password"
	AuthPrivateKey = "private_key"
)

// ErrProfileInvalid marks validation failures so callers can return a client
// error instead of a server error.
var ErrProfileInvalid = errors.New("profile invalid")

const DefaultStartCommand = `source .venv/bin/activate
unset PS1
export CONDA_CHANGEPS1=false
export VIRTUAL_ENV_DISABLE_PROMPT=1
chemssh --config config.yaml`

type Profile struct {
	ID                      string     `json:"id"`
	Kind                    string     `json:"kind"`
	Name                    string     `json:"name"`
	SSHHost                 string     `json:"ssh_host"`
	SSHPort                 int        `json:"ssh_port"`
	SSHUser                 string     `json:"ssh_user"`
	AuthMethod              string     `json:"auth_method"`
	HasPassword             bool       `json:"has_password"`
	PrivateKeyPath          string     `json:"private_key_path"`
	HasPrivateKeyPassphrase bool       `json:"has_private_key_passphrase"`
	RemoteHost              string     `json:"remote_host"`
	RemotePort              int        `json:"remote_port"`
	LocalHost               string     `json:"local_host"`
	LocalPort               int        `json:"local_port"`
	LocalURLPath            string     `json:"local_url_path"`
	PreStartCommands        string     `json:"pre_start_commands"`
	StartCommand            string     `json:"start_command"`
	HealthCheckURL          string     `json:"health_check_url"`
	OpenBrowser             bool       `json:"open_browser"`
	HasSecurityToken        bool       `json:"has_security_token"`
	LocalProxy              LocalProxy `json:"local_proxy"`
}

func NewProfileDefaults() Profile {
	return Profile{
		Kind:         ProfileKindRemote,
		SSHPort:      22,
		AuthMethod:   AuthPassword,
		RemoteHost:   "127.0.0.1",
		RemotePort:   8888,
		LocalHost:    "127.0.0.1",
		LocalPort:    8888,
		LocalURLPath: "/",
		StartCommand: DefaultStartCommand,
		OpenBrowser:  true,
		LocalProxy:   DefaultLocalProxy(),
	}
}

func NewLocalProfileDefaults() Profile {
	p := NewProfileDefaults()
	p.Kind = ProfileKindLocal
	p.Name = "Local ChemSSH"
	p.SSHPort = 0
	p.AuthMethod = ""
	p.SSHHost = ""
	p.SSHUser = ""
	p.HasPassword = false
	p.PrivateKeyPath = ""
	p.HasPrivateKeyPassphrase = false
	p.RemoteHost = ""
	p.RemotePort = 0
	p.PreStartCommands = ""
	p.StartCommand = ""
	return p
}

// Validate checks the fields the launcher needs before a profile can be saved
// through the API, the CLI, or an import. FileStore itself stays permissive so
// partially built profiles can still be round-tripped by callers that manage
// their own defaults.
func (p Profile) Validate() error {
	if strings.TrimSpace(p.Name) == "" {
		return invalidProfile("profile name is required")
	}
	if p.IsLocal() {
		if strings.TrimSpace(p.LocalHost) == "" {
			return invalidProfile("local ChemSSH host is required")
		}
		if p.LocalPort < 1 || p.LocalPort > 65535 {
			return invalidProfile("local ChemSSH port must be between 1 and 65535")
		}
		return nil
	}
	if strings.TrimSpace(p.SSHHost) == "" {
		return invalidProfile("SSH host is required")
	}
	if p.SSHPort < 1 || p.SSHPort > 65535 {
		return invalidProfile("SSH port must be between 1 and 65535")
	}
	if strings.TrimSpace(p.SSHUser) == "" {
		return invalidProfile("SSH user is required")
	}
	switch p.AuthMethod {
	case AuthPassword:
	case AuthPrivateKey:
		if strings.TrimSpace(p.PrivateKeyPath) == "" {
			return invalidProfile("private key path is required for private key authentication")
		}
	default:
		return invalidProfile("auth method must be password or private_key")
	}
	if strings.TrimSpace(p.RemoteHost) == "" {
		return invalidProfile("remote ChemSSH host is required")
	}
	if p.RemoteHost == "0.0.0.0" || p.RemoteHost == "::" {
		return invalidProfile("remote ChemSSH host must be a reachable address, not a wildcard bind address")
	}
	if p.RemotePort < 1 || p.RemotePort > 65535 {
		return invalidProfile("remote ChemSSH port must be between 1 and 65535")
	}
	if strings.TrimSpace(p.LocalHost) == "" {
		return invalidProfile("local bind host is required")
	}
	if p.LocalPort < 1 || p.LocalPort > 65535 {
		return invalidProfile("local bind port must be between 1 and 65535")
	}
	if p.LocalProxy.Enabled {
		if strings.TrimSpace(p.LocalProxy.Host) == "" {
			return invalidProfile("local proxy host is required when the local network channel is enabled")
		}
		if p.LocalProxy.Port < 1 || p.LocalProxy.Port > 65535 {
			return invalidProfile("local proxy port must be between 1 and 65535")
		}
	}
	return nil
}

func invalidProfile(message string) error {
	return fmt.Errorf("%w: %s", ErrProfileInvalid, message)
}

// Warnings reports configurations that are legal but easy to misread. They are
// surfaced to the user (profile log, CLI output, form hint) instead of being
// rejected, because each one still has a well-defined runtime behaviour.
func (p Profile) Warnings() []string {
	var warnings []string
	if p.UsesNonLoopbackLocalHost() {
		warnings = append(warnings, "local bind host "+p.LocalHost+" is not loopback; the tunnel is reachable from other machines")
	}
	if p.IsLocal() {
		// A local profile drives ChemSSH through LocalHost/LocalPort only, so a
		// leftover remote host/port would silently be ignored.
		if (p.RemoteHost != "" || p.RemotePort > 0) && (p.RemoteHost != p.LocalHost || p.RemotePort != p.LocalPort) {
			warnings = append(warnings, "local profile ignores the remote host and port; the launcher uses "+p.LocalAddress())
		}
		return warnings
	}
	if p.LocalHost == p.SSHHost && p.LocalPort == p.SSHPort {
		warnings = append(warnings, "local bind "+p.LocalAddress()+" is the SSH endpoint itself; the tunnel listener cannot share the SSH port")
	}
	if p.LocalProxy.Enabled && !p.LocalProxy.Normalized().IsLoopback() {
		warnings = append(warnings, "local proxy host "+p.LocalProxy.Host+" is not loopback; verify it is reachable from the launcher machine")
	}
	return warnings
}

func (p Profile) EffectiveKind() string {
	if p.Kind == ProfileKindLocal {
		return ProfileKindLocal
	}
	return ProfileKindRemote
}

func (p Profile) IsLocal() bool {
	return p.EffectiveKind() == ProfileKindLocal
}

// ProxyActive reports whether the session should open the reverse channel to
// the local proxy. Local profiles have no SSH transport to carry it.
func (p Profile) ProxyActive() bool {
	return p.LocalProxy.Enabled && !p.IsLocal()
}

func (p Profile) BrowserURL() string {
	return "http://" + p.LocalHost + ":" + itoa(p.LocalPort) + normalizeURLPath(p.LocalURLPath)
}

func (p Profile) HealthURL() string {
	if p.HealthCheckURL != "" {
		return p.HealthCheckURL
	}
	return p.BrowserURL()
}

func normalizeURLPath(path string) string {
	if path == "" {
		path = "/"
	}
	if path[0] != '/' {
		path = "/" + path
	}
	return path
}

func (p Profile) RemoteAddress() string {
	return p.RemoteHost + ":" + itoa(p.RemotePort)
}

func (p Profile) LocalAddress() string {
	return p.LocalHost + ":" + itoa(p.LocalPort)
}

func (p Profile) UsesNonLoopbackLocalHost() bool {
	return p.LocalHost == "0.0.0.0" || p.LocalHost == "::" || (p.LocalHost != "" && p.LocalHost != "127.0.0.1" && p.LocalHost != "localhost" && p.LocalHost != "::1")
}
