//! ChemSSH remote installer: release manifest handling, update planning and the
//! remote shell scripts that install or update ChemSSH in place.
//!
//! Layout on the remote machine (default `~/.chemssh`):
//!
//! ```text
//! <dir>/app/           code (backend, frontend/dist, plugins, ...) replaced in place on update
//! <dir>/.venv/         virtualenv, created once, never relocated
//! <dir>/config.yaml    user configuration, never overwritten
//! <dir>/install.json   installed version and dependency fingerprint
//! ```

pub mod manifest;
pub mod options;
pub mod script;

pub use manifest::{
    archive_urls, compare_versions, manifest_url, manifest_urls, plan, Action, ArchiveFile,
    InstallRecord, Manifest, ManifestError, RemoteProbe, GITHUB_REPO,
};
pub use options::{InstallDir, InstallerOptions, OptionsError};
pub use script::{
    build_install_script, build_probe_script, proxy_exports, shell_quote, ScriptParams,
    EXIT_DOWNLOAD_FAILED,
};
