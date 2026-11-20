use std::collections::BTreeMap;

use core_config::proxy_env_pairs;
use russh_sftp::client::SftpSession;
use russh_sftp::protocol::{FileAttributes, OpenFlags};
use serde::{Deserialize, Serialize};
use tokio::io::AsyncWriteExt;

use crate::SshError;

pub const PROXY_BRIDGE_VERSION: u32 = 1;
pub const PROXY_BRIDGE_DIR_NAME: &str = ".chemssh-launcher";

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ProxyBridgeFile {
    pub version: u32,
    pub proxy_url: String,
    pub no_proxy: String,
    pub env: BTreeMap<String, String>,
}

impl ProxyBridgeFile {
    pub fn new(proxy_url: &str, no_proxy: &str) -> Self {
        let mut env = BTreeMap::new();
        for pair in proxy_env_pairs(proxy_url, no_proxy) {
            env.insert(pair.key.to_string(), pair.value);
        }
        Self {
            version: PROXY_BRIDGE_VERSION,
            proxy_url: proxy_url.to_string(),
            no_proxy: no_proxy.to_string(),
            env,
        }
    }
}

pub fn proxy_bridge_path(home: &str, remote_port: u16) -> String {
    let trimmed = home.trim_end_matches('/');
    format!("{trimmed}/{PROXY_BRIDGE_DIR_NAME}/terminal-proxy-{remote_port}.json")
}

/// Publishes `$HOME/.chemssh-launcher/terminal-proxy-<port>.json` with mode `0600` over SFTP.
pub async fn write_proxy_bridge(
    sftp: &SftpSession,
    remote_port: u16,
    proxy_url: &str,
    no_proxy: &str,
) -> Result<String, SshError> {
    let home = sftp
        .canonicalize(".")
        .await
        .map_err(|e| SshError::ProxyBridge(format!("resolve remote home: {e}")))?;
    if home.trim().is_empty() {
        return Err(SshError::ProxyBridge(
            "resolve remote home: empty path".into(),
        ));
    }

    let dir = format!("{}/{}", home.trim_end_matches('/'), PROXY_BRIDGE_DIR_NAME);
    if sftp.try_exists(&dir).await.unwrap_or(false) == false {
        let _ = sftp.create_dir(&dir).await;
    }

    let target = proxy_bridge_path(&home, remote_port);
    let mut payload = serde_json::to_vec_pretty(&ProxyBridgeFile::new(proxy_url, no_proxy))
        .map_err(|e| SshError::ProxyBridge(format!("serialize bridge payload: {e}")))?;
    payload.push(b'\n');

    let mut file = sftp
        .open_with_flags(
            &target,
            OpenFlags::WRITE | OpenFlags::CREATE | OpenFlags::TRUNCATE,
        )
        .await
        .map_err(|e| SshError::ProxyBridge(format!("open {target}: {e}")))?;
    file.write_all(&payload)
        .await
        .map_err(|e| SshError::ProxyBridge(format!("write {target}: {e}")))?;
    file.shutdown()
        .await
        .map_err(|e| SshError::ProxyBridge(format!("close {target}: {e}")))?;

    let mut attrs = FileAttributes::empty();
    attrs.permissions = Some(0o600);
    let _ = sftp.set_metadata(&target, attrs).await;

    Ok(target)
}

/// Removes a previously published bridge file. Missing files are not treated as errors.
pub async fn remove_proxy_bridge(sftp: &SftpSession, remote_path: &str) -> Result<(), SshError> {
    if remote_path.trim().is_empty() {
        return Ok(());
    }
    if !sftp.try_exists(remote_path).await.unwrap_or(true) {
        return Ok(());
    }
    match sftp.remove_file(remote_path).await {
        Ok(()) => Ok(()),
        Err(err) => {
            if !sftp.try_exists(remote_path).await.unwrap_or(false) {
                Ok(())
            } else {
                Err(SshError::ProxyBridge(format!("remove {remote_path}: {err}")))
            }
        }
    }
}
