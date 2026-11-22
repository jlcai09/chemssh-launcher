use crate::{username, SecretError, SecretStore, SERVICE_NAME};

#[derive(Debug, Clone)]
pub struct KeyringStore {
    pub service: String,
}

impl Default for KeyringStore {
    fn default() -> Self {
        Self::new()
    }
}

impl KeyringStore {
    pub fn new() -> Self {
        Self {
            service: SERVICE_NAME.to_string(),
        }
    }

    /// Creates an entry compatible with `github.com/zalando/go-keyring` on Windows,
    /// where `TargetName` is `<service>:<profile_id>:<key>` (`chemssh-launcher:<id>:password`).
    fn go_compatible_entry(&self, profile_id: &str, key: &str) -> Result<keyring::Entry, SecretError> {
        let user = username(profile_id, key);
        let target = format!("{}:{user}", self.service);
        keyring::Entry::new_with_target(&target, &self.service, &user)
            .map_err(|e| SecretError::Backend(format!("keyring entry init failed: {e}")))
    }

    fn default_entry(&self, profile_id: &str, key: &str) -> Result<keyring::Entry, SecretError> {
        let user = username(profile_id, key);
        keyring::Entry::new(&self.service, &user)
            .map_err(|e| SecretError::Backend(format!("keyring entry init failed: {e}")))
    }
}

fn read_entry_flexible(entry: &keyring::Entry) -> Result<Option<String>, SecretError> {
    // `zalando/go-keyring` (`wincred`) stores raw UTF-8 bytes in `CredentialBlob`,
    // whereas `keyring::Entry::get_password()` on Windows expects UTF-16LE.
    // Reading raw bytes via `get_secret()` first handles Go-saved credentials directly,
    // and falls back to `get_password()` for UTF-16LE entries written by `keyring-rs`.
    match entry.get_secret() {
        Ok(bytes) => {
            if let Ok(utf8) = String::from_utf8(bytes.clone()) {
                if !utf8.contains('\0') {
                    return Ok(Some(utf8));
                }
            }
            match entry.get_password() {
                Ok(pw) => Ok(Some(pw)),
                Err(keyring::Error::NoEntry) => Ok(None),
                Err(e) => Err(SecretError::Backend(format!("keyring read failed: {e}"))),
            }
        }
        Err(keyring::Error::NoEntry) => Ok(None),
        Err(_) => match entry.get_password() {
            Ok(pw) => Ok(Some(pw)),
            Err(keyring::Error::NoEntry) => Ok(None),
            Err(e) => Err(SecretError::Backend(format!("keyring read failed: {e}"))),
        },
    }
}

impl SecretStore for KeyringStore {
    fn set(&self, profile_id: &str, key: &str, value: &str) -> Result<(), SecretError> {
        #[cfg(target_os = "windows")]
        {
            // Write raw UTF-8 bytes under TargetName `chemssh-launcher:<id>:<key>` so
            // credentials remain 100% interchangeable with the Go launcher on Windows.
            let entry = self.go_compatible_entry(profile_id, key)?;
            return entry
                .set_secret(value.as_bytes())
                .map_err(|e| SecretError::Backend(format!("keyring write failed: {e}")));
        }
        #[cfg(not(target_os = "windows"))]
        {
            let entry = self.default_entry(profile_id, key)?;
            entry
                .set_password(value)
                .map_err(|e| SecretError::Backend(format!("keyring write failed: {e}")))
        }
    }

    fn get(&self, profile_id: &str, key: &str) -> Result<Option<String>, SecretError> {
        let go_entry = self.go_compatible_entry(profile_id, key)?;
        if let Some(secret) = read_entry_flexible(&go_entry)? {
            return Ok(Some(secret));
        }
        let default_entry = self.default_entry(profile_id, key)?;
        read_entry_flexible(&default_entry)
    }

    fn delete(&self, profile_id: &str, key: &str) -> Result<(), SecretError> {
        let go_entry = self.go_compatible_entry(profile_id, key)?;
        let go_res = match go_entry.delete_credential() {
            Ok(()) | Err(keyring::Error::NoEntry) => Ok(()),
            Err(e) => Err(SecretError::Backend(format!("keyring delete failed: {e}"))),
        };
        let default_entry = self.default_entry(profile_id, key)?;
        let def_res = match default_entry.delete_credential() {
            Ok(()) | Err(keyring::Error::NoEntry) => Ok(()),
            Err(e) => Err(SecretError::Backend(format!("keyring delete failed: {e}"))),
        };
        go_res.and(def_res)
    }
}
