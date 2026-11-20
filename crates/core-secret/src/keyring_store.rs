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
}

impl SecretStore for KeyringStore {
    fn set(&self, profile_id: &str, key: &str, value: &str) -> Result<(), SecretError> {
        let entry = keyring::Entry::new(&self.service, &username(profile_id, key))
            .map_err(|e| SecretError::Backend(format!("keyring entry init failed: {e}")))?;
        entry
            .set_password(value)
            .map_err(|e| SecretError::Backend(format!("keyring write failed: {e}")))
    }

    fn get(&self, profile_id: &str, key: &str) -> Result<Option<String>, SecretError> {
        let entry = keyring::Entry::new(&self.service, &username(profile_id, key))
            .map_err(|e| SecretError::Backend(format!("keyring entry init failed: {e}")))?;
        match entry.get_password() {
            Ok(secret) => Ok(Some(secret)),
            Err(keyring::Error::NoEntry) => Ok(None),
            Err(e) => Err(SecretError::Backend(format!("keyring read failed: {e}"))),
        }
    }

    fn delete(&self, profile_id: &str, key: &str) -> Result<(), SecretError> {
        let entry = keyring::Entry::new(&self.service, &username(profile_id, key))
            .map_err(|e| SecretError::Backend(format!("keyring entry init failed: {e}")))?;
        match entry.delete_credential() {
            Ok(()) | Err(keyring::Error::NoEntry) => Ok(()),
            Err(e) => Err(SecretError::Backend(format!("keyring delete failed: {e}"))),
        }
    }
}
