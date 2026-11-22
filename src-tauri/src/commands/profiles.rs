use core_config::{Profile, ProfileStore};
use core_secret::{KEY_PASSWORD, KEY_PRIVATE_PASSPHRASE, KEY_SECURITY_TOKEN};
use core_session::{is_port_available, suggest_free_ports};
use serde::{Deserialize, Serialize};
use tauri::State;

use crate::state::AppState;

#[derive(Debug, Clone, Deserialize, Default)]
#[serde(default)]
pub struct SaveProfileRequest {
    pub profile: Profile,
    pub password: Option<String>,
    pub clear_password: bool,
    pub private_key_passphrase: Option<String>,
    pub clear_private_key_passphrase: bool,
    pub security_token: Option<String>,
    pub clear_security_token: bool,
}

#[derive(Debug, Clone, Serialize)]
pub struct SaveProfileResponse {
    pub profile: Profile,
    pub warnings: Vec<String>,
}

#[derive(Debug, Clone, Serialize)]
pub struct ProfileDefaultsResponse {
    pub remote: Profile,
    pub local: Profile,
}

#[derive(Debug, Clone, Serialize)]
pub struct PortCheckResponse {
    pub host: String,
    pub port: u16,
    pub available: bool,
    pub suggested_ports: Vec<u16>,
}

pub(crate) fn save_profile_impl(
    state: &AppState,
    req: SaveProfileRequest,
) -> Result<SaveProfileResponse, String> {
    let mut profile = req.profile;
    profile.local_proxy = profile.local_proxy.normalized();
    profile.x11 = profile.x11.normalized();

    // Preserve existing secret flags when updating an existing profile.
    if !profile.id.is_empty() {
        if let Ok(existing) = state.profiles.get(&profile.id) {
            profile.has_password = existing.has_password;
            profile.has_private_key_passphrase = existing.has_private_key_passphrase;
            profile.has_security_token = existing.has_security_token;
        }
    }

    profile.validate().map_err(|e| e.to_string())?;
    let warnings = profile.warnings();

    // Persist once so a newly created profile receives a stable ID before storing secrets.
    let mut saved = state.profiles.save(profile).map_err(|e| e.to_string())?;
    let id = saved.id.clone();

    let mut flags_changed = false;

    if req.clear_password {
        state
            .secrets
            .delete(&id, KEY_PASSWORD)
            .map_err(|e| e.to_string())?;
        saved.has_password = false;
        flags_changed = true;
    } else if let Some(pw) = req.password.as_deref().filter(|s| !s.is_empty()) {
        state
            .secrets
            .set(&id, KEY_PASSWORD, pw)
            .map_err(|e| e.to_string())?;
        saved.has_password = true;
        flags_changed = true;
    }

    if req.clear_private_key_passphrase {
        state
            .secrets
            .delete(&id, KEY_PRIVATE_PASSPHRASE)
            .map_err(|e| e.to_string())?;
        saved.has_private_key_passphrase = false;
        flags_changed = true;
    } else if let Some(pp) = req
        .private_key_passphrase
        .as_deref()
        .filter(|s| !s.is_empty())
    {
        state
            .secrets
            .set(&id, KEY_PRIVATE_PASSPHRASE, pp)
            .map_err(|e| e.to_string())?;
        saved.has_private_key_passphrase = true;
        flags_changed = true;
    }

    if req.clear_security_token {
        state
            .secrets
            .delete(&id, KEY_SECURITY_TOKEN)
            .map_err(|e| e.to_string())?;
        saved.has_security_token = false;
        flags_changed = true;
    } else if let Some(tok) = req.security_token.as_deref().filter(|s| !s.is_empty()) {
        state
            .secrets
            .set(&id, KEY_SECURITY_TOKEN, tok)
            .map_err(|e| e.to_string())?;
        saved.has_security_token = true;
        flags_changed = true;
    }

    if flags_changed {
        saved = state.profiles.save(saved).map_err(|e| e.to_string())?;
    }

    Ok(SaveProfileResponse {
        profile: saved,
        warnings,
    })
}

pub(crate) fn delete_profile_impl(state: &AppState, id_or_name: &str) -> Result<(), String> {
    let profile = state
        .profiles
        .get(id_or_name)
        .map_err(|e| e.to_string())?;
    state
        .profiles
        .delete(&profile.id)
        .map_err(|e| e.to_string())?;
    state
        .secrets
        .delete_profile_secrets(&profile.id)
        .map_err(|e| e.to_string())
}

#[tauri::command]
pub fn list_profiles(state: State<'_, AppState>) -> Result<Vec<Profile>, String> {
    state.profiles.list().map_err(|e| e.to_string())
}

#[tauri::command]
pub fn get_profile(state: State<'_, AppState>, id_or_name: String) -> Result<Profile, String> {
    state.profiles.get(&id_or_name).map_err(|e| e.to_string())
}

#[tauri::command]
pub fn save_profile(
    state: State<'_, AppState>,
    request: SaveProfileRequest,
) -> Result<SaveProfileResponse, String> {
    save_profile_impl(&state, request)
}

#[tauri::command]
pub fn delete_profile(state: State<'_, AppState>, id_or_name: String) -> Result<(), String> {
    delete_profile_impl(&state, &id_or_name)
}

#[tauri::command]
pub fn reorder_profiles(state: State<'_, AppState>, ids: Vec<String>) -> Result<(), String> {
    state.profiles.reorder(&ids).map_err(|e| e.to_string())
}

#[tauri::command]
pub fn duplicate_profile(
    state: State<'_, AppState>,
    id_or_name: String,
    new_name: Option<String>,
) -> Result<Profile, String> {
    let source = state.profiles.get(&id_or_name).map_err(|e| e.to_string())?;
    let mut dup = state
        .profiles
        .duplicate(&id_or_name)
        .map_err(|e| e.to_string())?;
    if let Some(name) = new_name.filter(|s| !s.trim().is_empty()) {
        dup.name = name;
        dup = state.profiles.save(dup).map_err(|e| e.to_string())?;
    }
    for key in [KEY_PASSWORD, KEY_PRIVATE_PASSPHRASE, KEY_SECURITY_TOKEN] {
        if let Ok(Some(val)) = state.secrets.get(&source.id, key) {
            let _ = state.secrets.set(&dup.id, key, &val);
        }
    }
    Ok(dup)
}

#[tauri::command]
pub fn profile_defaults() -> ProfileDefaultsResponse {
    ProfileDefaultsResponse {
        remote: Profile::remote_defaults(),
        local: Profile::local_defaults(),
    }
}

#[tauri::command]
pub fn check_local_port(host: String, port: u16, suggest_count: Option<usize>) -> PortCheckResponse {
    let available = is_port_available(&host, port);
    let suggested_ports = if available {
        Vec::new()
    } else {
        suggest_free_ports(&host, port, suggest_count.unwrap_or(5))
    };
    PortCheckResponse {
        host,
        port,
        available,
        suggested_ports,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Arc;
    use core_secret::MemoryStore;
    use tempfile::tempdir;

    fn test_state() -> (tempfile::TempDir, AppState) {
        let dir = tempdir().unwrap();
        let state = AppState::new_custom(
            dir.path().to_path_buf(),
            dir.path().join("profiles.json"),
            dir.path().join("settings.json"),
            dir.path().join("sftp-cache"),
            Arc::new(MemoryStore::new()),
        );
        (dir, state)
    }

    #[test]
    fn saved_secret_is_never_written_to_profile_json_and_cleaned_on_delete() {
        let (_dir, state) = test_state();
        let mut p = Profile::remote_defaults();
        p.name = "Example Server".into();
        p.ssh_host = "example.com".into();
        p.ssh_user = "user".into();

        let resp = save_profile_impl(
            &state,
            SaveProfileRequest {
                profile: p,
                password: Some("SUPER_SECRET_DUMMY".into()),
                ..Default::default()
            },
        )
        .unwrap();

        assert!(resp.profile.has_password);
        let id = resp.profile.id.clone();

        // Profile JSON on disk must not contain plaintext password.
        let disk_json = std::fs::read_to_string(&state.profiles.path).unwrap();
        assert!(!disk_json.contains("SUPER_SECRET_DUMMY"));

        // SecretStore holds the password under the profile ID.
        assert_eq!(
            state.secrets.get(&id, KEY_PASSWORD).unwrap().as_deref(),
            Some("SUPER_SECRET_DUMMY")
        );

        // Editing profile without providing a new password preserves `has_password`.
        let mut updated = resp.profile.clone();
        updated.name = "Example Server Renamed".into();
        let resp2 = save_profile_impl(
            &state,
            SaveProfileRequest {
                profile: updated,
                ..Default::default()
            },
        )
        .unwrap();
        assert!(resp2.profile.has_password);

        // Clearing password removes it from SecretStore and clears flag.
        let resp3 = save_profile_impl(
            &state,
            SaveProfileRequest {
                profile: resp2.profile,
                clear_password: true,
                ..Default::default()
            },
        )
        .unwrap();
        assert!(!resp3.profile.has_password);
        assert_eq!(state.secrets.get(&id, KEY_PASSWORD).unwrap(), None);

        // Set again then delete profile -> secret must be removed too.
        save_profile_impl(
            &state,
            SaveProfileRequest {
                profile: resp3.profile,
                security_token: Some("TOKEN_DUMMY".into()),
                ..Default::default()
            },
        )
        .unwrap();
        delete_profile_impl(&state, &id).unwrap();
        assert_eq!(state.secrets.get(&id, KEY_SECURITY_TOKEN).unwrap(), None);
    }
}
