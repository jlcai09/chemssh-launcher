use std::fs::{self, OpenOptions};
use std::io::Write;
use std::path::{Path, PathBuf};
use std::sync::Mutex;

use chrono::{SecondsFormat, Utc};
use core_config::new_id;
use serde::{Deserialize, Serialize};

pub const LAUNCHER_CLIENT_IDENTITY_VERSION: u32 = 1;
const LAUNCHER_CLIENT_ID_PREFIX: &str = "client_launcher_";

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct LauncherClientIdentity {
    pub version: u32,
    pub client_id: String,
    pub created_at: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct ChemSshBridgeClientIdentity {
    pub enabled: bool,
    pub version: u32,
    pub client_id: String,
    pub source: String,
}

impl From<&LauncherClientIdentity> for ChemSshBridgeClientIdentity {
    fn from(identity: &LauncherClientIdentity) -> Self {
        Self {
            enabled: true,
            version: identity.version,
            client_id: identity.client_id.clone(),
            source: "launcher".into(),
        }
    }
}

pub struct ClientIdentityStore {
    path: PathBuf,
    cached: Mutex<Option<LauncherClientIdentity>>,
}

impl ClientIdentityStore {
    pub fn new(path: PathBuf) -> Self {
        Self {
            path,
            cached: Mutex::new(None),
        }
    }

    /// Returns the persisted launcher client identity, creating and atomically saving
    /// a new one on first run.
    pub fn ensure(&self) -> Result<LauncherClientIdentity, String> {
        if let Ok(guard) = self.cached.lock() {
            if let Some(identity) = guard.as_ref() {
                return Ok(identity.clone());
            }
        }

        let identity = load_or_create_client_identity(&self.path)?;

        let mut guard = self
            .cached
            .lock()
            .map_err(|_| "client identity mutex poisoned".to_string())?;
        if let Some(existing) = guard.as_ref() {
            return Ok(existing.clone());
        }
        *guard = Some(identity.clone());
        Ok(identity)
    }

    /// Returns the stable launcher client id string, or an empty string if unavailable.
    pub fn client_id(&self) -> String {
        self.ensure().map(|id| id.client_id).unwrap_or_default()
    }
}

pub fn load_or_create_client_identity(path: &Path) -> Result<LauncherClientIdentity, String> {
    match read_client_identity(path) {
        Ok(identity) => return Ok(identity),
        Err(ReadIdentityError::NotFound) => {}
        Err(ReadIdentityError::Other(err)) => return Err(err),
    }

    let identity = LauncherClientIdentity {
        version: LAUNCHER_CLIENT_IDENTITY_VERSION,
        client_id: format!("{LAUNCHER_CLIENT_ID_PREFIX}{}", new_id()),
        created_at: Utc::now().to_rfc3339_opts(SecondsFormat::Secs, true),
    };
    write_client_identity(path, &identity)?;
    Ok(identity)
}

enum ReadIdentityError {
    NotFound,
    Other(String),
}

fn read_client_identity(path: &Path) -> Result<LauncherClientIdentity, ReadIdentityError> {
    let data = match fs::read(path) {
        Ok(bytes) => bytes,
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => {
            return Err(ReadIdentityError::NotFound)
        }
        Err(err) => return Err(ReadIdentityError::Other(err.to_string())),
    };
    let identity: LauncherClientIdentity = serde_json::from_slice(&data)
        .map_err(|e| ReadIdentityError::Other(format!("invalid client identity JSON: {e}")))?;
    validate_client_identity(&identity).map_err(ReadIdentityError::Other)?;
    Ok(identity)
}

fn write_client_identity(path: &Path, identity: &LauncherClientIdentity) -> Result<(), String> {
    validate_client_identity(identity)?;
    let parent = path
        .parent()
        .ok_or_else(|| "client identity path has no parent directory".to_string())?;
    create_dir_0700(parent)?;

    let mut data = serde_json::to_vec_pretty(identity)
        .map_err(|e| format!("serialize client identity: {e}"))?;
    data.push(b'\n');

    let file_name = path
        .file_name()
        .and_then(|s| s.to_str())
        .unwrap_or("client_identity.json");
    let tmp = parent.join(format!(".{file_name}.tmp"));
    write_file_0600(&tmp, &data)?;
    fs::rename(&tmp, path).map_err(|e| {
        let _ = fs::remove_file(&tmp);
        format!("commit client identity file: {e}")
    })
}

fn create_dir_0700(dir: &Path) -> Result<(), String> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::DirBuilderExt;
        std::fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(dir)
            .map_err(|e| format!("create config dir: {e}"))
    }
    #[cfg(not(unix))]
    {
        fs::create_dir_all(dir).map_err(|e| format!("create config dir: {e}"))
    }
}

fn write_file_0600(path: &Path, data: &[u8]) -> Result<(), String> {
    let mut opts = OpenOptions::new();
    opts.write(true).create(true).truncate(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        opts.mode(0o600);
    }
    let mut file = opts
        .open(path)
        .map_err(|e| format!("write client identity temp file: {e}"))?;
    file.write_all(data)
        .map_err(|e| format!("write client identity temp file: {e}"))?;
    file.sync_all()
        .map_err(|e| format!("flush client identity temp file: {e}"))?;
    Ok(())
}

pub fn validate_client_identity(identity: &LauncherClientIdentity) -> Result<(), String> {
    if identity.version != LAUNCHER_CLIENT_IDENTITY_VERSION {
        return Err("unsupported launcher client identity version".into());
    }
    if !is_valid_client_id(&identity.client_id) {
        return Err("invalid launcher client id".into());
    }
    if identity.created_at.trim().is_empty() {
        return Err("launcher client identity created_at is required".into());
    }
    Ok(())
}

fn is_valid_client_id(id: &str) -> bool {
    let bytes = id.as_bytes();
    !bytes.is_empty()
        && bytes.len() <= 80
        && bytes
            .iter()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'_' | b'.' | b'-'))
}

#[cfg(test)]
mod tests {
    use super::*;
    use tempfile::TempDir;

    #[test]
    fn creates_reloads_and_caches_stable_client_identity() {
        let dir = TempDir::new().unwrap();
        let path = dir.path().join("client_identity.json");

        let store = ClientIdentityStore::new(path.clone());
        let first = store.ensure().unwrap();
        assert_eq!(first.version, LAUNCHER_CLIENT_IDENTITY_VERSION);
        assert!(first.client_id.starts_with(LAUNCHER_CLIENT_ID_PREFIX));
        assert!(validate_client_identity(&first).is_ok());
        assert_eq!(store.client_id(), first.client_id);

        // Fresh store loading from disk must return the exact same identity.
        let reloaded = ClientIdentityStore::new(path).ensure().unwrap();
        assert_eq!(reloaded, first);
    }

    #[test]
    fn rejects_invalid_json_version_and_client_id() {
        let dir = TempDir::new().unwrap();

        let bad_json = dir.path().join("bad.json");
        fs::write(&bad_json, b"{not json").unwrap();
        assert!(load_or_create_client_identity(&bad_json)
            .unwrap_err()
            .contains("invalid client identity JSON"));

        let bad_ver = dir.path().join("bad_ver.json");
        fs::write(
            &bad_ver,
            br#"{"version":99,"client_id":"client_launcher_1","created_at":"2026-06-09T10:00:00Z"}"#,
        )
        .unwrap();
        assert!(load_or_create_client_identity(&bad_ver)
            .unwrap_err()
            .contains("unsupported"));

        let bad_id = dir.path().join("bad_id.json");
        fs::write(
            &bad_id,
            br#"{"version":1,"client_id":"bad id with spaces!","created_at":"2026-06-09T10:00:00Z"}"#,
        )
        .unwrap();
        assert!(load_or_create_client_identity(&bad_id)
            .unwrap_err()
            .contains("invalid launcher client id"));
    }
}
