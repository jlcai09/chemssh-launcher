use std::fs::{self, OpenOptions};
use std::io::Write;
use std::path::{Path, PathBuf};
use std::sync::Mutex;

use base64::engine::general_purpose::STANDARD_NO_PAD as BASE64_NO_PAD;
use base64::Engine;
use russh::keys::PublicKey;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use crate::SshError;

static KNOWN_HOSTS_LOCK: Mutex<()> = Mutex::new(());

#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum HostKeyPolicy {
    #[default]
    Strict,
    AcceptNew,
    ReplaceChanged,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct HostKeyVerificationError {
    pub host: String,
    pub address: String,
    pub key_type: String,
    pub fingerprint: String,
    pub known_hosts_path: String,
    pub previous_fingerprints: Vec<String>,
    pub mismatch: bool,
}

impl std::fmt::Display for HostKeyVerificationError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        if self.mismatch {
            write!(
                f,
                "ssh host key mismatch for {} ({} {}); possible man-in-the-middle attack or server reinstallation. Check {} before trusting this host again",
                self.address, self.key_type, self.fingerprint, self.known_hosts_path
            )
        } else {
            write!(
                f,
                "unknown ssh host key for {} ({} {}). Confirm this fingerprint with the server administrator, then trust it to save it in {}",
                self.address, self.key_type, self.fingerprint, self.known_hosts_path
            )
        }
    }
}

pub fn normalize_host_address(host: &str, port: u16) -> String {
    let trimmed = host.trim();
    if port == 22 {
        trimmed.to_string()
    } else if trimmed.contains(':') && !trimmed.starts_with('[') {
        format!("[{trimmed}]:{port}")
    } else {
        format!("[{trimmed}]:{port}")
    }
}

pub fn fingerprint_sha256(key: &PublicKey) -> String {
    if let Ok(openssh) = key.to_openssh() {
        let mut parts = openssh.split_whitespace();
        let _algo = parts.next();
        if let Some(b64) = parts.next() {
            if let Ok(raw) = base64::engine::general_purpose::STANDARD.decode(b64) {
                let digest = Sha256::digest(&raw);
                return format!("SHA256:{}", BASE64_NO_PAD.encode(digest));
            }
        }
    }
    key.fingerprint(russh::keys::HashAlg::Sha256).to_string()
}

pub fn key_type_name(key: &PublicKey) -> String {
    key.algorithm().as_str().to_string()
}

#[derive(Debug, Clone)]
pub struct KnownHostsVerifier {
    pub path: PathBuf,
    pub host: String,
    pub port: u16,
    pub policy: HostKeyPolicy,
}

impl KnownHostsVerifier {
    pub fn new(path: impl Into<PathBuf>, host: impl Into<String>, port: u16, policy: HostKeyPolicy) -> Self {
        Self {
            path: path.into(),
            host: host.into(),
            port,
            policy,
        }
    }

    pub fn verify(&self, key: &PublicKey) -> Result<(), SshError> {
        let _guard = KNOWN_HOSTS_LOCK
            .lock()
            .map_err(|_| SshError::HostKeyInternal("known_hosts lock poisoned".into()))?;
        ensure_known_hosts_file(&self.path)?;

        let address = normalize_host_address(&self.host, self.port);
        let key_type = key_type_name(key);
        let fingerprint = fingerprint_sha256(key);
        let openssh_key = key
            .to_openssh()
            .map_err(|e| SshError::HostKeyInternal(format!("encode public key: {e}")))?;
        let key_parts: Vec<&str> = openssh_key.split_whitespace().collect();
        if key_parts.len() < 2 {
            return Err(SshError::HostKeyInternal("malformed openssh public key".into()));
        }
        let target_key_body = format!("{} {}", key_parts[0], key_parts[1]);

        let content = fs::read_to_string(&self.path).unwrap_or_default();
        let mut previous_fingerprints = Vec::new();
        let mut matched_exact = false;

        for line in content.lines() {
            let trimmed = line.trim();
            if trimmed.is_empty() || trimmed.starts_with('#') {
                continue;
            }
            let fields: Vec<&str> = trimmed.split_whitespace().collect();
            if fields.len() < 3 {
                continue;
            }
            let hosts_field = fields[0];
            if !host_matches(hosts_field, &address) {
                continue;
            }
            let entry_key = format!("{} {}", fields[1], fields[2]);
            if entry_key == target_key_body {
                matched_exact = true;
                break;
            }
            if let Ok(parsed) = PublicKey::from_openssh(&entry_key) {
                previous_fingerprints.push(fingerprint_sha256(&parsed));
            }
        }

        if matched_exact {
            return Ok(());
        }

        let mismatch = !previous_fingerprints.is_empty();
        let verification_err = HostKeyVerificationError {
            host: self.host.clone(),
            address: address.clone(),
            key_type,
            fingerprint,
            known_hosts_path: self.path.display().to_string(),
            previous_fingerprints,
            mismatch,
        };

        match self.policy {
            HostKeyPolicy::Strict => Err(SshError::HostKeyVerification(verification_err)),
            HostKeyPolicy::AcceptNew => {
                if mismatch {
                    Err(SshError::HostKeyVerification(verification_err))
                } else {
                    append_known_host(&self.path, &address, &target_key_body)
                }
            }
            HostKeyPolicy::ReplaceChanged => {
                if mismatch {
                    replace_known_host(&self.path, &address, &target_key_body)
                } else {
                    append_known_host(&self.path, &address, &target_key_body)
                }
            }
        }
    }
}

fn host_matches(hosts_field: &str, target: &str) -> bool {
    hosts_field
        .split(',')
        .any(|entry| entry.eq_ignore_ascii_case(target))
}

fn ensure_known_hosts_file(path: &Path) -> Result<(), SshError> {
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)?;
    }
    let _ = OpenOptions::new().create(true).append(true).open(path)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let _ = fs::set_permissions(path, fs::Permissions::from_mode(0o600));
    }
    Ok(())
}

fn append_known_host(path: &Path, address: &str, key_body: &str) -> Result<(), SshError> {
    let mut file = OpenOptions::new().create(true).append(true).open(path)?;
    writeln!(file, "{address} {key_body}")?;
    Ok(())
}

fn replace_known_host(path: &Path, address: &str, key_body: &str) -> Result<(), SshError> {
    let content = fs::read_to_string(path).unwrap_or_default();
    let mut kept = Vec::new();
    for line in content.lines() {
        let trimmed = line.trim();
        let mut drop_line = false;
        if !trimmed.is_empty() && !trimmed.starts_with('#') {
            if let Some(first) = trimmed.split_whitespace().next() {
                if host_matches(first, address) {
                    drop_line = true;
                }
            }
        }
        if !drop_line {
            kept.push(line.to_string());
        }
    }
    kept.push(format!("{address} {key_body}"));
    let mut out = kept.join("\n");
    out.push('\n');
    fs::write(path, out)?;
    Ok(())
}
