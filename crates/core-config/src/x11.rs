use std::net::IpAddr;

use serde::{Deserialize, Serialize};

use crate::ConfigError;

pub const DEFAULT_X11_DISPLAY: &str = "127.0.0.1:0";

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, Default)]
#[serde(default)]
pub struct X11Config {
    pub enabled: bool,
    pub display: String,
    pub xauthority: String,
}

impl X11Config {
    pub fn default_config() -> Self {
        Self {
            enabled: false,
            display: DEFAULT_X11_DISPLAY.to_string(),
            xauthority: String::new(),
        }
    }

    pub fn normalized(&self) -> Self {
        let mut x = self.clone();
        x.display = x.display.trim().to_string();
        if x.display.is_empty() {
            x.display = DEFAULT_X11_DISPLAY.to_string();
        }
        x.xauthority = x.xauthority.trim().to_string();
        x
    }

    pub fn validate(&self) -> Result<(), ConfigError> {
        let x = self.normalized();
        let Some(sep) = x.display.rfind(':') else {
            return Err(ConfigError::InvalidProfile(
                "X11 display must be a loopback TCP display such as 127.0.0.1:0.0".into(),
            ));
        };
        if sep < 1 {
            return Err(ConfigError::InvalidProfile(
                "X11 display must be a loopback TCP display such as 127.0.0.1:0.0".into(),
            ));
        }

        let raw_host = &x.display[..sep];
        let host = raw_host
            .strip_prefix('[')
            .and_then(|s| s.strip_suffix(']'))
            .unwrap_or(raw_host);
        let is_loopback = host == "localhost"
            || host
                .parse::<IpAddr>()
                .map(|ip| ip.is_loopback())
                .unwrap_or(false);
        if !is_loopback {
            return Err(ConfigError::InvalidProfile(
                "X11 display host must be localhost or a loopback IP address".into(),
            ));
        }

        let rest = &x.display[sep + 1..];
        let parts: Vec<&str> = rest.split('.').collect();
        if parts.len() > 2 || !is_ascii_digits(parts[0]) {
            return Err(ConfigError::InvalidProfile(
                "X11 display number must be between 0 and 59535".into(),
            ));
        }
        let display_num: u32 = parts[0].parse().map_err(|_| {
            ConfigError::InvalidProfile("X11 display number must be between 0 and 59535".into())
        })?;
        if display_num > 65535 - 6000 {
            return Err(ConfigError::InvalidProfile(
                "X11 display number must be between 0 and 59535".into(),
            ));
        }
        if parts.len() == 2 {
            if !is_ascii_digits(parts[1]) {
                return Err(ConfigError::InvalidProfile(
                    "X11 screen number must be between 0 and 255".into(),
                ));
            }
            let screen: u32 = parts[1].parse().map_err(|_| {
                ConfigError::InvalidProfile("X11 screen number must be between 0 and 255".into())
            })?;
            if screen > 255 {
                return Err(ConfigError::InvalidProfile(
                    "X11 screen number must be between 0 and 255".into(),
                ));
            }
        }

        if x.xauthority.contains('\0') {
            return Err(ConfigError::InvalidProfile(
                "X11 paths must not contain NUL bytes".into(),
            ));
        }
        Ok(())
    }
}

fn is_ascii_digits(value: &str) -> bool {
    !value.is_empty() && value.bytes().all(|b| b.is_ascii_digit())
}
