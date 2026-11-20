use std::collections::HashMap;
use std::fs;
use std::path::PathBuf;

use aes_gcm::aead::{Aead, KeyInit};
use aes_gcm::{Aes256Gcm, Nonce};
use argon2::{Algorithm, Argon2, Params, Version};
use base64::engine::general_purpose::STANDARD as BASE64;
use base64::Engine;
use rand::RngCore;
use serde::{Deserialize, Serialize};

use crate::{username, SecretError, SecretStore};

const VAULT_VERSION: u32 = 1;
const ARGON2_TIME_COST: u32 = 3;
const ARGON2_MEMORY_KIB: u32 = 64 * 1024;
const ARGON2_PARALLELISM: u32 = 4;
const KEY_LEN: usize = 32;

#[derive(Serialize, Deserialize)]
struct VaultFile {
    version: u32,
    salt: String,
    nonce: String,
    data: String,
}

/// Encrypted file fallback compatible with the Go launcher's `vault.json` format.
/// Never stores or logs `master_password` or decrypted secret values.
pub struct VaultStore {
    pub path: PathBuf,
    master_password: String,
}

impl VaultStore {
    pub fn new(path: impl Into<PathBuf>, master_password: impl Into<String>) -> Self {
        Self {
            path: path.into(),
            master_password: master_password.into(),
        }
    }

    fn load(&self) -> Result<HashMap<String, String>, SecretError> {
        if self.master_password.is_empty() {
            return Err(SecretError::MissingMasterPassword);
        }
        let bytes = match fs::read(&self.path) {
            Ok(b) => b,
            Err(err) if err.kind() == std::io::ErrorKind::NotFound => return Ok(HashMap::new()),
            Err(err) => return Err(SecretError::Io(err)),
        };

        let vf: VaultFile = serde_json::from_slice(&bytes)
            .map_err(|_| SecretError::Vault("invalid vault file format".into()))?;
        if vf.version != VAULT_VERSION {
            return Err(SecretError::Vault(format!(
                "unsupported vault version {}",
                vf.version
            )));
        }

        let salt = BASE64
            .decode(&vf.salt)
            .map_err(|_| SecretError::Vault("invalid vault salt encoding".into()))?;
        let nonce_bytes = BASE64
            .decode(&vf.nonce)
            .map_err(|_| SecretError::Vault("invalid vault nonce encoding".into()))?;
        let ciphertext = BASE64
            .decode(&vf.data)
            .map_err(|_| SecretError::Vault("invalid vault payload encoding".into()))?;
        if nonce_bytes.len() != 12 {
            return Err(SecretError::Vault("invalid vault nonce length".into()));
        }

        let cipher = self.build_cipher(&salt)?;
        let nonce = Nonce::from_slice(&nonce_bytes);
        let plaintext = cipher
            .decrypt(nonce, ciphertext.as_ref())
            .map_err(|_| SecretError::DecryptFailed)?;

        if plaintext.is_empty() {
            return Ok(HashMap::new());
        }
        serde_json::from_slice(&plaintext)
            .map_err(|_| SecretError::Vault("invalid decrypted vault payload".into()))
    }

    fn save(&self, values: &HashMap<String, String>) -> Result<(), SecretError> {
        if self.master_password.is_empty() {
            return Err(SecretError::MissingMasterPassword);
        }
        let mut salt = [0u8; 16];
        let mut nonce_bytes = [0u8; 12];
        rand::rng().fill_bytes(&mut salt);
        rand::rng().fill_bytes(&mut nonce_bytes);

        let cipher = self.build_cipher(&salt)?;
        let plaintext = serde_json::to_vec(values)
            .map_err(|_| SecretError::Vault("failed to encode vault payload".into()))?;
        let ciphertext = cipher
            .encrypt(Nonce::from_slice(&nonce_bytes), plaintext.as_ref())
            .map_err(|_| SecretError::Vault("failed to encrypt vault payload".into()))?;

        let vf = VaultFile {
            version: VAULT_VERSION,
            salt: BASE64.encode(salt),
            nonce: BASE64.encode(nonce_bytes),
            data: BASE64.encode(ciphertext),
        };
        let mut out = serde_json::to_vec_pretty(&vf)
            .map_err(|_| SecretError::Vault("failed to serialize vault file".into()))?;
        out.push(b'\n');

        if let Some(parent) = self.path.parent() {
            fs::create_dir_all(parent)?;
            #[cfg(unix)]
            {
                use std::os::unix::fs::PermissionsExt;
                let _ = fs::set_permissions(parent, fs::Permissions::from_mode(0o700));
            }
        }
        fs::write(&self.path, out)?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let _ = fs::set_permissions(&self.path, fs::Permissions::from_mode(0o600));
        }
        Ok(())
    }

    fn build_cipher(&self, salt: &[u8]) -> Result<Aes256Gcm, SecretError> {
        let params = Params::new(
            ARGON2_MEMORY_KIB,
            ARGON2_TIME_COST,
            ARGON2_PARALLELISM,
            Some(KEY_LEN),
        )
        .map_err(|_| SecretError::Vault("invalid argon2 parameters".into()))?;
        let argon2 = Argon2::new(Algorithm::Argon2id, Version::V0x13, params);
        let mut key = [0u8; KEY_LEN];
        argon2
            .hash_password_into(self.master_password.as_bytes(), salt, &mut key)
            .map_err(|_| SecretError::Vault("failed to derive vault key".into()))?;
        Aes256Gcm::new_from_slice(&key)
            .map_err(|_| SecretError::Vault("failed to initialize vault cipher".into()))
    }
}

impl SecretStore for VaultStore {
    fn set(&self, profile_id: &str, key: &str, value: &str) -> Result<(), SecretError> {
        let mut values = self.load()?;
        values.insert(username(profile_id, key), value.to_string());
        self.save(&values)
    }

    fn get(&self, profile_id: &str, key: &str) -> Result<Option<String>, SecretError> {
        let values = self.load()?;
        Ok(values.get(&username(profile_id, key)).cloned())
    }

    fn delete(&self, profile_id: &str, key: &str) -> Result<(), SecretError> {
        let mut values = self.load()?;
        values.remove(&username(profile_id, key));
        self.save(&values)
    }
}
