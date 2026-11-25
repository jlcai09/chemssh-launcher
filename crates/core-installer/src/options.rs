use serde::{Deserialize, Serialize};
use thiserror::Error;

pub const DEFAULT_INSTALL_DIR: &str = "~/.chemssh";

#[derive(Debug, Error, PartialEq, Eq)]
pub enum OptionsError {
    #[error("{0}")]
    Invalid(String),
}

fn default_true() -> bool {
    true
}

/// User-tunable installer settings. Every value is validated before it reaches a shell.
#[derive(Debug, Clone, Deserialize, Serialize, PartialEq, Eq)]
pub struct InstallerOptions {
    /// Install directory; `~/x`, a path relative to home, or an absolute path.
    #[serde(default)]
    pub install_dir: String,
    /// Python interpreter name or path; empty means auto-detect.
    #[serde(default)]
    pub python: String,
    /// pip index URL; empty means the default index.
    #[serde(default)]
    pub pip_index: String,
    /// Optional prefix of a third-party GitHub download proxy, e.g. `https://proxy.example.com`.
    #[serde(default)]
    pub proxy_prefix: String,
    /// Skip the profile's pre-start commands (defaults to true so old `cd`/venv lines do not break install).
    #[serde(default = "default_true")]
    pub ignore_pre_start: bool,
}

impl Default for InstallerOptions {
    fn default() -> Self {
        Self {
            install_dir: String::new(),
            python: String::new(),
            pip_index: String::new(),
            proxy_prefix: String::new(),
            ignore_pre_start: true,
        }
    }
}

/// A validated install directory, anchored at the remote home unless absolute.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum InstallDir {
    Home(String),
    Absolute(String),
}

impl InstallDir {
    pub fn parse(raw: &str) -> Result<Self, OptionsError> {
        let raw = raw.trim();
        let raw = if raw.is_empty() { DEFAULT_INSTALL_DIR } else { raw };
        let (absolute, rest) = if let Some(rest) = raw.strip_prefix("$HOME/") {
            (false, rest)
        } else if let Some(rest) = raw.strip_prefix("~/") {
            (false, rest)
        } else if let Some(rest) = raw.strip_prefix('/') {
            (true, rest)
        } else if raw == "~" || raw == "$HOME" {
            (false, "")
        } else {
            (false, raw)
        };
        let rest = rest.trim_end_matches('/');
        if rest.is_empty() {
            return Err(OptionsError::Invalid(
                "install directory must be a subdirectory, not the home or root directory".into(),
            ));
        }
        for comp in rest.split('/') {
            if comp.is_empty() || comp == "." || comp == ".." {
                return Err(OptionsError::Invalid("install directory contains an invalid component".into()));
            }
            if !comp
                .chars()
                .all(|c| c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '-'))
            {
                return Err(OptionsError::Invalid(
                    "install directory may only contain letters, digits, '.', '_' and '-'".into(),
                ));
            }
        }
        Ok(if absolute {
            InstallDir::Absolute(format!("/{rest}"))
        } else {
            InstallDir::Home(rest.to_string())
        })
    }

    /// Shell expression usable inside double quotes.
    pub fn shell_expr(&self) -> String {
        match self {
            InstallDir::Home(rest) => format!("$HOME/{rest}"),
            InstallDir::Absolute(path) => path.clone(),
        }
    }

    pub fn resolve(&self, home: &str) -> String {
        match self {
            InstallDir::Home(rest) => format!("{}/{rest}", home.trim_end_matches('/')),
            InstallDir::Absolute(path) => path.clone(),
        }
    }
}

impl InstallerOptions {
    pub fn dir(&self) -> Result<InstallDir, OptionsError> {
        InstallDir::parse(&self.install_dir)
    }

    pub fn validate(&self) -> Result<(), OptionsError> {
        self.dir()?;
        if !self.python.trim().is_empty() && !is_safe_path(self.python.trim()) {
            return Err(OptionsError::Invalid("python path contains unsupported characters".into()));
        }
        validate_url("pip index", &self.pip_index)?;
        validate_url("proxy prefix", &self.proxy_prefix)?;
        Ok(())
    }

    pub fn python(&self) -> &str {
        self.python.trim()
    }

    pub fn pip_index(&self) -> &str {
        self.pip_index.trim()
    }

    pub fn proxy_prefix(&self) -> &str {
        self.proxy_prefix.trim()
    }
}

fn is_safe_path(s: &str) -> bool {
    !s.is_empty()
        && s.len() <= 256
        && s.chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '-' | '+' | '/'))
}

fn validate_url(label: &str, value: &str) -> Result<(), OptionsError> {
    let value = value.trim();
    if value.is_empty() {
        return Ok(());
    }
    let ok_scheme = value.starts_with("https://") || value.starts_with("http://");
    let ok_chars = value.len() <= 512
        && value
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '~' | ':' | '/' | '-' | '+' | '%' | '@' | '=' | '&' | '?' | '#'));
    if ok_scheme && ok_chars {
        Ok(())
    } else {
        Err(OptionsError::Invalid(format!("{label} must be an http(s) URL without special characters")))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn default_and_home_relative_dirs() {
        assert_eq!(InstallDir::parse("").unwrap(), InstallDir::Home(".chemssh".into()));
        assert_eq!(InstallDir::parse("~/apps/chemssh/").unwrap(), InstallDir::Home("apps/chemssh".into()));
        assert_eq!(InstallDir::parse("$HOME/x").unwrap(), InstallDir::Home("x".into()));
        assert_eq!(InstallDir::parse("tools/chemssh").unwrap().shell_expr(), "$HOME/tools/chemssh");
    }

    #[test]
    fn absolute_dirs_and_resolution() {
        let d = InstallDir::parse("/data1/user/chemssh").unwrap();
        assert_eq!(d, InstallDir::Absolute("/data1/user/chemssh".into()));
        assert_eq!(d.resolve("/home/user"), "/data1/user/chemssh");
        assert_eq!(InstallDir::parse("~/.chemssh").unwrap().resolve("/home/user/"), "/home/user/.chemssh");
    }

    #[test]
    fn rejects_dangerous_dirs() {
        for bad in ["/", "~", "~/", "~/a/../b", "~/a b", "~/a;rm", "~/$(x)", "~/a'b", "//x", "~/a//b"] {
            assert!(InstallDir::parse(bad).is_err(), "{bad} should be rejected");
        }
    }

    #[test]
    fn validates_urls_and_python() {
        let mut o = InstallerOptions::default();
        assert!(o.validate().is_ok());
        o.pip_index = "https://pypi.tuna.tsinghua.edu.cn/simple".into();
        o.proxy_prefix = "https://proxy.example.com".into();
        o.python = "/opt/python3.12/bin/python3".into();
        assert!(o.validate().is_ok());
        o.pip_index = "https://x.example.com/$(id)".into();
        assert!(o.validate().is_err());
        o.pip_index.clear();
        o.proxy_prefix = "ftp://x".into();
        assert!(o.validate().is_err());
        o.proxy_prefix.clear();
        o.python = "python3; id".into();
        assert!(o.validate().is_err());
    }
}
