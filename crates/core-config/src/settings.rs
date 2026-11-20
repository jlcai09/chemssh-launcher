use std::fs;
use std::path::Path;

use serde::{Deserialize, Serialize};

use crate::{paths, ConfigError};

pub const LANGUAGE_SYSTEM: &str = "system";
pub const LANGUAGE_CHINESE: &str = "zh";
pub const LANGUAGE_ENGLISH: &str = "en";

pub const DEFAULT_TERMINAL_FONT_SIZE: u32 = 13;
pub const MIN_TERMINAL_FONT_SIZE: u32 = 10;
pub const MAX_TERMINAL_FONT_SIZE: u32 = 24;
pub const DEFAULT_TERMINAL_SCROLLBACK: u32 = 5000;
pub const MIN_TERMINAL_SCROLLBACK: u32 = 1000;
pub const MAX_TERMINAL_SCROLLBACK: u32 = 50000;

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default)]
pub struct UISettings {
    pub version: u32,
    pub language: String,
    pub terminal_font_size: u32,
    pub terminal_scrollback: u32,
}

impl Default for UISettings {
    fn default() -> Self {
        Self {
            version: 1,
            language: LANGUAGE_SYSTEM.to_string(),
            terminal_font_size: DEFAULT_TERMINAL_FONT_SIZE,
            terminal_scrollback: DEFAULT_TERMINAL_SCROLLBACK,
        }
    }
}

pub fn normalize_ui_language(value: &str) -> Option<&'static str> {
    match value.trim().to_ascii_lowercase().as_str() {
        "" | LANGUAGE_SYSTEM => Some(LANGUAGE_SYSTEM),
        LANGUAGE_CHINESE => Some(LANGUAGE_CHINESE),
        LANGUAGE_ENGLISH => Some(LANGUAGE_ENGLISH),
        _ => None,
    }
}

pub fn load_ui_settings() -> Result<UISettings, ConfigError> {
    let path = paths::default_settings_path().map_err(ConfigError::Path)?;
    load_ui_settings_file(&path)
}

pub fn load_ui_settings_file(path: &Path) -> Result<UISettings, ConfigError> {
    let raw = match fs::read(path) {
        Ok(bytes) => bytes,
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => return Ok(UISettings::default()),
        Err(err) => return Err(ConfigError::Io(err)),
    };
    if raw.iter().all(|b| b.is_ascii_whitespace()) {
        return Ok(UISettings::default());
    }
    let parsed: UISettings = match serde_json::from_slice(&raw) {
        Ok(v) => v,
        Err(_) => return Ok(UISettings::default()),
    };
    Ok(normalize_loaded(parsed))
}

pub fn save_ui_settings(settings: &UISettings) -> Result<(), ConfigError> {
    let path = paths::default_settings_path().map_err(ConfigError::Path)?;
    save_ui_settings_file(&path, settings)
}

pub fn save_ui_settings_file(path: &Path, settings: &UISettings) -> Result<(), ConfigError> {
    let language = normalize_ui_language(&settings.language).ok_or_else(|| {
        ConfigError::InvalidSettings(format!("unsupported UI language {:?}", settings.language))
    })?;

    let mut normalized = settings.clone();
    normalized.version = 1;
    normalized.language = language.to_string();
    if normalized.terminal_font_size == 0 {
        normalized.terminal_font_size = DEFAULT_TERMINAL_FONT_SIZE;
    }
    if !(MIN_TERMINAL_FONT_SIZE..=MAX_TERMINAL_FONT_SIZE).contains(&normalized.terminal_font_size) {
        return Err(ConfigError::InvalidSettings(format!(
            "terminal font size must be between {MIN_TERMINAL_FONT_SIZE} and {MAX_TERMINAL_FONT_SIZE}"
        )));
    }
    if normalized.terminal_scrollback == 0 {
        normalized.terminal_scrollback = DEFAULT_TERMINAL_SCROLLBACK;
    }
    if !(MIN_TERMINAL_SCROLLBACK..=MAX_TERMINAL_SCROLLBACK)
        .contains(&normalized.terminal_scrollback)
    {
        return Err(ConfigError::InvalidSettings(format!(
            "terminal scrollback must be between {MIN_TERMINAL_SCROLLBACK} and {MAX_TERMINAL_SCROLLBACK}"
        )));
    }

    crate::store::write_json_atomic(path, &normalized)
}

fn normalize_loaded(mut settings: UISettings) -> UISettings {
    settings.version = 1;
    settings.language = normalize_ui_language(&settings.language)
        .unwrap_or(LANGUAGE_SYSTEM)
        .to_string();
    settings.terminal_font_size = clamp_or_default(
        settings.terminal_font_size,
        DEFAULT_TERMINAL_FONT_SIZE,
        MIN_TERMINAL_FONT_SIZE,
        MAX_TERMINAL_FONT_SIZE,
    );
    settings.terminal_scrollback = clamp_or_default(
        settings.terminal_scrollback,
        DEFAULT_TERMINAL_SCROLLBACK,
        MIN_TERMINAL_SCROLLBACK,
        MAX_TERMINAL_SCROLLBACK,
    );
    settings
}

fn clamp_or_default(value: u32, fallback: u32, min: u32, max: u32) -> u32 {
    if value == 0 {
        fallback
    } else {
        value.clamp(min, max)
    }
}
