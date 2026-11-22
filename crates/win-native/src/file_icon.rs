use std::collections::{HashMap, VecDeque};
use std::sync::{Arc, Mutex};

use crate::file_icon_fallback::render_fallback_icon_png;
#[cfg(target_os = "windows")]
use crate::file_icon_windows::extract_windows_icon_png;

const MAX_ICON_CACHE_ENTRIES: usize = 500;

#[derive(Debug, Clone)]
pub struct FileIconData {
    pub content_type: &'static str,
    pub bytes: Arc<[u8]>,
}

pub struct FileIconService {
    cache: Mutex<IconLruCache>,
}

struct IconLruCache {
    map: HashMap<String, Arc<[u8]>>,
    order: VecDeque<String>,
}

impl IconLruCache {
    fn new() -> Self {
        Self {
            map: HashMap::with_capacity(MAX_ICON_CACHE_ENTRIES),
            order: VecDeque::with_capacity(MAX_ICON_CACHE_ENTRIES),
        }
    }

    fn get(&mut self, key: &str) -> Option<Arc<[u8]>> {
        let value = self.map.get(key).cloned()?;
        self.touch(key);
        Some(value)
    }

    fn insert(&mut self, key: String, value: Arc<[u8]>) {
        if self.map.contains_key(&key) {
            self.map.insert(key.clone(), value);
            self.touch(&key);
            return;
        }
        while self.order.len() >= MAX_ICON_CACHE_ENTRIES {
            if let Some(evicted) = self.order.pop_front() {
                self.map.remove(&evicted);
            }
        }
        self.order.push_back(key.clone());
        self.map.insert(key, value);
    }

    fn touch(&mut self, key: &str) {
        if let Some(pos) = self.order.iter().position(|k| k == key) {
            if let Some(item) = self.order.remove(pos) {
                self.order.push_back(item);
            }
        }
    }
}

impl Default for FileIconService {
    fn default() -> Self {
        Self::new()
    }
}

impl FileIconService {
    pub fn new() -> Self {
        Self {
            cache: Mutex::new(IconLruCache::new()),
        }
    }

    pub fn get_icon(&self, name: &str, is_dir: bool, size: u32) -> FileIconData {
        let norm_size = normalize_size(size);
        let ext = extract_extension(name, is_dir);
        let key = cache_key_from_ext(is_dir, &ext, norm_size);

        if let Ok(mut guard) = self.cache.lock() {
            if let Some(cached) = guard.get(&key) {
                return FileIconData {
                    content_type: "image/png",
                    bytes: cached,
                };
            }
        }

        let lookup = lookup_name_from_ext(is_dir, &ext);
        let png_bytes = render_platform_or_fallback_icon(&lookup, is_dir, &ext, norm_size);
        let arc_bytes: Arc<[u8]> = Arc::from(png_bytes);

        if let Ok(mut guard) = self.cache.lock() {
            guard.insert(key, Arc::clone(&arc_bytes));
        }

        FileIconData {
            content_type: "image/png",
            bytes: arc_bytes,
        }
    }
}

pub fn normalize_size(size: u32) -> u32 {
    if size <= 16 { 16 } else { 32 }
}

pub fn extract_extension(name: &str, is_dir: bool) -> String {
    if is_dir {
        return String::new();
    }
    let trimmed = name.trim().trim_end_matches(['/', '\\']);
    let base = trimmed
        .rsplit(['/', '\\'])
        .next()
        .unwrap_or("")
        .to_ascii_lowercase();
    if base.is_empty() || base == "." || base == ".." {
        return String::new();
    }
    if base.starts_with('.') {
        if base.len() == 1 || base[1..].ends_with('.') {
            return String::new();
        }
        return match base.rfind('.') {
            Some(0) => base,
            Some(idx) if idx + 1 < base.len() => base[idx..].to_string(),
            _ => String::new(),
        };
    }
    match base.rfind('.') {
        Some(idx) if idx + 1 < base.len() => base[idx..].to_string(),
        _ => String::new(),
    }
}

pub fn cache_key(name: &str, is_dir: bool, size: u32) -> String {
    let norm_size = normalize_size(size);
    let ext = extract_extension(name, is_dir);
    cache_key_from_ext(is_dir, &ext, norm_size)
}

pub fn lookup_name(name: &str, is_dir: bool) -> String {
    let ext = extract_extension(name, is_dir);
    lookup_name_from_ext(is_dir, &ext)
}

fn cache_key_from_ext(is_dir: bool, ext: &str, norm_size: u32) -> String {
    if is_dir {
        format!("dir:{norm_size}")
    } else if ext.is_empty() {
        format!("file:{norm_size}")
    } else {
        format!("{ext}:{norm_size}")
    }
}

fn lookup_name_from_ext(is_dir: bool, ext: &str) -> String {
    if is_dir {
        "folder".to_string()
    } else if ext.is_empty() {
        "file".to_string()
    } else {
        format!("file{ext}")
    }
}

fn render_platform_or_fallback_icon(
    lookup: &str,
    is_dir: bool,
    ext: &str,
    norm_size: u32,
) -> Vec<u8> {
    #[cfg(target_os = "windows")]
    {
        if let Ok(bytes) = extract_windows_icon_png(lookup, is_dir, norm_size) {
            if !bytes.is_empty() {
                return bytes;
            }
        }
    }
    let _ = lookup;
    render_fallback_icon_png(is_dir, ext, norm_size)
}

pub fn encode_rgba_png(width: u32, height: u32, rgba: &[u8]) -> Result<Vec<u8>, String> {
    let mut out = Vec::new();
    {
        let mut encoder = png::Encoder::new(&mut out, width, height);
        encoder.set_color(png::ColorType::Rgba);
        encoder.set_depth(png::BitDepth::Eight);
        let mut writer = encoder
            .write_header()
            .map_err(|e| format!("png header encode error: {e}"))?;
        writer
            .write_image_data(rgba)
            .map_err(|e| format!("png pixel encode error: {e}"))?;
    }
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn normalize_size_clamps_to_16_or_32() {
        assert_eq!(normalize_size(0), 16);
        assert_eq!(normalize_size(16), 16);
        assert_eq!(normalize_size(24), 32);
        assert_eq!(normalize_size(64), 32);
    }

    #[test]
    fn cache_key_and_lookup_match_go_rules() {
        assert_eq!(cache_key("src", true, 16), "dir:16");
        assert_eq!(lookup_name("src", true), "folder");

        assert_eq!(cache_key("README", false, 16), "file:16");
        assert_eq!(lookup_name("README", false), "file");

        assert_eq!(cache_key(".gitignore", false, 16), ".gitignore:16");
        assert_eq!(lookup_name(".gitignore", false), "file.gitignore");

        assert_eq!(cache_key("archive.TAR.GZ", false, 32), ".gz:32");
        assert_eq!(lookup_name("archive.TAR.GZ", false), "file.gz");
    }

    #[test]
    fn icon_service_emits_valid_png_and_reuses_cache() {
        let svc = FileIconService::new();
        let a = svc.get_icon("a.py", false, 16);
        let b = svc.get_icon("b.py", false, 16);
        assert_eq!(a.content_type, "image/png");
        assert!(a.bytes.starts_with(b"\x89PNG\r\n\x1a\n"));
        assert!(Arc::ptr_eq(&a.bytes, &b.bytes));

        let dir_icon = svc.get_icon("folder", true, 32);
        assert!(dir_icon.bytes.starts_with(b"\x89PNG\r\n\x1a\n"));
        assert_ne!(a.bytes.as_ref(), dir_icon.bytes.as_ref());
    }
}
