package config

const (
	AuthPassword   = "password"
	AuthPrivateKey = "private_key"
)

const DefaultStartCommand = `source .venv/bin/activate
__chemweb_conda_prompt=""
if [ -n "${CONDA_DEFAULT_ENV:-}" ]; then
  __chemweb_conda_prompt="(${CONDA_DEFAULT_ENV}) "
fi

__chemweb_venv_prompt=""
if [ -n "${VIRTUAL_ENV:-}" ]; then
  __chemweb_venv_prompt="($(basename "$VIRTUAL_ENV")) "
fi

__chemweb_prompt_char="$"
if [ "$(id -u)" = "0" ]; then
  __chemweb_prompt_char="#"
fi

export PS1="${__chemweb_conda_prompt}${__chemweb_venv_prompt}[\u@\h \W]${__chemweb_prompt_char} "
chemweb --config config.yaml`

type Profile struct {
	ID                      string `json:"id"`
	Name                    string `json:"name"`
	SSHHost                 string `json:"ssh_host"`
	SSHPort                 int    `json:"ssh_port"`
	SSHUser                 string `json:"ssh_user"`
	AuthMethod              string `json:"auth_method"`
	HasPassword             bool   `json:"has_password"`
	PrivateKeyPath          string `json:"private_key_path"`
	HasPrivateKeyPassphrase bool   `json:"has_private_key_passphrase"`
	RemoteHost              string `json:"remote_host"`
	RemotePort              int    `json:"remote_port"`
	LocalHost               string `json:"local_host"`
	LocalPort               int    `json:"local_port"`
	LocalURLPath            string `json:"local_url_path"`
	PreStartCommands        string `json:"pre_start_commands"`
	StartCommand            string `json:"start_command"`
	HealthCheckURL          string `json:"health_check_url"`
	OpenBrowser             bool   `json:"open_browser"`
}

func NewProfileDefaults() Profile {
	return Profile{
		SSHPort:      22,
		AuthMethod:   AuthPassword,
		RemoteHost:   "127.0.0.1",
		RemotePort:   8888,
		LocalHost:    "127.0.0.1",
		LocalPort:    8888,
		LocalURLPath: "/",
		StartCommand: DefaultStartCommand,
		OpenBrowser:  true,
	}
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
