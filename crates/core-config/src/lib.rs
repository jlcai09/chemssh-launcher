pub mod local_proxy;
pub mod paths;
pub mod profile;
pub mod settings;
pub mod store;
pub mod x11;

use thiserror::Error;

pub use local_proxy::{
    proxy_env_pairs, EnvPair, LocalProxy, DEFAULT_LOCAL_PROXY_HOST, DEFAULT_LOCAL_PROXY_NO_PROXY,
    DEFAULT_LOCAL_PROXY_PORT,
};
pub use profile::{
    Profile, AUTH_PASSWORD, AUTH_PRIVATE_KEY, DEFAULT_START_COMMAND, PROFILE_KIND_LOCAL,
    PROFILE_KIND_REMOTE,
};
pub use settings::{
    load_ui_settings, load_ui_settings_file, normalize_ui_language, save_ui_settings,
    save_ui_settings_file, UISettings, DEFAULT_TERMINAL_FONT_SIZE, DEFAULT_TERMINAL_SCROLLBACK,
    LANGUAGE_CHINESE, LANGUAGE_ENGLISH, LANGUAGE_SYSTEM, MAX_TERMINAL_FONT_SIZE,
    MAX_TERMINAL_SCROLLBACK, MIN_TERMINAL_FONT_SIZE, MIN_TERMINAL_SCROLLBACK,
};
pub use store::{new_id, FileStore, ProfileStore};
pub use x11::{X11Config, DEFAULT_X11_DISPLAY};

#[derive(Debug, Error)]
pub enum ConfigError {
    #[error("profile invalid: {0}")]
    InvalidProfile(String),
    #[error("profile not found: {0}")]
    ProfileNotFound(String),
    #[error("invalid settings: {0}")]
    InvalidSettings(String),
    #[error("config path error: {0}")]
    Path(String),
    #[error("io error: {0}")]
    Io(#[from] std::io::Error),
    #[error("json error: {0}")]
    Json(#[from] serde_json::Error),
}

#[cfg(test)]
mod tests {
    use super::*;
    use tempfile::tempdir;

    #[test]
    fn profile_json_roundtrip_and_legacy_normalization() {
        let dir = tempdir().unwrap();
        let path = dir.path().join("profiles.json");
        let legacy_json = r#"{
  "version": 1,
  "profiles": [
    {
      "id": "p1",
      "name": "Example HPC",
      "ssh_host": "example.com",
      "ssh_port": 22,
      "ssh_user": "user",
      "auth_method": "password",
      "remote_host": "127.0.0.1",
      "remote_port": 8888,
      "local_host": "127.0.0.1",
      "local_port": 8888
    }
  ]
}"#;
        std::fs::write(&path, legacy_json).unwrap();

        let store = FileStore::new(&path);
        let list = store.list().unwrap();
        assert_eq!(list.len(), 1);
        let p = &list[0];
        assert_eq!(p.local_proxy.host, DEFAULT_LOCAL_PROXY_HOST);
        assert_eq!(p.local_proxy.port, DEFAULT_LOCAL_PROXY_PORT);
        assert_eq!(p.local_proxy.no_proxy, DEFAULT_LOCAL_PROXY_NO_PROXY);
        assert_eq!(p.x11.display, DEFAULT_X11_DISPLAY);
        assert!(p.validate().is_ok());
    }

    #[test]
    fn proxy_env_pairs_case_and_empty_no_proxy() {
        let with_no_proxy = proxy_env_pairs("http://127.0.0.1:41234", "localhost,127.0.0.1");
        assert_eq!(with_no_proxy.len(), 8);
        assert!(with_no_proxy.iter().any(|p| p.key == "http_proxy"));
        assert!(with_no_proxy.iter().any(|p| p.key == "HTTP_PROXY"));
        assert!(with_no_proxy.iter().any(|p| p.key == "no_proxy"));
        assert!(with_no_proxy.iter().any(|p| p.key == "NO_PROXY"));

        let without_no_proxy = proxy_env_pairs("http://127.0.0.1:41234", "   ");
        assert_eq!(without_no_proxy.len(), 6);
        assert!(!without_no_proxy.iter().any(|p| p.key == "no_proxy"));
    }

    #[test]
    fn profile_validation_rejects_wildcard_remote_and_non_loopback_x11() {
        let mut p = Profile::remote_defaults();
        p.name = "Test".into();
        p.ssh_host = "example.com".into();
        p.ssh_user = "user".into();
        p.remote_host = "0.0.0.0".into();
        assert!(p.validate().is_err());

        p.remote_host = "127.0.0.1".into();
        p.x11.enabled = true;
        p.x11.display = "192.168.1.10:0".into();
        assert!(p.validate().is_err());

        p.x11.display = "127.0.0.1:0.0".into();
        assert!(p.validate().is_ok());
    }

    #[test]
    fn file_store_crud_reorder_duplicate_and_export_strips_secret_flags() {
        let dir = tempdir().unwrap();
        let store = FileStore::new(dir.path().join("profiles.json"));

        let mut p1 = Profile::remote_defaults();
        p1.name = "Alpha".into();
        p1.ssh_host = "example.com".into();
        p1.ssh_user = "user".into();
        p1.has_password = true;
        let p1 = store.save(p1).unwrap();

        let mut p2 = Profile::local_defaults();
        p2.name = "Beta".into();
        let p2 = store.save(p2).unwrap();

        store.reorder(&[p2.id.clone(), p1.id.clone()]).unwrap();
        let list = store.list().unwrap();
        assert_eq!(list[0].id, p2.id);
        assert_eq!(list[1].id, p1.id);

        let dup = store.duplicate(&p1.id).unwrap();
        assert_ne!(dup.id, p1.id);
        assert!(!dup.has_password);

        let exported = store.export_json().unwrap();
        assert!(!exported.contains("\"has_password\": true"));

        store.delete(&dup.id).unwrap();
        assert_eq!(store.list().unwrap().len(), 2);
    }

    #[test]
    fn ui_settings_roundtrip_and_bounds() {
        let dir = tempdir().unwrap();
        let path = dir.path().join("settings.json");

        let mut s = UISettings::default();
        s.language = "zh".into();
        s.terminal_font_size = 15;
        s.terminal_scrollback = 10000;
        save_ui_settings_file(&path, &s).unwrap();

        let loaded = load_ui_settings_file(&path).unwrap();
        assert_eq!(loaded, s);

        let mut bad = s.clone();
        bad.terminal_font_size = 99;
        assert!(save_ui_settings_file(&path, &bad).is_err());
    }
}
