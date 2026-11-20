use std::net::IpAddr;

use serde::{Deserialize, Serialize};

pub const DEFAULT_LOCAL_PROXY_HOST: &str = "127.0.0.1";
pub const DEFAULT_LOCAL_PROXY_PORT: u16 = 7890;
pub const DEFAULT_LOCAL_PROXY_NO_PROXY: &str = "localhost,127.0.0.1,::1";

/// Points the remote side of a session at a proxy running on the launcher machine.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct LocalProxy {
    pub enabled: bool,
    pub host: String,
    pub port: u16,
    pub no_proxy: String,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct EnvPair {
    pub key: &'static str,
    pub value: String,
}

impl Default for LocalProxy {
    fn default() -> Self {
        Self {
            enabled: false,
            host: String::new(),
            port: 0,
            no_proxy: String::new(),
        }
    }
}

impl LocalProxy {
    pub fn default_config() -> Self {
        Self {
            enabled: false,
            host: DEFAULT_LOCAL_PROXY_HOST.to_string(),
            port: DEFAULT_LOCAL_PROXY_PORT,
            no_proxy: DEFAULT_LOCAL_PROXY_NO_PROXY.to_string(),
        }
    }

    /// Fills fields missing from legacy profile files.
    /// A completely empty value is treated as legacy and gets default `no_proxy` too;
    /// a partially filled value preserves `no_proxy` as configured (including empty).
    pub fn normalized(&self) -> Self {
        let mut lp = self.clone();
        if lp.host.trim().is_empty() && lp.port == 0 && lp.no_proxy.trim().is_empty() {
            lp.host = DEFAULT_LOCAL_PROXY_HOST.to_string();
            lp.port = DEFAULT_LOCAL_PROXY_PORT;
            lp.no_proxy = DEFAULT_LOCAL_PROXY_NO_PROXY.to_string();
            return lp;
        }
        if lp.host.trim().is_empty() {
            lp.host = DEFAULT_LOCAL_PROXY_HOST.to_string();
        }
        if lp.port == 0 {
            lp.port = DEFAULT_LOCAL_PROXY_PORT;
        }
        lp
    }

    pub fn address(&self) -> String {
        let host = self.host.trim();
        if host.contains(':') && !host.starts_with('[') {
            format!("[{host}]:{}", self.port)
        } else {
            format!("{host}:{}", self.port)
        }
    }

    pub fn url(&self) -> String {
        format!("http://{}", self.address())
    }

    pub fn is_loopback(&self) -> bool {
        let host = self.host.trim();
        if host.eq_ignore_ascii_case("localhost") {
            return true;
        }
        let unbracketed = host
            .strip_prefix('[')
            .and_then(|s| s.strip_suffix(']'))
            .unwrap_or(host);
        unbracketed
            .parse::<IpAddr>()
            .map(|ip| ip.is_loopback())
            .unwrap_or(false)
    }
}

/// Emits both lowercase and uppercase proxy variables; omits `no_proxy`/`NO_PROXY` when empty.
pub fn proxy_env_pairs(proxy_url: &str, no_proxy: &str) -> Vec<EnvPair> {
    let mut pairs = vec![
        EnvPair { key: "http_proxy", value: proxy_url.to_string() },
        EnvPair { key: "https_proxy", value: proxy_url.to_string() },
        EnvPair { key: "all_proxy", value: proxy_url.to_string() },
        EnvPair { key: "HTTP_PROXY", value: proxy_url.to_string() },
        EnvPair { key: "HTTPS_PROXY", value: proxy_url.to_string() },
        EnvPair { key: "ALL_PROXY", value: proxy_url.to_string() },
    ];
    if !no_proxy.trim().is_empty() {
        pairs.push(EnvPair { key: "no_proxy", value: no_proxy.to_string() });
        pairs.push(EnvPair { key: "NO_PROXY", value: no_proxy.to_string() });
    }
    pairs
}
