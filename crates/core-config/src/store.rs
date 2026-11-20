use std::collections::{HashMap, HashSet};
use std::fs;
use std::path::{Path, PathBuf};

use rand::RngCore;
use serde::{Deserialize, Serialize};

use crate::{ConfigError, Profile};

#[derive(Debug, Clone, Serialize, Deserialize)]
struct FileData {
    #[serde(default = "default_version")]
    version: u32,
    #[serde(default)]
    profiles: Vec<Profile>,
}

fn default_version() -> u32 {
    1
}

impl Default for FileData {
    fn default() -> Self {
        Self {
            version: 1,
            profiles: Vec::new(),
        }
    }
}

pub trait ProfileStore: Send + Sync {
    fn list(&self) -> Result<Vec<Profile>, ConfigError>;
    fn get(&self, id_or_name: &str) -> Result<Profile, ConfigError>;
    fn save(&self, profile: Profile) -> Result<Profile, ConfigError>;
    fn delete(&self, id_or_name: &str) -> Result<(), ConfigError>;
    fn reorder(&self, ids: &[String]) -> Result<(), ConfigError>;
}

#[derive(Debug, Clone)]
pub struct FileStore {
    pub path: PathBuf,
}

impl FileStore {
    pub fn new(path: impl Into<PathBuf>) -> Self {
        Self { path: path.into() }
    }

    pub fn duplicate(&self, id_or_name: &str) -> Result<Profile, ConfigError> {
        let mut copy = self.get(id_or_name)?;
        copy.id = new_id();
        copy.name = format!("{} (Copy)", copy.name);
        // Never claim secrets are saved on a duplicated profile; user must set them explicitly.
        copy.has_password = false;
        copy.has_private_key_passphrase = false;
        copy.has_security_token = false;
        self.save(copy)
    }

    /// Exports profiles as JSON without any secret values (flags reset to false).
    pub fn export_json(&self) -> Result<String, ConfigError> {
        let mut profiles = self.list()?;
        for p in &mut profiles {
            p.has_password = false;
            p.has_private_key_passphrase = false;
            p.has_security_token = false;
        }
        let payload = FileData {
            version: 1,
            profiles,
        };
        let mut out = serde_json::to_string_pretty(&payload)?;
        out.push('\n');
        Ok(out)
    }

    /// Imports profiles from JSON, validating each entry and stripping secret flags.
    pub fn import_json(&self, raw_json: &str) -> Result<Vec<Profile>, ConfigError> {
        let parsed: FileData = serde_json::from_str(raw_json)?;
        let mut imported = Vec::with_capacity(parsed.profiles.len());
        for mut profile in parsed.profiles {
            profile.local_proxy = profile.local_proxy.normalized();
            profile.x11 = profile.x11.normalized();
            profile.has_password = false;
            profile.has_private_key_passphrase = false;
            profile.has_security_token = false;
            profile.validate()?;
            let saved = self.save(profile)?;
            imported.push(saved);
        }
        Ok(imported)
    }

    fn load(&self) -> Result<FileData, ConfigError> {
        let bytes = match fs::read(&self.path) {
            Ok(b) => b,
            Err(err) if err.kind() == std::io::ErrorKind::NotFound => return Ok(FileData::default()),
            Err(err) => return Err(ConfigError::Io(err)),
        };
        if bytes.is_empty() {
            return Ok(FileData::default());
        }
        let mut data: FileData = serde_json::from_slice(&bytes)?;
        if data.version == 0 {
            data.version = 1;
        }
        for profile in &mut data.profiles {
            profile.local_proxy = profile.local_proxy.normalized();
            profile.x11 = profile.x11.normalized();
        }
        Ok(data)
    }

    fn persist(&self, mut data: FileData) -> Result<(), ConfigError> {
        if data.version == 0 {
            data.version = 1;
        }
        write_json_atomic(&self.path, &data)
    }
}

impl ProfileStore for FileStore {
    fn list(&self) -> Result<Vec<Profile>, ConfigError> {
        Ok(self.load()?.profiles)
    }

    fn get(&self, id_or_name: &str) -> Result<Profile, ConfigError> {
        let data = self.load()?;
        for p in data.profiles {
            if p.id == id_or_name || p.name.eq_ignore_ascii_case(id_or_name) {
                return Ok(p);
            }
        }
        Err(ConfigError::ProfileNotFound(id_or_name.to_string()))
    }

    fn save(&self, mut profile: Profile) -> Result<Profile, ConfigError> {
        profile.local_proxy = profile.local_proxy.normalized();
        profile.x11 = profile.x11.normalized();
        if profile.id.is_empty() {
            profile.id = new_id();
        }

        let mut data = self.load()?;
        if let Some(existing) = data.profiles.iter_mut().find(|p| p.id == profile.id) {
            *existing = profile.clone();
        } else {
            data.profiles.push(profile.clone());
        }
        self.persist(data)?;
        Ok(profile)
    }

    fn delete(&self, id_or_name: &str) -> Result<(), ConfigError> {
        let mut data = self.load()?;
        let pos = data
            .profiles
            .iter()
            .position(|p| p.id == id_or_name || p.name.eq_ignore_ascii_case(id_or_name))
            .ok_or_else(|| ConfigError::ProfileNotFound(id_or_name.to_string()))?;
        data.profiles.remove(pos);
        self.persist(data)
    }

    fn reorder(&self, ids: &[String]) -> Result<(), ConfigError> {
        let mut data = self.load()?;
        let by_id: HashMap<String, Profile> = data
            .profiles
            .iter()
            .cloned()
            .map(|p| (p.id.clone(), p))
            .collect();
        let mut seen = HashSet::with_capacity(ids.len());
        let mut ordered = Vec::with_capacity(data.profiles.len());

        for id in ids {
            if id.is_empty() {
                return Err(ConfigError::InvalidProfile("profile id is required".into()));
            }
            if !seen.insert(id.clone()) {
                return Err(ConfigError::InvalidProfile(format!(
                    "duplicate profile id: {id}"
                )));
            }
            let profile = by_id
                .get(id)
                .ok_or_else(|| ConfigError::ProfileNotFound(id.clone()))?;
            ordered.push(profile.clone());
        }
        for profile in data.profiles {
            if !seen.contains(&profile.id) {
                ordered.push(profile);
            }
        }
        data.profiles = ordered;
        self.persist(data)
    }
}

pub fn new_id() -> String {
    let mut bytes = [0u8; 16];
    rand::rng().fill_bytes(&mut bytes);
    hex::encode(bytes)
}

pub(crate) fn write_json_atomic<T: Serialize>(path: &Path, value: &T) -> Result<(), ConfigError> {
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let _ = fs::set_permissions(parent, fs::Permissions::from_mode(0o700));
        }
    }

    let mut content = serde_json::to_vec_pretty(value)?;
    content.push(b'\n');

    let tmp_name = format!(
        ".{}.{}.tmp",
        path.file_name().and_then(|s| s.to_str()).unwrap_or("config"),
        new_id()
    );
    let tmp_path = path.with_file_name(tmp_name);
    fs::write(&tmp_path, &content)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let _ = fs::set_permissions(&tmp_path, fs::Permissions::from_mode(0o600));
    }
    fs::rename(&tmp_path, path)?;
    Ok(())
}
