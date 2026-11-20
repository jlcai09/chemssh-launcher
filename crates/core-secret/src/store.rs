use std::collections::HashMap;
use std::sync::Mutex;

use crate::SecretError;

pub const SERVICE_NAME: &str = "chemssh-launcher";
pub const KEY_PASSWORD: &str = "password";
pub const KEY_PRIVATE_PASSPHRASE: &str = "key-passphrase";
pub const KEY_SECURITY_TOKEN: &str = "security-token";

pub fn username(profile_id: &str, key: &str) -> String {
    format!("{profile_id}:{key}")
}

pub trait SecretStore: Send + Sync {
    fn set(&self, profile_id: &str, key: &str, value: &str) -> Result<(), SecretError>;
    fn get(&self, profile_id: &str, key: &str) -> Result<Option<String>, SecretError>;
    fn delete(&self, profile_id: &str, key: &str) -> Result<(), SecretError>;

    fn delete_profile_secrets(&self, profile_id: &str) -> Result<(), SecretError> {
        for key in [KEY_PASSWORD, KEY_PRIVATE_PASSPHRASE, KEY_SECURITY_TOKEN] {
            self.delete(profile_id, key)?;
        }
        Ok(())
    }
}

#[derive(Default)]
pub struct MemoryStore {
    values: Mutex<HashMap<String, String>>,
}

impl MemoryStore {
    pub fn new() -> Self {
        Self::default()
    }
}

impl SecretStore for MemoryStore {
    fn set(&self, profile_id: &str, key: &str, value: &str) -> Result<(), SecretError> {
        let mut guard = self
            .values
            .lock()
            .map_err(|_| SecretError::Backend("memory secret store lock poisoned".into()))?;
        guard.insert(username(profile_id, key), value.to_string());
        Ok(())
    }

    fn get(&self, profile_id: &str, key: &str) -> Result<Option<String>, SecretError> {
        let guard = self
            .values
            .lock()
            .map_err(|_| SecretError::Backend("memory secret store lock poisoned".into()))?;
        Ok(guard.get(&username(profile_id, key)).cloned())
    }

    fn delete(&self, profile_id: &str, key: &str) -> Result<(), SecretError> {
        let mut guard = self
            .values
            .lock()
            .map_err(|_| SecretError::Backend("memory secret store lock poisoned".into()))?;
        guard.remove(&username(profile_id, key));
        Ok(())
    }
}
