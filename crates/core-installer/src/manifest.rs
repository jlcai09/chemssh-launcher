use serde::{Deserialize, Serialize};
use thiserror::Error;

/// The only repository releases are fetched from.
pub const GITHUB_REPO: &str = "jlcai09/chemssh";

#[derive(Debug, Error, PartialEq, Eq)]
pub enum ManifestError {
    #[error("invalid release manifest: {0}")]
    Invalid(String),
}

#[derive(Debug, Clone, Deserialize, Serialize, PartialEq, Eq)]
pub struct ArchiveFile {
    pub name: String,
    pub sha256: String,
}

/// `latest.json` published next to each GitHub release.
#[derive(Debug, Clone, Deserialize, Serialize, PartialEq, Eq)]
pub struct Manifest {
    pub version: String,
    pub tag: String,
    /// Fingerprint of everything that decides whether the virtualenv needs a rebuild
    /// (dependencies, python requirement, entry points, constraints).
    #[serde(default)]
    pub deps_fingerprint: String,
    pub files: Vec<ArchiveFile>,
}

impl Manifest {
    pub fn parse(text: &str) -> Result<Self, ManifestError> {
        let manifest: Manifest = serde_json::from_str(text.trim())
            .map_err(|e| ManifestError::Invalid(e.to_string()))?;
        manifest.validate()?;
        Ok(manifest)
    }

    pub fn validate(&self) -> Result<(), ManifestError> {
        if !is_safe_token(&self.version) {
            return Err(ManifestError::Invalid("version contains unsupported characters".into()));
        }
        if !is_safe_token(&self.tag) {
            return Err(ManifestError::Invalid("tag contains unsupported characters".into()));
        }
        if !self.deps_fingerprint.is_empty() && !is_hex64(&self.deps_fingerprint) {
            return Err(ManifestError::Invalid("deps_fingerprint must be 64 hex characters".into()));
        }
        for file in &self.files {
            if !is_safe_token(&file.name) || file.name.contains('/') {
                return Err(ManifestError::Invalid(format!("unsafe file name: {}", file.name)));
            }
            if !is_hex64(&file.sha256) {
                return Err(ManifestError::Invalid(format!("bad sha256 for {}", file.name)));
            }
        }
        self.archive().map(|_| ())
    }

    /// The `.tar.gz` release archive.
    pub fn archive(&self) -> Result<&ArchiveFile, ManifestError> {
        self.files
            .iter()
            .find(|f| f.name.ends_with(".tar.gz"))
            .ok_or_else(|| ManifestError::Invalid("no .tar.gz archive listed".into()))
    }
}

/// `install.json` written on the remote machine after each successful install or update.
#[derive(Debug, Clone, Default, Deserialize, Serialize, PartialEq, Eq)]
pub struct InstallRecord {
    #[serde(default)]
    pub version: String,
    #[serde(default)]
    pub tag: String,
    #[serde(default)]
    pub deps_fingerprint: String,
    #[serde(default)]
    pub installed_at: String,
}

/// What a probe of the remote install directory found.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct RemoteProbe {
    pub home: String,
    pub venv_ok: bool,
    pub record: Option<InstallRecord>,
}

impl RemoteProbe {
    pub fn parse(output: &str) -> Self {
        let mut probe = RemoteProbe::default();
        let mut in_record = false;
        let mut record_text = String::new();
        for line in output.lines() {
            let line = line.trim_end_matches('\r');
            if in_record {
                if line == "__END__" {
                    in_record = false;
                } else {
                    record_text.push_str(line);
                    record_text.push('\n');
                }
                continue;
            }
            if line == "__RECORD__" {
                in_record = true;
            } else if let Some(v) = line.strip_prefix("HOME_DIR=") {
                probe.home = v.trim().to_string();
            } else if let Some(v) = line.strip_prefix("VENV_OK=") {
                probe.venv_ok = v.trim() == "1";
            }
        }
        if !record_text.trim().is_empty() {
            probe.record = serde_json::from_str(record_text.trim()).ok();
        }
        probe
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Action {
    /// Nothing installed yet.
    Install,
    /// Rebuild the virtualenv from scratch and reinstall everything.
    Repair,
    /// Dependencies unchanged: replace frontend/backend files only.
    UpdateFast,
    /// Dependencies changed (or unknown): replace files and reinstall dependencies.
    UpdateFull,
    UpToDate,
}

impl Action {
    pub fn script_name(self) -> &'static str {
        match self {
            Action::Install => "install",
            Action::Repair => "repair",
            Action::UpdateFast => "update_fast",
            Action::UpdateFull => "update_full",
            Action::UpToDate => "up_to_date",
        }
    }
}

/// Decides what to do. `force_repair` always rebuilds the virtualenv.
pub fn plan(
    record: Option<&InstallRecord>,
    venv_ok: bool,
    manifest: &Manifest,
    force_repair: bool,
) -> Action {
    let Some(record) = record else {
        return Action::Install;
    };
    if force_repair || !venv_ok {
        return Action::Repair;
    }
    if compare_versions(&manifest.version, &record.version) != std::cmp::Ordering::Greater {
        return Action::UpToDate;
    }
    let same_deps = !manifest.deps_fingerprint.is_empty()
        && manifest.deps_fingerprint.eq_ignore_ascii_case(&record.deps_fingerprint);
    if same_deps {
        Action::UpdateFast
    } else {
        Action::UpdateFull
    }
}

/// Numeric dotted comparison; a leading `v` and any `-`/`+` suffix are ignored.
pub fn compare_versions(a: &str, b: &str) -> std::cmp::Ordering {
    fn parts(v: &str) -> Vec<u64> {
        let core = v.trim().trim_start_matches(['v', 'V']);
        let core = core.split(['-', '+']).next().unwrap_or("");
        core.split('.').map(|p| p.parse::<u64>().unwrap_or(0)).collect()
    }
    let (mut pa, mut pb) = (parts(a), parts(b));
    let len = pa.len().max(pb.len());
    pa.resize(len, 0);
    pb.resize(len, 0);
    pa.cmp(&pb)
}

pub fn manifest_url() -> String {
    format!("https://github.com/{GITHUB_REPO}/releases/latest/download/latest.json")
}

/// Candidate URLs for `latest.json`: official GitHub URL first, then the optional proxy-prefixed one.
pub fn manifest_urls(proxy_prefix: &str) -> Vec<String> {
    let official = manifest_url();
    let mut urls = vec![official.clone()];
    let prefix = proxy_prefix.trim().trim_end_matches('/');
    if !prefix.is_empty() {
        urls.push(format!("{prefix}/{official}"));
    }
    urls
}

/// Download candidates for an archive: official URL first, then the optional proxy-prefixed one.
/// The URL is always built from the tag and file name, never taken from the manifest.
pub fn archive_urls(tag: &str, name: &str, proxy_prefix: &str) -> Vec<String> {
    let official = format!("https://github.com/{GITHUB_REPO}/releases/download/{tag}/{name}");
    let mut urls = vec![official.clone()];
    let prefix = proxy_prefix.trim().trim_end_matches('/');
    if !prefix.is_empty() {
        urls.push(format!("{prefix}/{official}"));
    }
    urls
}

pub(crate) fn is_safe_token(s: &str) -> bool {
    !s.is_empty()
        && s.len() <= 128
        && s.chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '-' | '+'))
        && !s.starts_with('.')
}

fn is_hex64(s: &str) -> bool {
    s.len() == 64 && s.chars().all(|c| c.is_ascii_hexdigit())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::cmp::Ordering;

    const FP_A: &str = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
    const FP_B: &str = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";

    fn manifest(version: &str, fp: &str) -> Manifest {
        Manifest {
            version: version.into(),
            tag: format!("v{version}"),
            deps_fingerprint: fp.into(),
            files: vec![ArchiveFile {
                name: format!("chemssh-{version}.tar.gz"),
                sha256: FP_A.into(),
            }],
        }
    }

    fn record(version: &str, fp: &str) -> InstallRecord {
        InstallRecord {
            version: version.into(),
            deps_fingerprint: fp.into(),
            ..Default::default()
        }
    }

    #[test]
    fn parses_valid_manifest() {
        let text = format!(
            r#"{{"version":"0.3.11","tag":"v0.3.11","deps_fingerprint":"{FP_A}",
            "files":[{{"name":"chemssh-0.3.11.tar.gz","sha256":"{FP_B}"}},
                     {{"name":"chemssh-0.3.11.zip","sha256":"{FP_B}"}}]}}"#
        );
        let m = Manifest::parse(&text).unwrap();
        assert_eq!(m.archive().unwrap().name, "chemssh-0.3.11.tar.gz");
    }

    #[test]
    fn rejects_unsafe_or_incomplete_manifest() {
        assert!(Manifest::parse("not json").is_err());
        let traversal = format!(
            r#"{{"version":"1","tag":"v1","files":[{{"name":"../x.tar.gz","sha256":"{FP_A}"}}]}}"#
        );
        assert!(Manifest::parse(&traversal).is_err());
        let bad_sha = r#"{"version":"1","tag":"v1","files":[{"name":"a.tar.gz","sha256":"zz"}]}"#;
        assert!(Manifest::parse(bad_sha).is_err());
        let no_archive = format!(
            r#"{{"version":"1","tag":"v1","files":[{{"name":"a.zip","sha256":"{FP_A}"}}]}}"#
        );
        assert!(Manifest::parse(&no_archive).is_err());
        let shell = format!(
            r#"{{"version":"1;rm","tag":"v1","files":[{{"name":"a.tar.gz","sha256":"{FP_A}"}}]}}"#
        );
        assert!(Manifest::parse(&shell).is_err());
    }

    #[test]
    fn version_comparison() {
        assert_eq!(compare_versions("0.3.11", "0.3.10"), Ordering::Greater);
        assert_eq!(compare_versions("v0.4", "0.4.0"), Ordering::Equal);
        assert_eq!(compare_versions("0.3.9", "0.3.10"), Ordering::Less);
        assert_eq!(compare_versions("1.0.0-rc1", "1.0.0"), Ordering::Equal);
    }

    #[test]
    fn plan_covers_all_actions() {
        let m = manifest("0.3.11", FP_A);
        assert_eq!(plan(None, false, &m, false), Action::Install);
        assert_eq!(plan(Some(&record("0.3.10", FP_A)), true, &m, false), Action::UpdateFast);
        assert_eq!(plan(Some(&record("0.3.10", FP_B)), true, &m, false), Action::UpdateFull);
        assert_eq!(plan(Some(&record("0.3.10", "")), true, &m, false), Action::UpdateFull);
        assert_eq!(plan(Some(&record("0.3.11", FP_A)), true, &m, false), Action::UpToDate);
        assert_eq!(plan(Some(&record("0.3.12", FP_A)), true, &m, false), Action::UpToDate);
        assert_eq!(plan(Some(&record("0.3.10", FP_A)), false, &m, false), Action::Repair);
        assert_eq!(plan(Some(&record("0.3.11", FP_A)), true, &m, true), Action::Repair);
    }

    #[test]
    fn plan_without_manifest_fingerprint_is_full_update() {
        let m = manifest("0.3.11", "");
        assert_eq!(plan(Some(&record("0.3.10", "")), true, &m, false), Action::UpdateFull);
    }

    #[test]
    fn archive_urls_are_built_from_tag_and_name() {
        let urls = archive_urls("v1.0.0", "chemssh-1.0.0.tar.gz", "");
        assert_eq!(
            urls,
            vec!["https://github.com/jlcai09/chemssh/releases/download/v1.0.0/chemssh-1.0.0.tar.gz"]
        );
        let urls = archive_urls("v1.0.0", "chemssh-1.0.0.tar.gz", "https://proxy.example.com/");
        assert_eq!(urls.len(), 2);
        assert!(urls[1].starts_with("https://proxy.example.com/https://github.com/"));
    }

    #[test]
    fn manifest_urls_include_optional_proxy_prefix() {
        let urls = manifest_urls("");
        assert_eq!(
            urls,
            vec!["https://github.com/jlcai09/chemssh/releases/latest/download/latest.json"]
        );
        let urls = manifest_urls("https://proxy.example.com/");
        assert_eq!(urls.len(), 2);
        assert_eq!(
            urls[1],
            "https://proxy.example.com/https://github.com/jlcai09/chemssh/releases/latest/download/latest.json"
        );
    }

    #[test]
    fn parses_probe_output() {
        let out = "HOME_DIR=/home/user\nVENV_OK=1\n__RECORD__\n{\"version\":\"0.3.10\",\"deps_fingerprint\":\"x\"}\n__END__\n";
        let p = RemoteProbe::parse(out);
        assert_eq!(p.home, "/home/user");
        assert!(p.venv_ok);
        assert_eq!(p.record.unwrap().version, "0.3.10");
        let empty = RemoteProbe::parse("HOME_DIR=/h\nVENV_OK=0\n__RECORD__\n\n__END__\n");
        assert!(empty.record.is_none() && !empty.venv_ok);
    }
}
