pub mod keyring_store;
pub mod store;
pub mod vault_store;

use thiserror::Error;

pub use keyring_store::KeyringStore;
pub use store::{
    username, MemoryStore, SecretStore, KEY_PASSWORD, KEY_PRIVATE_PASSPHRASE, KEY_SECURITY_TOKEN,
    SERVICE_NAME,
};
pub use vault_store::VaultStore;

#[derive(Debug, Error)]
pub enum SecretError {
    #[error("vault master password is required")]
    MissingMasterPassword,
    #[error("could not decrypt vault")]
    DecryptFailed,
    #[error("vault error: {0}")]
    Vault(String),
    #[error("secret backend error: {0}")]
    Backend(String),
    #[error("io error: {0}")]
    Io(#[from] std::io::Error),
}

#[cfg(test)]
mod tests {
    use super::*;
    use tempfile::tempdir;

    #[test]
    fn memory_store_roundtrip_and_delete_all() {
        let store = MemoryStore::new();
        store.set("p1", KEY_PASSWORD, "dummy-password").unwrap();
        store.set("p1", KEY_SECURITY_TOKEN, "dummy-token").unwrap();

        assert_eq!(
            store.get("p1", KEY_PASSWORD).unwrap().as_deref(),
            Some("dummy-password")
        );
        assert_eq!(store.get("p1", KEY_PRIVATE_PASSPHRASE).unwrap(), None);

        store.delete_profile_secrets("p1").unwrap();
        assert_eq!(store.get("p1", KEY_PASSWORD).unwrap(), None);
        assert_eq!(store.get("p1", KEY_SECURITY_TOKEN).unwrap(), None);
    }

    #[test]
    fn vault_store_encrypts_at_rest_and_rejects_wrong_password() {
        let dir = tempdir().unwrap();
        let path = dir.path().join("vault.json");

        let store = VaultStore::new(&path, "master-pass-1");
        store.set("p1", KEY_PASSWORD, "REPLACE_ME_SECRET").unwrap();

        let raw = std::fs::read_to_string(&path).unwrap();
        assert!(!raw.contains("REPLACE_ME_SECRET"));
        assert!(!raw.contains("master-pass-1"));

        let reopened = VaultStore::new(&path, "master-pass-1");
        assert_eq!(
            reopened.get("p1", KEY_PASSWORD).unwrap().as_deref(),
            Some("REPLACE_ME_SECRET")
        );

        let wrong = VaultStore::new(&path, "wrong-master-pass");
        let err = wrong.get("p1", KEY_PASSWORD).unwrap_err();
        assert!(matches!(err, SecretError::DecryptFailed));
        assert!(!err.to_string().contains("wrong-master-pass"));
    }
}
