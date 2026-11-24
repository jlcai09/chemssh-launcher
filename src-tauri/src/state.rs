use std::collections::VecDeque;
use std::net::TcpListener;
use std::path::PathBuf;
use std::sync::{Arc, Mutex};

use core_config::{paths, FileStore};
use core_secret::{KeyringStore, SecretStore};
use core_transfer::TransferProgressStore;
use win_native::{DragJobRegistry, FileIconService};

use crate::chemssh_bridge::ChemSshBridgeManager;
use crate::chemssh_proxy::{bind_proxy_listener, generate_api_token};
use crate::client_identity::ClientIdentityStore;
use crate::commands::sessions::SessionManager;
use crate::commands::sftp::SftpManager;
use crate::commands::sftp_sync::SftpOpenSyncManager;
use crate::commands::terminal::TerminalManager;

const MAX_LAUNCHER_LOG_LINES: usize = 2000;

#[derive(Default)]
pub struct LauncherLogStore {
    lines: Mutex<VecDeque<String>>,
}

impl LauncherLogStore {
    pub fn push(&self, line: impl Into<String>) {
        let raw = line.into();
        let trimmed = raw.trim_end_matches(['\r', '\n']);
        if trimmed.is_empty() {
            return;
        }
        if let Ok(mut guard) = self.lines.lock() {
            while guard.len() >= MAX_LAUNCHER_LOG_LINES {
                guard.pop_front();
            }
            guard.push_back(trimmed.to_string());
        }
    }

    pub fn snapshot(&self) -> Vec<String> {
        self.lines
            .lock()
            .map(|guard| guard.iter().cloned().collect())
            .unwrap_or_default()
    }
}

#[derive(Clone)]
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
    pub sftp_sync: Arc<SftpOpenSyncManager>,
    pub transfers: Arc<TransferProgressStore>,
    pub drag_jobs: Arc<DragJobRegistry>,
    pub file_icons: Arc<FileIconService>,
    pub launcher_logs: Arc<LauncherLogStore>,
    pub client_identity: Arc<ClientIdentityStore>,
    pub chemssh_bridge: Arc<ChemSshBridgeManager>,
    pub proxy_base_url: String,
    pub api_token: String,
    pub proxy_listener: Arc<Mutex<Option<TcpListener>>>,
}

impl AppState {
    pub fn new_default() -> Result<Self, String> {
        let config_dir = paths::default_dir()?;
        let profiles_path = paths::default_profiles_path()?;
        let settings_path = paths::default_settings_path()?;
        let known_hosts_path = paths::default_known_hosts_path()?;
        let sftp_cache_dir = paths::default_sftp_open_cache_dir()?;
        let client_identity_path = paths::default_client_identity_path()?;
        let (listener, proxy_base_url) = bind_proxy_listener()?;
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
            sftp_sync: Arc::new(SftpOpenSyncManager::new()),
            transfers: Arc::new(TransferProgressStore::new()),
            drag_jobs: Arc::new(DragJobRegistry::new()),
            file_icons: Arc::new(FileIconService::new()),
            launcher_logs: Arc::new(LauncherLogStore::default()),
            client_identity: Arc::new(ClientIdentityStore::new(client_identity_path)),
            chemssh_bridge: Arc::new(ChemSshBridgeManager::new()),
            proxy_base_url,
            api_token: generate_api_token(),
            proxy_listener: Arc::new(Mutex::new(Some(listener))),
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
        let client_identity_path = config_dir.join("client_identity.json");
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
            sftp_sync: Arc::new(SftpOpenSyncManager::new()),
            transfers: Arc::new(TransferProgressStore::new()),
            drag_jobs: Arc::new(DragJobRegistry::new()),
            file_icons: Arc::new(FileIconService::new()),
            launcher_logs: Arc::new(LauncherLogStore::default()),
            client_identity: Arc::new(ClientIdentityStore::new(client_identity_path)),
            chemssh_bridge: Arc::new(ChemSshBridgeManager::new()),
            proxy_base_url: String::new(),
            api_token: generate_api_token(),
            proxy_listener: Arc::new(Mutex::new(None)),
        }
    }
}
