use std::path::PathBuf;
use std::sync::Arc;

use core_config::{paths, FileStore};
use core_secret::{KeyringStore, SecretStore};
use core_transfer::TransferProgressStore;

use crate::commands::sessions::SessionManager;
use crate::commands::sftp::SftpManager;
use crate::commands::terminal::TerminalManager;

pub struct AppState {
    pub profiles: Arc<FileStore>,
    pub secrets: Arc<dyn SecretStore>,
    pub config_dir: PathBuf,
    pub settings_path: PathBuf,
    pub known_hosts_path: PathBuf,
    pub sftp_cache_dir: PathBuf,
    pub sessions: Arc<SessionManager>,
    pub terminals: Arc<TerminalManager>,
    pub sftp: Arc<SftpManager>,
    pub transfers: Arc<TransferProgressStore>,
}

impl AppState {
    pub fn new_default() -> Result<Self, String> {
        let config_dir = paths::default_dir()?;
        let profiles_path = paths::default_profiles_path()?;
        let settings_path = paths::default_settings_path()?;
        let known_hosts_path = paths::default_known_hosts_path()?;
        let sftp_cache_dir = paths::default_sftp_open_cache_dir()?;
        Ok(Self {
            profiles: Arc::new(FileStore::new(profiles_path)),
            secrets: Arc::new(KeyringStore::new()),
            config_dir,
            settings_path,
            known_hosts_path,
            sftp_cache_dir,
            sessions: Arc::new(SessionManager::default()),
            terminals: Arc::new(TerminalManager::default()),
            sftp: Arc::new(SftpManager::default()),
            transfers: Arc::new(TransferProgressStore::new()),
        })
    }

    pub fn new_custom(
        config_dir: PathBuf,
        profiles_path: PathBuf,
        settings_path: PathBuf,
        sftp_cache_dir: PathBuf,
        secrets: Arc<dyn SecretStore>,
    ) -> Self {
        let known_hosts_path = config_dir.join("known_hosts");
        Self {
            profiles: Arc::new(FileStore::new(profiles_path)),
            secrets,
            config_dir,
            settings_path,
            known_hosts_path,
            sftp_cache_dir,
            sessions: Arc::new(SessionManager::default()),
            terminals: Arc::new(TerminalManager::default()),
            sftp: Arc::new(SftpManager::default()),
            transfers: Arc::new(TransferProgressStore::new()),
        }
    }
}
