use serde::{Deserialize, Serialize};

use crate::{ConfigError, LocalProxy, X11Config};

pub const PROFILE_KIND_REMOTE: &str = "remote";
pub const PROFILE_KIND_LOCAL: &str = "local";

pub const AUTH_PASSWORD: &str = "password";
pub const AUTH_PRIVATE_KEY: &str = "private_key";

pub const DEFAULT_START_COMMAND: &str = "source .venv/bin/activate\nunset PS1\nexport CONDA_CHANGEPS1=false\nexport VIRTUAL_ENV_DISABLE_PROMPT=1\nchemssh --config config.yaml";

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, Default)]
#[serde(default)]
pub struct Profile {
    pub id: String,
    pub kind: String,
    pub name: String,
    pub ssh_host: String,
    pub ssh_port: u16,
    pub ssh_user: String,
    pub auth_method: String,
    pub has_password: bool,
    pub private_key_path: String,
    pub has_private_key_passphrase: bool,
    pub remote_host: String,
    pub remote_port: u16,
    pub local_host: String,
    pub local_port: u16,
    pub local_url_path: String,
    pub pre_start_commands: String,
    pub start_command: String,
    pub health_check_url: String,
    pub open_browser: bool,
    pub has_security_token: bool,
    pub local_proxy: LocalProxy,
    pub x11: X11Config,
}

impl Profile {
    pub fn remote_defaults() -> Self {
        Self {
            id: String::new(),
            kind: PROFILE_KIND_REMOTE.to_string(),
            name: String::new(),
            ssh_host: String::new(),
            ssh_port: 22,
            ssh_user: String::new(),
            auth_method: AUTH_PASSWORD.to_string(),
            has_password: false,
            private_key_path: String::new(),
            has_private_key_passphrase: false,
            remote_host: "127.0.0.1".to_string(),
            remote_port: 8888,
            local_host: "127.0.0.1".to_string(),
            local_port: 8888,
            local_url_path: "/".to_string(),
            pre_start_commands: String::new(),
            start_command: DEFAULT_START_COMMAND.to_string(),
            health_check_url: String::new(),
            open_browser: true,
            has_security_token: false,
            local_proxy: LocalProxy::default_config(),
            x11: X11Config::default_config(),
        }
    }

    pub fn local_defaults() -> Self {
        let mut p = Self::remote_defaults();
        p.kind = PROFILE_KIND_LOCAL.to_string();
        p.name = "Local ChemSSH".to_string();
        p.ssh_port = 0;
        p.auth_method.clear();
        p.ssh_host.clear();
        p.ssh_user.clear();
        p.remote_host.clear();
        p.remote_port = 0;
        p.pre_start_commands.clear();
        p.start_command.clear();
        p
    }

    pub fn effective_kind(&self) -> &'static str {
        if self.kind == PROFILE_KIND_LOCAL {
            PROFILE_KIND_LOCAL
        } else {
            PROFILE_KIND_REMOTE
        }
    }

    pub fn is_local(&self) -> bool {
        self.effective_kind() == PROFILE_KIND_LOCAL
    }

    pub fn proxy_active(&self) -> bool {
        self.local_proxy.enabled && !self.is_local()
    }

    pub fn x11_active(&self) -> bool {
        self.x11.enabled && !self.is_local()
    }

    pub fn browser_url(&self) -> String {
        format!(
            "http://{}:{}{}",
            self.local_host,
            self.local_port,
            normalize_url_path(&self.local_url_path)
        )
    }

    pub fn health_url(&self) -> String {
        if !self.health_check_url.is_empty() {
            self.health_check_url.clone()
        } else {
            self.browser_url()
        }
    }

    pub fn remote_address(&self) -> String {
        format!("{}:{}", self.remote_host, self.remote_port)
    }

    pub fn local_address(&self) -> String {
        format!("{}:{}", self.local_host, self.local_port)
    }

    pub fn uses_non_loopback_local_host(&self) -> bool {
        self.local_host == "0.0.0.0"
            || self.local_host == "::"
            || (!self.local_host.is_empty()
                && self.local_host != "127.0.0.1"
                && self.local_host != "localhost"
                && self.local_host != "::1")
    }

    pub fn validate(&self) -> Result<(), ConfigError> {
        if self.name.trim().is_empty() {
            return Err(ConfigError::InvalidProfile("profile name is required".into()));
        }
        if self.is_local() {
            if self.local_host.trim().is_empty() {
                return Err(ConfigError::InvalidProfile(
                    "local ChemSSH host is required".into(),
                ));
            }
            if self.local_port == 0 {
                return Err(ConfigError::InvalidProfile(
                    "local ChemSSH port must be between 1 and 65535".into(),
                ));
            }
            return Ok(());
        }
        if self.ssh_host.trim().is_empty() {
            return Err(ConfigError::InvalidProfile("SSH host is required".into()));
        }
        if self.ssh_port == 0 {
            return Err(ConfigError::InvalidProfile(
                "SSH port must be between 1 and 65535".into(),
            ));
        }
        if self.ssh_user.trim().is_empty() {
            return Err(ConfigError::InvalidProfile("SSH user is required".into()));
        }
        match self.auth_method.as_str() {
            AUTH_PASSWORD => {}
            AUTH_PRIVATE_KEY => {
                if self.private_key_path.trim().is_empty() {
                    return Err(ConfigError::InvalidProfile(
                        "private key path is required for private key authentication".into(),
                    ));
                }
            }
            _ => {
                return Err(ConfigError::InvalidProfile(
                    "auth method must be password or private_key".into(),
                ));
            }
        }
        if self.remote_host.trim().is_empty() {
            return Err(ConfigError::InvalidProfile(
                "remote ChemSSH host is required".into(),
            ));
        }
        if self.remote_host == "0.0.0.0" || self.remote_host == "::" {
            return Err(ConfigError::InvalidProfile(
                "remote ChemSSH host must be a reachable address, not a wildcard bind address"
                    .into(),
            ));
        }
        if self.remote_port == 0 {
            return Err(ConfigError::InvalidProfile(
                "remote ChemSSH port must be between 1 and 65535".into(),
            ));
        }
        if self.local_host.trim().is_empty() {
            return Err(ConfigError::InvalidProfile(
                "local bind host is required".into(),
            ));
        }
        if self.local_port == 0 {
            return Err(ConfigError::InvalidProfile(
                "local bind port must be between 1 and 65535".into(),
            ));
        }
        if self.x11_active() {
            self.x11.validate()?;
        }
        if self.local_proxy.enabled {
            if self.local_proxy.host.trim().is_empty() {
                return Err(ConfigError::InvalidProfile(
                    "local proxy host is required when the local network channel is enabled".into(),
                ));
            }
            if self.local_proxy.port == 0 {
                return Err(ConfigError::InvalidProfile(
                    "local proxy port must be between 1 and 65535".into(),
                ));
            }
        }
        Ok(())
    }

    pub fn warnings(&self) -> Vec<String> {
        let mut warnings = Vec::new();
        if self.uses_non_loopback_local_host() {
            warnings.push(format!(
                "local bind host {} is not loopback; the tunnel is reachable from other machines",
                self.local_host
            ));
        }
        if self.is_local() {
            if (!self.remote_host.is_empty() || self.remote_port > 0)
                && (self.remote_host != self.local_host || self.remote_port != self.local_port)
            {
                warnings.push(format!(
                    "local profile ignores the remote host and port; the launcher uses {}",
                    self.local_address()
                ));
            }
            return warnings;
        }
        if self.local_host == self.ssh_host && self.local_port == self.ssh_port {
            warnings.push(format!(
                "local bind {} is the SSH endpoint itself; the tunnel listener cannot share the SSH port",
                self.local_address()
            ));
        }
        if self.local_proxy.enabled && !self.local_proxy.normalized().is_loopback() {
            warnings.push(format!(
                "local proxy host {} is not loopback; verify it is reachable from the launcher machine",
                self.local_proxy.host
            ));
        }
        warnings
    }
}

fn normalize_url_path(path: &str) -> String {
    if path.is_empty() {
        return "/".to_string();
    }
    if path.starts_with('/') {
        path.to_string()
    } else {
        format!("/{path}")
    }
}
