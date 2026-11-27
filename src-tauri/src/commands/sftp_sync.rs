use std::collections::{HashMap, VecDeque};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant, SystemTime};

use chrono::{SecondsFormat, Utc};
use core_ssh::shell_quote;
use core_transfer::{
    remote_io_err, sftp_err, sftp_shell_sees_marker, RemoteStagedFile, TransferError,
};
use russh_sftp::client::SftpSession;
use russh_sftp::protocol::{FileAttributes, OpenFlags};
use serde::Serialize;
use sha2::{Digest, Sha256};
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};
use tokio::sync::Mutex;

use crate::commands::sftp::{SftpManager, SftpTransport};
use crate::state::LauncherLogStore;

const POLL_INTERVAL: Duration = Duration::from_millis(500);
const DEBOUNCE_DURATION: Duration = Duration::from_millis(500);
const ERROR_RETRY_BACKOFF: Duration = Duration::from_secs(3);
pub const MAX_SYNC_FILE_BYTES: u64 = 100 * 1024 * 1024;
const REMOTE_HASH_SHELL_THRESHOLD_BYTES: u64 = 256 * 1024;
const MAX_SYNC_EVENTS: usize = 500;
const CHUNK_SIZE: usize = 256 * 1024;

type Fingerprint = [u8; 32];

fn is_regular_file(attrs: &FileAttributes) -> bool {
    attrs
        .permissions
        .is_some_and(|mode| mode & 0o170000 == 0o100000)
}

#[derive(Clone, Copy)]
enum RemoteHashTool {
    Sha256sum,
    Shasum,
    OpenSsl,
}

impl RemoteHashTool {
    // Hash stdin: no filename interpolation by the tool and no filename in its output.
    fn command(self) -> &'static str {
        match self {
            Self::Sha256sum => "sha256sum",
            Self::Shasum => "shasum -a 256",
            Self::OpenSsl => "openssl dgst -sha256",
        }
    }
}

#[derive(Default)]
pub(crate) struct RemoteHashCapabilities {
    tool: Option<Option<RemoteHashTool>>,
    // A shell/SFTP match in the user's home does not prove one in a mounted
    // workspace. Prove the exact canonical parent once per SFTP connection.
    verified_parents: HashMap<String, bool>,
}

fn parse_remote_sha256(output: &str) -> Option<Fingerprint> {
    let mut tokens = output
        .split_ascii_whitespace()
        .filter(|token| token.len() == 64 && token.bytes().all(|byte| byte.is_ascii_hexdigit()));
    let hex = tokens.next()?;
    if tokens.next().is_some() || output.trim().lines().count() != 1 {
        return None;
    }
    let mut digest = [0; 32];
    for (i, value) in digest.iter_mut().enumerate() {
        *value = u8::from_str_radix(&hex[i * 2..i * 2 + 2], 16).ok()?;
    }
    Some(digest)
}

async fn probe_remote_hash_tool(transport: &SftpTransport) -> Option<RemoteHashTool> {
    let empty: Fingerprint = Sha256::digest([]).into();
    for tool in [
        RemoteHashTool::Sha256sum,
        RemoteHashTool::Shasum,
        RemoteHashTool::OpenSsl,
    ] {
        let command = format!("{} < /dev/null", tool.command());
        if transport
            .connection
            .exec_command_timeout(&command, Duration::from_secs(2))
            .await
            .is_ok_and(|out| {
                out.exit_code == Some(0) && parse_remote_sha256(&out.stdout) == Some(empty)
            })
        {
            return Some(tool);
        }
    }
    None
}

async fn probe_remote_hash_parent(transport: &SftpTransport, parent: &str) -> bool {
    let nonce = core_config::new_id();
    let path = format!(
        "{}/.chemssh-hash-{nonce}.part",
        parent.trim_end_matches('/')
    );
    let mut attrs = FileAttributes::empty();
    attrs.permissions = Some(0o600);
    let opened = transport
        .sftp
        .open_with_flags_and_attributes(
            &path,
            OpenFlags::WRITE | OpenFlags::CREATE | OpenFlags::EXCLUDE,
            attrs,
        )
        .await;
    let Ok(mut file) = opened else { return false };
    let wrote = file.write_all(nonce.as_bytes()).await.is_ok() && file.shutdown().await.is_ok();
    drop(file);
    let matches = wrote && sftp_shell_sees_marker(&transport.connection, &path, &nonce).await;
    let _ = transport.sftp.remove_file(path).await;
    matches
}

async fn remote_hash_tool(transport: &SftpTransport, path: &str) -> Option<RemoteHashTool> {
    let (parent, _) = path.rsplit_once('/')?;
    if !path.starts_with('/') || path.contains('\0') {
        return None;
    }
    let parent = if parent.is_empty() { "/" } else { parent };
    let mut capabilities = transport.sync_hash.lock().await;
    if capabilities.tool.is_none() {
        capabilities.tool = Some(probe_remote_hash_tool(transport).await);
    }
    let tool = capabilities.tool.flatten()?;
    let supported = if let Some(&supported) = capabilities.verified_parents.get(parent) {
        supported
    } else {
        let supported = probe_remote_hash_parent(transport, parent).await;
        if capabilities.verified_parents.len() >= 128 {
            capabilities.verified_parents.clear();
        }
        capabilities
            .verified_parents
            .insert(parent.to_owned(), supported);
        supported
    };
    supported.then_some(tool)
}

#[derive(Debug, Clone, Serialize)]
pub struct OpenSyncEvent {
    pub seq: u64,
    pub time: String,
    pub session: String,
    pub remote_path: String,
    pub local_path: String,
    pub status: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
struct LocalStamp {
    mtime: SystemTime,
    len: u64,
    #[cfg(unix)]
    identity: (u64, u64, i64, i64),
}

impl LocalStamp {
    fn from_metadata(meta: std::fs::Metadata) -> std::io::Result<Self> {
        if !meta.is_file() {
            return Err(std::io::Error::other("cache path is not a regular file"));
        }
        #[cfg(unix)]
        use std::os::unix::fs::MetadataExt;
        Ok(Self {
            mtime: meta.modified()?,
            len: meta.len(),
            #[cfg(unix)]
            identity: (meta.dev(), meta.ino(), meta.ctime(), meta.ctime_nsec()),
        })
    }

    async fn read(path: &Path) -> std::io::Result<Self> {
        Self::from_metadata(tokio::fs::symlink_metadata(path).await?)
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
struct Endpoint(String, u16, String);

impl Endpoint {
    fn of(t: &SftpTransport) -> Self {
        Self(t.ssh_host.clone(), t.ssh_port, t.ssh_user.clone())
    }
}

#[derive(Clone)]
struct EditCopy {
    session_id: String,
    remote_path: String,
    canonical_path: String,
    local_path: PathBuf,
    endpoint: Endpoint,
    active: Arc<AtomicBool>,
}

struct Watch {
    copy: EditCopy,
    baseline: Fingerprint,
    synced_stamp: LocalStamp,
    seen_stamp: Option<LocalStamp>,
    seen_since: Instant,
    retry_at: Option<Instant>,
    ignored_stamp: Option<LocalStamp>,
    // A conflict or uncertain commit must not be retried implicitly.
    blocked: bool,
}

#[derive(Default)]
pub struct SftpOpenSyncManager {
    cache_gate: Mutex<()>,
    watches: Mutex<HashMap<(String, String), Watch>>,
    events: Mutex<VecDeque<OpenSyncEvent>>,
    next_seq: AtomicU64,
    worker_started: AtomicBool,
}

impl SftpOpenSyncManager {
    pub fn new() -> Self {
        Self {
            next_seq: AtomicU64::new(1),
            ..Self::default()
        }
    }

    // Both open entry points go through this method; there is no separate
    // registration step that could reset a dirty copy's baseline.
    pub(crate) async fn open_cached(
        self: &Arc<Self>,
        sftp_manager: Arc<SftpManager>,
        launcher_logs: Arc<LauncherLogStore>,
        session_id: String,
        remote_path: String,
        workspace_root: Option<String>,
        cache_root: &Path,
        safe_name: &str,
    ) -> Result<PathBuf, String> {
        let _gate = self.cache_gate.lock().await;
        let key = (session_id.clone(), remote_path.clone());
        let previous = self.watches.lock().await.get(&key).map(|w| w.copy.clone());
        if let Some(previous) = previous.as_ref() {
            if !previous.active.load(Ordering::Acquire) {
                return Err(
                    "SFTP edit session was disconnected; cached edits were retained".into(),
                );
            }
            match LocalStamp::read(&previous.local_path).await {
                Ok(_) => return Ok(previous.local_path.clone()),
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                    // The path can be briefly absent while an editor saves by rename.
                    return Err("cached edit copy is temporarily unavailable; wait for the editor or reconnect to open a new copy".into());
                }
                Err(e) => return Err(format!("cannot reopen cached edit copy: {e}")),
            }
        }

        let managed = sftp_manager.get_session(&session_id).await?;
        let (transport, _) = managed.ensure_live().await?;
        let (canonical_path, before_meta) = canonical_regular_path(&transport.sftp, &remote_path)
            .await
            .map_err(|e| e.to_string())?;
        if let Some(root) = workspace_root {
            let canonical_root = transport
                .sftp
                .canonicalize(root)
                .await
                .map_err(|e| sftp_err("resolve remote workspace", e).to_string())?;
            if !path_in_remote_workspace(&canonical_path, &canonical_root) {
                return Err("remote edit file is outside the ChemSSH workspace".into());
            }
        }

        tokio::fs::create_dir_all(cache_root)
            .await
            .map_err(|e| e.to_string())?;
        // A unique app-owned bucket leaves old and untracked edits untouched.
        // A dropped TempDir removes failed or cancelled initial downloads.
        let mut bucket_builder = tempfile::Builder::new();
        bucket_builder.prefix("edit-");
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            bucket_builder.permissions(std::fs::Permissions::from_mode(0o700));
        }
        let bucket = bucket_builder
            .tempdir_in(cache_root)
            .map_err(|e| format!("create edit cache directory: {e}"))?;
        let path = bucket.path().join(safe_name);
        let mut local = tokio::fs::OpenOptions::new()
            .create_new(true)
            .write(true)
            .open(&path)
            .await
            .map_err(|e| e.to_string())?;
        let baseline = download_baseline(&transport, &canonical_path, &before_meta, &mut local)
            .await
            .map_err(|e| e.to_string())?;
        local.flush().await.map_err(|e| e.to_string())?;
        drop(local);
        let synced_stamp = LocalStamp::read(&path).await.map_err(|e| e.to_string())?;
        let mut watches = self.watches.lock().await;
        // Snapshot after download, before publication, in case the session closed.
        sftp_manager.get_entry_with_id(&session_id).await?;
        let _ = bucket.keep();
        watches.insert(
            key,
            Watch {
                copy: EditCopy {
                    session_id,
                    remote_path,
                    canonical_path,
                    local_path: path.clone(),
                    endpoint: Endpoint::of(&transport),
                    active: Arc::new(AtomicBool::new(true)),
                },
                baseline,
                synced_stamp,
                seen_stamp: Some(synced_stamp),
                seen_since: Instant::now(),
                retry_at: None,
                ignored_stamp: None,
                blocked: false,
            },
        );
        drop(watches);

        if !self.worker_started.swap(true, Ordering::AcqRel) {
            let mgr = Arc::clone(self);
            tokio::spawn(async move { mgr.run_loop(sftp_manager, launcher_logs).await });
        }
        Ok(path)
    }

    pub async fn unregister_session(&self, session_id: &str) {
        let watches = self.watches.lock().await;
        for watch in watches.values().filter(|w| w.copy.session_id == session_id) {
            watch.copy.active.store(false, Ordering::Release);
        }
    }

    pub(crate) async fn clear_clean_cache(&self, root: &Path) -> Result<(usize, usize), String> {
        let _gate = self.cache_gate.lock().await;
        let mut watches = self.watches.lock().await;
        let mut entries = match tokio::fs::read_dir(root).await {
            Ok(entries) => entries,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok((0, 0)),
            Err(e) => return Err(e.to_string()),
        };
        let (mut removed, mut kept) = (0, 0);
        while let Some(entry) = entries.next_entry().await.map_err(|e| e.to_string())? {
            let dir = entry.path();
            let Some((key, watch)) = watches
                .iter()
                .find(|(_, w)| w.copy.local_path.parent() == Some(&dir))
            else {
                // Legacy and previous-process buckets have no trusted baseline.
                kept += 1;
                continue;
            };
            if watch.copy.active.load(Ordering::Acquire)
                || watch.blocked
                || !entry.file_type().await.is_ok_and(|ty| ty.is_dir())
            {
                kept += 1;
                continue;
            }
            let path = &watch.copy.local_path;
            let stamp = match LocalStamp::read(path).await {
                Ok(stamp) => stamp,
                Err(_) => {
                    kept += 1;
                    continue;
                }
            };
            if !matches!(fingerprint_local(path, stamp).await, Ok(hash) if hash == watch.baseline) {
                kept += 1;
                continue;
            }
            // Do not recursively delete untracked files (e.g. editor backups).
            let mut children = tokio::fs::read_dir(&dir).await.map_err(|e| e.to_string())?;
            let only_file = children
                .next_entry()
                .await
                .map_err(|e| e.to_string())?
                .is_some_and(|child| child.path() == *path)
                && children
                    .next_entry()
                    .await
                    .map_err(|e| e.to_string())?
                    .is_none();
            if !only_file || LocalStamp::read(path).await.ok() != Some(stamp) {
                kept += 1;
                continue;
            }
            tokio::fs::remove_file(path)
                .await
                .map_err(|e| e.to_string())?;
            let _ = tokio::fs::remove_dir(&dir).await;
            let key = key.clone();
            watches.remove(&key);
            removed += 1;
        }
        Ok((removed, kept))
    }

    pub async fn events_after(&self, after_seq: u64) -> Vec<OpenSyncEvent> {
        let guard = self.events.lock().await;
        guard
            .iter()
            .filter(|ev| ev.seq > after_seq)
            .cloned()
            .collect()
    }

    pub async fn events_after_for_session(
        &self,
        session_id: &str,
        after_seq: u64,
    ) -> Vec<OpenSyncEvent> {
        if session_id.is_empty() {
            return Vec::new();
        }
        let guard = self.events.lock().await;
        guard
            .iter()
            .filter(|ev| ev.seq > after_seq && ev.session == session_id)
            .cloned()
            .collect()
    }

    pub(crate) async fn push_event(
        &self,
        session: &str,
        remote_path: &str,
        local_path: &str,
        status: &str,
        error: Option<String>,
    ) {
        let mut guard = self.events.lock().await;
        let seq = self.next_seq.fetch_add(1, Ordering::Relaxed);
        let event = OpenSyncEvent {
            seq,
            time: Utc::now().to_rfc3339_opts(SecondsFormat::Secs, true),
            session: session.to_string(),
            remote_path: remote_path.to_string(),
            local_path: local_path.to_string(),
            status: status.to_string(),
            error,
        };
        while guard.len() >= MAX_SYNC_EVENTS {
            guard.pop_front();
        }
        guard.push_back(event);
    }

    async fn run_loop(
        self: Arc<Self>,
        sftp_manager: Arc<SftpManager>,
        launcher_logs: Arc<LauncherLogStore>,
    ) {
        let mut ticker = tokio::time::interval(POLL_INTERVAL);
        ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        loop {
            ticker.tick().await;
            self.poll_once(&sftp_manager, &launcher_logs).await;
        }
    }

    async fn poll_once(&self, sftp_manager: &SftpManager, launcher_logs: &LauncherLogStore) {
        let candidates: Vec<_> = self
            .watches
            .lock()
            .await
            .iter()
            .filter(|(_, w)| w.copy.active.load(Ordering::Acquire) && !w.blocked)
            .map(|(key, w)| (key.clone(), w.copy.clone()))
            .collect();
        for (key, copy) in candidates {
            let stamp = LocalStamp::read(&copy.local_path).await.ok();
            let now = Instant::now();
            let (stamp, baseline) = {
                let mut watches = self.watches.lock().await;
                let Some(watch) = watches.get_mut(&key) else {
                    continue;
                };
                if !watch.copy.active.load(Ordering::Acquire) {
                    continue;
                }
                if watch.seen_stamp != stamp {
                    watch.seen_stamp = stamp;
                    watch.seen_since = now;
                    watch.ignored_stamp = None;
                    continue;
                }
                let Some(stamp) = stamp else {
                    continue;
                };
                if stamp == watch.synced_stamp
                    || Some(stamp) == watch.ignored_stamp
                    || now.duration_since(watch.seen_since) < DEBOUNCE_DURATION
                    || watch.retry_at.is_some_and(|t| now < t)
                {
                    continue;
                }
                (stamp, watch.baseline)
            };
            let outcome = if stamp.len > MAX_SYNC_FILE_BYTES {
                Err(SyncFailure::Oversized)
            } else {
                upload_cached_file(sftp_manager, &copy, stamp, baseline).await
            };
            let local_str = copy.local_path.to_string_lossy();
            let (status, error) = {
                let mut watches = self.watches.lock().await;
                let Some(watch) = watches.get_mut(&key) else {
                    continue;
                };
                match outcome {
                    Ok(fingerprint) => {
                        watch.baseline = fingerprint;
                        watch.synced_stamp = stamp;
                        watch.retry_at = None;
                        if fingerprint == baseline {
                            continue;
                        }
                        ("done", None)
                    }
                    Err(SyncFailure::LocalChanged | SyncFailure::Disconnected) => continue,
                    Err(err) => {
                        match err {
                            SyncFailure::Conflict(_) | SyncFailure::Publish(_) => {
                                watch.blocked = true
                            }
                            SyncFailure::Oversized => watch.ignored_stamp = Some(stamp),
                            _ => watch.retry_at = Some(Instant::now() + ERROR_RETRY_BACKOFF),
                        }
                        ("error", Some(err.to_string()))
                    }
                }
            };
            let remote_path = &copy.remote_path;
            if let Some(err) = &error {
                launcher_logs.push(format!("[sftp-sync] failed {remote_path}: {err}"));
            } else {
                launcher_logs.push(format!("[sftp-sync] synced {remote_path}"));
            }
            self.push_event(&copy.session_id, remote_path, &local_str, status, error)
                .await;
        }
    }
}

fn path_in_remote_workspace(path: &str, root: &str) -> bool {
    let prefix = root.trim_end_matches('/');
    path.starts_with('/')
        && root.starts_with('/')
        && path
            .strip_prefix(prefix)
            .is_some_and(|suffix| suffix.starts_with('/'))
}

async fn canonical_regular_path(
    sftp: &SftpSession,
    path: &str,
) -> Result<(String, FileAttributes), TransferError> {
    let canonical = sftp
        .canonicalize(path)
        .await
        .map_err(|e| sftp_err("resolve remote edit file", e))?;
    let meta = sftp
        .symlink_metadata(&canonical)
        .await
        .map_err(|e| sftp_err("stat remote edit file", e))?;
    if !is_regular_file(&meta) {
        return Err(TransferError::InvalidInput(
            "remote edit path is not a regular file".into(),
        ));
    }
    if meta.size.is_some_and(|size| size > MAX_SYNC_FILE_BYTES) {
        return Err(TransferError::InvalidInput(
            "remote edit file exceeds the 100 MiB limit".into(),
        ));
    }
    Ok((canonical, meta))
}

async fn digest_copy<R: AsyncRead + Unpin, W: AsyncWrite + Unpin>(
    reader: &mut R,
    writer: &mut W,
    max_bytes: u64,
) -> std::io::Result<(Fingerprint, u64)> {
    let mut hash = Sha256::new();
    let mut buf = vec![0; CHUNK_SIZE];
    let mut len = 0;
    loop {
        let n = reader.read(&mut buf).await?;
        if n == 0 {
            break;
        }
        len += n as u64;
        if len > max_bytes {
            return Err(std::io::Error::other("file exceeded the transfer limit"));
        }
        hash.update(&buf[..n]);
        writer.write_all(&buf[..n]).await?;
    }
    Ok((hash.finalize().into(), len))
}

async fn remote_fingerprint_with_before<W: AsyncWrite + Unpin>(
    sftp: &SftpSession,
    path: &str,
    before: &FileAttributes,
    writer: &mut W,
) -> Result<(Fingerprint, u32), TransferError> {
    if !is_regular_file(before) || before.size.is_some_and(|len| len > MAX_SYNC_FILE_BYTES) {
        return Err(TransferError::InvalidInput(
            "remote edit file is not a regular file within the 100 MiB limit".into(),
        ));
    }
    let mut remote = sftp
        .open(path)
        .await
        .map_err(|e| sftp_err("open remote edit file", e))?;
    let (hash, len) = digest_copy(&mut remote, writer, MAX_SYNC_FILE_BYTES)
        .await
        .map_err(|e| remote_io_err("read remote edit file", e))?;
    let after = sftp
        .symlink_metadata(path)
        .await
        .map_err(|e| sftp_err("stat remote edit file", e))?;
    Ok((hash, checked_remote_mode(before, &after, Some(len))?))
}

fn checked_remote_mode(
    before: &FileAttributes,
    after: &FileAttributes,
    len: Option<u64>,
) -> Result<u32, TransferError> {
    if !is_regular_file(after)
        || before.size != after.size
        || before.mtime != after.mtime
        || before.permissions != after.permissions
        || len
            .zip(after.size)
            .is_some_and(|(read, expected)| read != expected)
    {
        return Err(TransferError::InvalidInput(
            "remote edit file changed while reading".into(),
        ));
    }
    let mode = after.permissions.ok_or_else(|| {
        TransferError::InvalidInput("remote server did not report file permissions".into())
    })?;
    Ok(mode & 0o777)
}

async fn verify_remote_fingerprint(
    transport: &SftpTransport,
    path: &str,
    before: &FileAttributes,
) -> Result<(Fingerprint, u32), TransferError> {
    let sftp = &transport.sftp;
    if !is_regular_file(before) || before.size.is_some_and(|size| size > MAX_SYNC_FILE_BYTES) {
        return Err(TransferError::InvalidInput(
            "remote edit file is not a regular file within the 100 MiB limit".into(),
        ));
    }
    // For small files (<= 256 KiB), reading directly over the already-open SFTP
    // channel takes ~1 RTT and avoids spawning remote shell exec channels.
    if before
        .size
        .is_some_and(|size| size > REMOTE_HASH_SHELL_THRESHOLD_BYTES)
    {
        if let Some(tool) = remote_hash_tool(transport, path).await {
            let command = format!(
                "p={}; [ -f \"$p\" ] && [ ! -L \"$p\" ] && {} < \"$p\"",
                shell_quote(path),
                tool.command(),
            );
            let hash = transport
                .connection
                .exec_command_timeout(&command, Duration::from_secs(15))
                .await
                .ok()
                .filter(|out| out.exit_code == Some(0))
                .and_then(|out| parse_remote_sha256(&out.stdout));
            if let Some(hash) = hash {
                let after = sftp
                    .symlink_metadata(path)
                    .await
                    .map_err(|e| sftp_err("stat remote edit file", e))?;
                return Ok((hash, checked_remote_mode(before, &after, None)?));
            }
        }
    }
    remote_fingerprint_with_before(sftp, path, before, &mut tokio::io::sink()).await
}

async fn download_baseline(
    transport: &SftpTransport,
    path: &str,
    before: &FileAttributes,
    local: &mut tokio::fs::File,
) -> Result<Fingerprint, TransferError> {
    let (hash, _) = remote_fingerprint_with_before(&transport.sftp, path, before, local).await?;
    Ok(hash)
}

#[derive(Debug)]
enum SyncFailure {
    LocalChanged,
    Disconnected,
    Oversized,
    Conflict(String),
    Publish(String),
    Retry(String),
}

impl std::fmt::Display for SyncFailure {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::LocalChanged => f.write_str("cached edit is still changing"),
            Self::Disconnected => f.write_str("SFTP edit session disconnected"),
            Self::Oversized => f.write_str("cached edit exceeds the 100 MiB limit; local copy retained"),
            Self::Conflict(reason) => write!(f, "remote edit conflict: {reason}; auto-sync stopped and local copy retained"),
            Self::Publish(reason) => write!(f, "could not confirm safe remote publication: {reason}; auto-sync stopped and local copy retained"),
            Self::Retry(reason) => f.write_str(reason),
        }
    }
}

async fn read_local(path: &Path, stamp: LocalStamp) -> Result<tokio::fs::File, SyncFailure> {
    if LocalStamp::read(path).await.ok() != Some(stamp) {
        return Err(SyncFailure::LocalChanged);
    }
    let file = tokio::fs::File::open(path)
        .await
        .map_err(|e| SyncFailure::Retry(e.to_string()))?;
    if file
        .metadata()
        .await
        .and_then(LocalStamp::from_metadata)
        .ok()
        != Some(stamp)
    {
        return Err(SyncFailure::LocalChanged);
    }
    Ok(file)
}

async fn fingerprint_local(path: &Path, stamp: LocalStamp) -> Result<Fingerprint, SyncFailure> {
    let mut file = read_local(path, stamp).await?;
    let (hash, len) = digest_copy(&mut file, &mut tokio::io::sink(), stamp.len)
        .await
        .map_err(|e| SyncFailure::Retry(e.to_string()))?;
    if len != stamp.len || LocalStamp::read(path).await.ok() != Some(stamp) {
        return Err(SyncFailure::LocalChanged);
    }
    Ok(hash)
}

async fn require_remote_baseline(
    transport: &SftpTransport,
    copy: &EditCopy,
    baseline: Fingerprint,
) -> Result<u32, SyncFailure> {
    let (canonical, before) = canonical_regular_path(&transport.sftp, &copy.remote_path)
        .await
        .map_err(|e| remote_baseline_failure(e))?;
    if canonical != copy.canonical_path {
        return Err(SyncFailure::Conflict(
            "remote path now resolves to a different file".into(),
        ));
    }
    let (hash, mode) = verify_remote_fingerprint(transport, &canonical, &before)
        .await
        .map_err(remote_baseline_failure)?;
    compare_remote_baseline(hash, mode, baseline)
}

fn compare_remote_baseline(
    hash: Fingerprint,
    mode: u32,
    baseline: Fingerprint,
) -> Result<u32, SyncFailure> {
    if hash != baseline {
        return Err(SyncFailure::Conflict(
            "remote file contents changed since the last sync".into(),
        ));
    }
    Ok(mode)
}

fn remote_baseline_failure(error: TransferError) -> SyncFailure {
    if error.is_transport_error() {
        SyncFailure::Retry(error.to_string())
    } else {
        SyncFailure::Conflict(format!("could not verify original remote file ({error})"))
    }
}

async fn upload_cached_file(
    sftp_manager: &SftpManager,
    copy: &EditCopy,
    stamp: LocalStamp,
    baseline: Fingerprint,
) -> Result<Fingerprint, SyncFailure> {
    let fingerprint = fingerprint_local(&copy.local_path, stamp).await?;
    if fingerprint == baseline {
        return Ok(fingerprint);
    }
    if !copy.active.load(Ordering::Acquire) {
        return Err(SyncFailure::Disconnected);
    }
    let managed = match sftp_manager.get_session(&copy.session_id).await {
        Ok(managed) => managed,
        Err(_) => {
            copy.active.store(false, Ordering::Release);
            return Err(SyncFailure::Disconnected);
        }
    };
    // Reconnect before side effects only. Do not replay a possibly executed commit.
    let (transport, _) = managed.ensure_live().await.map_err(SyncFailure::Retry)?;
    if Endpoint::of(&transport) != copy.endpoint {
        return Err(SyncFailure::Conflict("remote endpoint changed".into()));
    }
    let mode = require_remote_baseline(&transport, copy, baseline).await?;
    let mut local = read_local(&copy.local_path, stamp).await?;
    let mut staged = RemoteStagedFile::create_for_existing_with_ssh(
        Arc::clone(&transport.sftp),
        &copy.canonical_path,
        mode,
        &transport.connection,
    )
    .await
    .map_err(remote_baseline_failure)?;
    let result = async {
        let (written, len) = digest_copy(&mut local, staged.file_mut(), stamp.len)
            .await
            .map_err(|e| SyncFailure::Retry(e.to_string()))?;
        if len != stamp.len
            || written != fingerprint
            || fingerprint_local(&copy.local_path, stamp).await? != written
        {
            return Err(SyncFailure::LocalChanged);
        }
        staged
            .verify_size(stamp.len)
            .await
            .map_err(|e| SyncFailure::Retry(e.to_string()))?;
        if require_remote_baseline(&transport, copy, baseline).await? != mode {
            return Err(SyncFailure::Conflict(
                "remote permissions changed during sync".into(),
            ));
        }
        if LocalStamp::read(&copy.local_path).await.ok() != Some(stamp) {
            return Err(SyncFailure::LocalChanged);
        }
        let (_, current) = sftp_manager
            .get_entry_with_id(&copy.session_id)
            .await
            .map_err(|_| SyncFailure::Disconnected)?;
        if !copy.active.load(Ordering::Acquire) {
            return Err(SyncFailure::Disconnected);
        }
        if !Arc::ptr_eq(&transport.sftp, &current.sftp) {
            return Err(SyncFailure::Retry(
                "SFTP transport changed during sync".into(),
            ));
        }
        staged
            .commit_with_ssh(&transport.connection)
            .await
            .map_err(|e| SyncFailure::Publish(e.to_string()))?;
        Ok(written)
    }
    .await;
    if result.is_err() {
        staged.cleanup().await;
    }
    result
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn remote_hash_parses_supported_tools_and_rejects_ambiguous_output() {
        let hex = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855";
        let digest: Fingerprint = Sha256::digest([]).into();
        assert_eq!(parse_remote_sha256(&format!("{hex}  -\n")), Some(digest));
        assert_eq!(
            parse_remote_sha256(&format!("SHA2-256(stdin)= {hex}\n")),
            Some(digest)
        );
        assert_eq!(parse_remote_sha256(&format!("{hex} {hex}\n")), None);
        assert_eq!(parse_remote_sha256(&format!("banner\n{hex}  -")), None);
        assert_eq!(parse_remote_sha256("not a hash"), None);
    }

    #[test]
    fn a_content_mismatch_remains_a_conflict_even_with_identical_metadata() {
        let mut metadata = FileAttributes::empty();
        metadata.size = Some(3);
        metadata.mtime = Some(100);
        metadata.permissions = Some(0o100664);
        let mode = checked_remote_mode(&metadata, &metadata, Some(3)).unwrap();
        assert_eq!(mode, 0o664);
        let baseline = Sha256::digest(b"old").into();
        assert!(matches!(
            compare_remote_baseline(Sha256::digest(b"new").into(), mode, baseline),
            Err(SyncFailure::Conflict(_))
        ));
        assert_eq!(
            compare_remote_baseline(baseline, mode, baseline).unwrap(),
            mode
        );
        assert!(checked_remote_mode(&metadata, &metadata, Some(2)).is_err());
        let mut changed = metadata.clone();
        changed.permissions = Some(0o100640);
        assert!(checked_remote_mode(&metadata, &changed, None).is_err());
        changed.permissions = Some(0o120664);
        assert!(!is_regular_file(&changed));
        assert!(checked_remote_mode(&metadata, &changed, None).is_err());
    }

    async fn add_copy(
        manager: &SftpOpenSyncManager,
        root: &Path,
        session: &str,
        active: bool,
    ) -> PathBuf {
        let bucket = root.join(session);
        tokio::fs::create_dir(&bucket).await.unwrap();
        let path = bucket.join("edit.txt");
        tokio::fs::write(&path, b"original").await.unwrap();
        let stamp = LocalStamp::read(&path).await.unwrap();
        let watch = Watch {
            copy: EditCopy {
                session_id: session.into(),
                remote_path: "/home/user/edit.txt".into(),
                canonical_path: "/home/user/edit.txt".into(),
                local_path: path.clone(),
                endpoint: Endpoint("example.com".into(), 22, "user".into()),
                active: Arc::new(AtomicBool::new(active)),
            },
            baseline: fingerprint_local(&path, stamp).await.unwrap(),
            synced_stamp: stamp,
            seen_stamp: Some(stamp),
            seen_since: Instant::now(),
            retry_at: None,
            ignored_stamp: None,
            blocked: false,
        };
        manager
            .watches
            .lock()
            .await
            .insert((session.into(), watch.copy.remote_path.clone()), watch);
        path
    }

    #[tokio::test]
    async fn cache_clear_removes_only_known_inactive_clean_copies() {
        let tmp = tempfile::tempdir().unwrap();
        let root = tmp.path();
        let manager = SftpOpenSyncManager::new();
        let clean = add_copy(&manager, root, "clean", true).await;
        manager.unregister_session("clean").await;
        let active = add_copy(&manager, root, "active", true).await;
        let dirty = add_copy(&manager, root, "dirty", false).await;
        tokio::fs::write(&dirty, b"modified").await.unwrap();
        let backed_up = add_copy(&manager, root, "backup", false).await;
        let backup = backed_up.with_extension("bak");
        tokio::fs::write(&backup, b"editor backup").await.unwrap();
        let legacy = root.join("legacy.txt");
        tokio::fs::write(&legacy, b"unknown provenance")
            .await
            .unwrap();

        assert_eq!(manager.clear_clean_cache(root).await.unwrap(), (1, 4));
        assert!(!clean.exists());
        assert!(active.exists());
        assert_eq!(tokio::fs::read(&dirty).await.unwrap(), b"modified");
        assert!(backup.exists());
        assert!(legacy.exists());
    }

    #[tokio::test]
    async fn reopen_reuses_existing_dirty_copy_and_retains_baseline() {
        let tmp = tempfile::tempdir().unwrap();
        let manager = Arc::new(SftpOpenSyncManager::new());
        let clean = add_copy(&manager, tmp.path(), "session", true).await;
        tokio::fs::write(&clean, b"modified").await.unwrap();
        let key = ("session".into(), "/home/user/edit.txt".into());
        let baseline = manager.watches.lock().await[&key].baseline;
        let result = manager
            .open_cached(
                Arc::new(SftpManager::default()),
                Arc::new(LauncherLogStore::default()),
                "session".into(),
                "/home/user/edit.txt".into(),
                None,
                tmp.path(),
                "edit.txt",
            )
            .await
            .unwrap();
        assert_eq!(result, clean);
        assert_eq!(tokio::fs::read(&result).await.unwrap(), b"modified");
        assert_eq!(manager.watches.lock().await[&key].baseline, baseline);
    }

    #[tokio::test]
    async fn digest_copy_refuses_stream_growth_past_limit() {
        let mut output = Vec::new();
        let result = digest_copy(&mut &b"too long"[..], &mut output, 3).await;
        assert!(result.is_err());
        assert!(output.is_empty());
    }

    #[test]
    fn remote_workspace_check_requires_component_boundary() {
        assert!(path_in_remote_workspace("/work/nested/edit.txt", "/work"));
        assert!(path_in_remote_workspace("/work/edit.txt", "/"));
        assert!(!path_in_remote_workspace("/work-extra/edit.txt", "/work"));
        assert!(!path_in_remote_workspace("/work", "/work"));
    }
}
