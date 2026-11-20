use std::path::PathBuf;

pub const APP_NAME: &str = "ChemSSHLauncher";

pub fn default_dir() -> Result<PathBuf, String> {
    #[cfg(target_os = "windows")]
    {
        if let Ok(app_data) = std::env::var("AppData") {
            if !app_data.is_empty() {
                return Ok(PathBuf::from(app_data).join(APP_NAME));
            }
        }
    }

    let base = dirs::config_dir().ok_or_else(|| "could not determine user config directory".to_string())?;
    #[cfg(target_os = "linux")]
    {
        Ok(base.join("chemssh-launcher"))
    }
    #[cfg(not(target_os = "linux"))]
    {
        Ok(base.join(APP_NAME))
    }
}

pub fn default_profiles_path() -> Result<PathBuf, String> {
    Ok(default_dir()?.join("profiles.json"))
}

pub fn default_settings_path() -> Result<PathBuf, String> {
    Ok(default_dir()?.join("settings.json"))
}

pub fn default_vault_path() -> Result<PathBuf, String> {
    Ok(default_dir()?.join("vault.json"))
}

pub fn default_known_hosts_path() -> Result<PathBuf, String> {
    Ok(default_dir()?.join("known_hosts"))
}

pub fn default_client_identity_path() -> Result<PathBuf, String> {
    Ok(default_dir()?.join("client_identity.json"))
}

pub fn default_sftp_open_cache_dir() -> Result<PathBuf, String> {
    if let Some(cache) = dirs::cache_dir() {
        #[cfg(target_os = "linux")]
        {
            return Ok(cache.join("chemssh-launcher").join("sftp-open"));
        }
        #[cfg(not(target_os = "linux"))]
        {
            return Ok(cache.join(APP_NAME).join("sftp-open"));
        }
    }
    Ok(default_dir()?.join("sftp-open-cache"))
}
