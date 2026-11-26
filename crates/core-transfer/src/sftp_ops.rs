use std::borrow::Borrow;
use std::path::Path;
use std::time::Duration;

use core_ssh::{shell_quote, SshConnection};
use russh_sftp::client::{error::Error as SftpError, fs::File, RawSftpSession, SftpSession};
use russh_sftp::protocol::{FileAttributes, OpenFlags, Packet, StatusCode};
use tokio::io::{AsyncReadExt, AsyncWriteExt};

use crate::local_fs::{format_iso_utc, format_unix_mode, FsEntry};
use crate::progress::TransferProgressStore;
use crate::{remote_io_err, sftp_err, TransferError};

const CHUNK_SIZE: usize = 256 * 1024;

pub async fn list_remote_dir(
    sftp: &SftpSession,
    path: &str,
) -> Result<Vec<FsEntry>, TransferError> {
    let dir = sftp
        .read_dir(path)
        .await
        .map_err(|e| sftp_err(format!("list {path}"), e))?;
    let base = path.trim_end_matches('/');
    let mut entries = Vec::new();
    let mut symlink_indices = Vec::new();

    for entry in dir {
        let name = entry.file_name();
        if name == "." || name == ".." {
            continue;
        }
        let meta = entry.metadata();
        let is_symlink = entry.file_type().is_symlink() || meta.is_symlink();
        let is_dir = meta.is_dir();
        let full_path = if base.is_empty() {
            format!("/{name}")
        } else {
            format!("{base}/{name}")
        };
        let mod_time_ms = meta.mtime.map(|m| m as i64 * 1000).unwrap_or(0);
        if is_symlink {
            symlink_indices.push((entries.len(), full_path.clone()));
        }
        entries.push(FsEntry {
            name,
            path: full_path,
            is_dir,
            size: if is_dir { 0 } else { meta.size.unwrap_or(0) },
            mod_time_ms,
            mod_time: format_iso_utc(mod_time_ms),
            mode: format_unix_mode(is_dir, meta.permissions),
            is_symlink,
            link_target: String::new(),
            link_broken: false,
        });
    }

    if !symlink_indices.is_empty() {
        let queue = tokio::sync::Mutex::new(symlink_indices);
        let results = tokio::sync::Mutex::new(Vec::new());

        let resolve_worker = || async {
            loop {
                let next = {
                    let mut guard = queue.lock().await;
                    guard.pop()
                };
                let Some((idx, full_path)) = next else {
                    break;
                };
                let link_target = sftp.read_link(&full_path).await.unwrap_or_default();
                let target_meta = sftp.metadata(&full_path).await.ok();
                let mut guard = results.lock().await;
                guard.push((idx, link_target, target_meta));
            }
        };

        tokio::join!(
            resolve_worker(),
            resolve_worker(),
            resolve_worker(),
            resolve_worker(),
            resolve_worker(),
            resolve_worker(),
            resolve_worker(),
            resolve_worker(),
        );

        let resolved = results.into_inner();
        for (idx, link_target, target_meta) in resolved {
            if let Some(entry) = entries.get_mut(idx) {
                entry.link_target = link_target;
                match target_meta {
                    Some(meta) => {
                        let is_dir = meta.is_dir();
                        let mod_time_ms = meta
                            .mtime
                            .map(|m| m as i64 * 1000)
                            .unwrap_or(entry.mod_time_ms);
                        entry.is_dir = is_dir;
                        entry.size = if is_dir {
                            0
                        } else {
                            meta.size.unwrap_or(entry.size)
                        };
                        entry.mod_time_ms = mod_time_ms;
                        entry.mod_time = format_iso_utc(mod_time_ms);
                        entry.mode = format_unix_mode(is_dir, meta.permissions);
                        entry.link_broken = false;
                    }
                    None => {
                        entry.is_dir = false;
                        entry.link_broken = true;
                    }
                }
            }
        }
    }

    entries.sort_by(|a, b| {
        b.is_dir
            .cmp(&a.is_dir)
            .then_with(|| a.name.to_lowercase().cmp(&b.name.to_lowercase()))
    });
    Ok(entries)
}

pub async fn delete_remote_path(sftp: &SftpSession, path: &str) -> Result<(), TransferError> {
    let lmeta = sftp
        .symlink_metadata(path)
        .await
        .map_err(|e| sftp_err(format!("lstat {path}"), e))?;

    if lmeta.is_symlink() || !lmeta.is_dir() {
        sftp.remove_file(path)
            .await
            .map_err(|e| sftp_err(format!("remove {path}"), e))?;
        return Ok(());
    }

    let entries = sftp
        .read_dir(path)
        .await
        .map_err(|e| sftp_err(format!("read dir {path}"), e))?;
    let base = path.trim_end_matches('/');
    for entry in entries {
        let name = entry.file_name();
        if name == "." || name == ".." {
            continue;
        }
        let child = if base.is_empty() {
            format!("/{name}")
        } else {
            format!("{base}/{name}")
        };
        if entry.file_type().is_symlink() || !entry.metadata().is_dir() {
            sftp.remove_file(&child)
                .await
                .map_err(|e| sftp_err(format!("remove {child}"), e))?;
        } else {
            Box::pin(delete_remote_path(sftp, &child)).await?;
        }
    }

    sftp.remove_dir(path)
        .await
        .map_err(|e| sftp_err(format!("rmdir {path}"), e))?;
    Ok(())
}

const DEFAULT_NEW_FILE_MODE: u32 = 0o666;
const STAGING_DIR_MODE: u32 = 0o700;
const PRIVATE_FILE_MODE: u32 = 0o600;
const REGULAR_TYPE: u32 = 0o100000;
const DIRECTORY_TYPE: u32 = 0o040000;
const TYPE_MASK: u32 = 0o170000;
const PERMISSION_MASK: u32 = 0o777;
const POSIX_RENAME: &str = "posix-rename@openssh.com";
const SSH_PROBE_TIMEOUT: Duration = Duration::from_secs(3);
const SSH_REPLACE_TIMEOUT: Duration = Duration::from_secs(10);
const EXCLUSIVE_WRITE: OpenFlags = OpenFlags::WRITE
    .union(OpenFlags::CREATE)
    .union(OpenFlags::EXCLUDE);

fn attrs_with_mode(mode: u32) -> FileAttributes {
    let mut attrs = FileAttributes::empty();
    attrs.permissions = Some(mode);
    attrs
}

#[derive(Debug, PartialEq, Eq)]
struct ExistingTarget {
    mode: u32,
    size: Option<u64>,
    mtime: Option<u32>,
    uid: Option<u32>,
    gid: Option<u32>,
}

fn target_from_attrs(attrs: FileAttributes) -> Result<ExistingTarget, TransferError> {
    let Some(mode) = attrs.permissions else {
        return Err(TransferError::InvalidInput(
            "remote server did not report target permissions".into(),
        ));
    };
    // Compare the type bits exactly; FileAttributes::is_regular is a flag
    // containment check and also returns true for symbolic links.
    if mode & TYPE_MASK != REGULAR_TYPE {
        return Err(TransferError::InvalidInput(
            "remote destination must be a regular file, not a link or directory".into(),
        ));
    }
    Ok(ExistingTarget {
        mode: mode & PERMISSION_MASK,
        size: attrs.size,
        mtime: attrs.mtime,
        uid: attrs.uid,
        gid: attrs.gid,
    })
}

async fn read_target(
    sftp: &SftpSession,
    destination: &str,
) -> Result<Option<ExistingTarget>, TransferError> {
    match sftp.symlink_metadata(destination).await {
        Ok(attrs) => target_from_attrs(attrs).map(Some),
        Err(SftpError::Status(err)) if err.status_code == StatusCode::NoSuchFile => Ok(None),
        Err(err) => Err(sftp_err("stat remote transfer destination", err)),
    }
}

fn canonical_child(parent: &str, basename: &str) -> String {
    format!("{}/{basename}", parent.trim_end_matches('/'))
}

struct NamespaceMarker {
    path: String,
    nonce: String,
}

/// Private directory on the destination filesystem, with an exclusively
/// created staging file inside. The destination itself is not opened for write.
pub struct RemoteStagedFile<S> {
    sftp: S,
    destination: String,
    requested_parent: String,
    canonical_parent: String,
    target: Option<ExistingTarget>,
    create_mode: u32,
    directory: String,
    staging: String,
    marker: Option<NamespaceMarker>,
    posix_session: Option<RawSftpSession>,
    file: Option<File>,
    sealed: bool,
    published: bool,
    publication_uncertain: bool,
}

impl<S: Borrow<SftpSession>> RemoteStagedFile<S> {
    pub async fn create(sftp: S, destination: &str) -> Result<Self, TransferError> {
        Self::create_inner(sftp, destination, DEFAULT_NEW_FILE_MODE, None, None).await
    }

    pub async fn create_with_ssh(
        sftp: S,
        destination: &str,
        connection: &SshConnection,
    ) -> Result<Self, TransferError> {
        Self::create_inner(
            sftp,
            destination,
            DEFAULT_NEW_FILE_MODE,
            None,
            Some(connection),
        )
        .await
    }

    pub async fn create_for_existing_with_ssh(
        sftp: S,
        destination: &str,
        expected_mode: u32,
        connection: &SshConnection,
    ) -> Result<Self, TransferError> {
        Self::create_inner(
            sftp,
            destination,
            DEFAULT_NEW_FILE_MODE,
            Some(expected_mode),
            Some(connection),
        )
        .await
    }

    async fn create_inner(
        sftp: S,
        destination: &str,
        new_mode: u32,
        expected_mode: Option<u32>,
        connection: Option<&SshConnection>,
    ) -> Result<Self, TransferError> {
        let (requested_parent, name) = destination
            .rsplit_once('/')
            .map_or((".", destination), |(parent, name)| {
                (if parent.is_empty() { "/" } else { parent }, name)
            });
        if matches!(name, "" | "." | "..") || destination.contains('\0') {
            return Err(TransferError::InvalidInput(
                "invalid remote file destination".into(),
            ));
        }
        let session = sftp.borrow();
        let parent = session
            .canonicalize(requested_parent)
            .await
            .map_err(|err| sftp_err("resolve remote destination parent", err))?;
        if !parent.starts_with('/') || parent.contains('\0') {
            return Err(TransferError::InvalidInput(
                "invalid canonical remote parent".into(),
            ));
        }
        let destination = canonical_child(&parent, name);
        let target = read_target(session, &destination).await?;
        if expected_mode.is_some_and(|mode| {
            target.as_ref().map(|target| target.mode) != Some(mode & PERMISSION_MASK)
        }) {
            return Err(TransferError::InvalidInput(
                "remote destination permissions or type changed".into(),
            ));
        }
        let directory = canonical_child(
            &parent,
            &format!(".chemssh-transfer-{}", uuid::Uuid::new_v4()),
        );
        let staging = canonical_child(&directory, "payload.part");
        let (marker, posix_session) =
            create_private_directory(session, connection, &directory, &parent, target.is_some())
                .await?;
        // For new files the server applies its creation policy (including umask
        // and inherited defaults); SETSTAT afterwards would bypass that policy.
        let mode = if target.is_some() {
            PRIVATE_FILE_MODE
        } else {
            new_mode & PERMISSION_MASK
        };
        let file = match session
            .open_with_flags_and_attributes(&staging, EXCLUSIVE_WRITE, attrs_with_mode(mode))
            .await
        {
            Ok(file) => file,
            Err(err) => {
                cleanup_stage(session, &directory, Some(&staging), marker.as_ref()).await;
                return Err(sftp_err("create remote staging file", err));
            }
        };
        Ok(Self {
            sftp,
            destination,
            requested_parent: requested_parent.into(),
            canonical_parent: parent,
            target,
            create_mode: mode,
            directory,
            staging,
            marker,
            posix_session,
            file: Some(file),
            sealed: false,
            published: false,
            publication_uncertain: false,
        })
    }

    pub fn file_mut(&mut self) -> &mut File {
        self.file.as_mut().expect("staging file must be open")
    }

    /// Wait for every write acknowledgement and close the remote handle.
    pub async fn seal(&mut self) -> Result<(), TransferError> {
        if self.sealed {
            return Ok(());
        }
        let file = self
            .file
            .as_mut()
            .ok_or_else(|| TransferError::Internal("remote staging file already closed".into()))?;
        file.shutdown()
            .await
            .map_err(|err| remote_io_err("close remote staging file", err))?;
        self.file.take();
        self.sealed = true;
        Ok(())
    }

    pub async fn verify_size(&mut self, expected: u64) -> Result<(), TransferError> {
        self.seal().await?;
        let staged = self
            .sftp
            .borrow()
            .metadata(&self.staging)
            .await
            .map_err(|err| sftp_err("stat remote staging file", err))?;
        if let Some(actual) = staged.size {
            verify_length(Some(expected), actual)?;
        }
        Ok(())
    }

    pub async fn commit(&mut self) -> Result<(), TransferError> {
        self.publish(None).await
    }

    pub async fn commit_with_ssh(
        &mut self,
        connection: &SshConnection,
    ) -> Result<(), TransferError> {
        self.publish(Some(connection)).await
    }

    async fn recheck_target(&self) -> Result<(), TransferError> {
        let sftp = self.sftp.borrow();
        if sftp
            .canonicalize(&self.requested_parent)
            .await
            .map_err(|err| sftp_err("recheck remote destination parent", err))?
            != self.canonical_parent
            || read_target(sftp, &self.destination).await? != self.target
        {
            return Err(TransferError::InvalidInput(
                "remote transfer destination changed before publication".into(),
            ));
        }
        Ok(())
    }

    async fn publish(&mut self, connection: Option<&SshConnection>) -> Result<(), TransferError> {
        if self.published {
            return Ok(());
        }
        if self.publication_uncertain {
            return Err(publication_unknown(
                "previous publication did not return a definitive result",
            ));
        }
        self.seal().await?;
        let sftp = self.sftp.borrow();
        let staged = sftp
            .symlink_metadata(&self.staging)
            .await
            .map_err(|err| sftp_err("stat remote staging file before publication", err))?;
        if !staged.permissions.is_some_and(|mode| {
            mode & TYPE_MASK == REGULAR_TYPE && mode & 0o7777 & !self.create_mode == 0
        }) {
            return Err(TransferError::InvalidInput(
                "remote staged file type or permissions are unexpected".into(),
            ));
        }
        if let Some(target) = &self.target {
            sftp.set_metadata(&self.staging, attrs_with_mode(target.mode))
                .await
                .map_err(|err| sftp_err("preserve remote target permissions", err))?;
            let staged = sftp
                .symlink_metadata(&self.staging)
                .await
                .map_err(|err| sftp_err("verify remote staging permissions", err))?;
            if staged.permissions.map(|mode| mode & (TYPE_MASK | 0o7777))
                != Some(REGULAR_TYPE | target.mode)
            {
                return Err(TransferError::InvalidInput(
                    "remote server did not preserve target permissions".into(),
                ));
            }
        }
        self.recheck_target().await?;

        // Standard v3 RENAME is for new destinations. Overwrite only with
        // negotiated exact-path replacement or the verified shell fallback.
        let mut renamed = false;
        if let (Some(raw), Some(marker)) = (self.posix_session.take(), &self.marker) {
            // The channel already proved its namespace at setup. If it can no
            // longer read the marker, fall back before any rename is sent.
            if tokio::time::timeout(SSH_PROBE_TIMEOUT, raw_sees_marker(&raw, marker))
                .await
                .unwrap_or(false)
            {
                self.recheck_target().await?;
                self.publication_uncertain = true;
                match posix_rename(&raw, &self.staging, &self.destination).await {
                    PosixRenameResult::Replaced => renamed = true,
                    PosixRenameResult::Unsupported => {}
                    PosixRenameResult::Rejected(message) => {
                        self.publication_uncertain = false;
                        return Err(TransferError::Sftp(message));
                    }
                    PosixRenameResult::Unknown(message) => {
                        return Err(publication_unknown(message))
                    }
                }
                self.publication_uncertain = false;
            }
        }
        if self.target.is_none() {
            self.recheck_target().await?;
            self.publication_uncertain = true;
            match sftp.rename(&self.staging, &self.destination).await {
                Ok(()) => renamed = true,
                Err(err) if is_uncertain_sftp_result(&err) => return Err(publication_unknown(err)),
                Err(err) => {
                    self.publication_uncertain = false;
                    return Err(sftp_err("publish remote file", err));
                }
            }
            self.publication_uncertain = false;
        }
        if !renamed {
            let (Some(connection), Some(marker)) = (connection, &self.marker) else {
                return Err(no_safe_replacement());
            };
            if !crate::sftp_shell_sees_marker(connection, &marker.path, &marker.nonce).await
                || !shell_has_exact_mv(connection, &self.directory).await
            {
                return Err(no_safe_replacement());
            }
            self.recheck_target().await?;
            self.publication_uncertain = true;
            let cmd = format!(
                "command mv -fT -- {} {}",
                shell_quote(&self.staging),
                shell_quote(&self.destination)
            );
            let out = connection
                .exec_command_timeout(&cmd, SSH_REPLACE_TIMEOUT)
                .await
                .map_err(publication_unknown)?;
            match out.exit_code {
                Some(0) => self.publication_uncertain = false,
                Some(_) => return Err(publication_unknown(out.stderr.trim())),
                None => return Err(publication_unknown("missing remote exit status")),
            }
        }
        self.published = true;
        cleanup_stage(sftp, &self.directory, None, self.marker.as_ref()).await;
        Ok(())
    }

    pub async fn cleanup(&mut self) {
        if let Some(mut file) = self.file.take() {
            let _ = tokio::time::timeout(Duration::from_secs(2), file.shutdown()).await;
        }
        if !self.published && !self.publication_uncertain {
            cleanup_stage(
                self.sftp.borrow(),
                &self.directory,
                Some(&self.staging),
                self.marker.as_ref(),
            )
            .await;
        }
    }
}

async fn create_private_directory(
    sftp: &SftpSession,
    connection: Option<&SshConnection>,
    directory: &str,
    parent: &str,
    replacing: bool,
) -> Result<(Option<NamespaceMarker>, Option<RawSftpSession>), TransferError> {
    // The pinned high-level API has no MKDIR-with-attributes. Prefer the raw
    // channel on the same authenticated SSH connection, but verify a freshly
    // generated nonce through the high-level session before sending file data.
    let raw = if let Some(connection) = connection {
        tokio::time::timeout(SSH_PROBE_TIMEOUT, async {
            let raw = connection.open_raw_sftp().await.ok()?;
            let version = raw.init().await.ok()?;
            let has_posix_rename = version
                .extensions
                .get(POSIX_RENAME)
                .is_some_and(|v| v == "1");
            Some((raw, has_posix_rename))
        })
        .await
        .ok()
        .flatten()
    } else {
        None
    };
    if let Some((raw, has_posix_rename)) = raw {
        if raw.realpath(parent).await.is_ok_and(|name| {
            name.files
                .first()
                .is_some_and(|file| file.filename == parent)
        }) {
            let marker = NamespaceMarker {
                path: canonical_child(directory, "namespace"),
                nonce: uuid::Uuid::new_v4().simple().to_string(),
            };
            let mut marker_created = false;
            let setup = async {
                raw.mkdir(directory, attrs_with_mode(STAGING_DIR_MODE))
                    .await
                    .map_err(|err| sftp_err("create private remote staging directory", err))?;
                let outcome = async {
                    let mode = raw
                        .lstat(directory)
                        .await
                        .map_err(|err| sftp_err("stat raw remote staging directory", err))?
                        .attrs
                        .permissions;
                    if !mode
                        .is_some_and(|mode| mode & TYPE_MASK == DIRECTORY_TYPE && mode & 0o077 == 0)
                    {
                        return Err(TransferError::InvalidInput(
                            "raw remote staging directory is not private".into(),
                        ));
                    }
                    let handle = raw
                        .open(
                            &marker.path,
                            EXCLUSIVE_WRITE,
                            attrs_with_mode(PRIVATE_FILE_MODE),
                        )
                        .await
                        .map_err(|err| sftp_err("create remote namespace marker", err))?
                        .handle;
                    marker_created = true;
                    let written = raw
                        .write(&handle, 0, marker.nonce.as_bytes().to_vec())
                        .await;
                    let closed = raw.close(handle).await;
                    written.map_err(|err| sftp_err("write remote namespace marker", err))?;
                    closed.map_err(|err| sftp_err("close remote namespace marker", err))?;
                    if !sftp_sees_marker(sftp, &marker).await {
                        return Err(TransferError::InvalidInput(
                            "SFTP channels do not see the same staging namespace".into(),
                        ));
                    }
                    enforce_private_directory(sftp, directory).await
                }
                .await;
                if outcome.is_err() {
                    let _ = tokio::time::timeout(Duration::from_secs(2), async {
                        if marker_created {
                            let _ = raw.remove(&marker.path).await;
                        }
                        let _ = raw.rmdir(directory).await;
                    })
                    .await;
                }
                outcome
            }
            .await;
            setup?;
            if replacing {
                return Ok((Some(marker), has_posix_rename.then_some(raw)));
            }
            if let Err(err) = sftp.remove_file(&marker.path).await {
                cleanup_stage(sftp, directory, None, Some(&marker)).await;
                return Err(sftp_err("remove remote namespace marker", err));
            }
            return Ok((None, None));
        }
    }

    // Without the raw channel the normal MKDIR has no mode argument. It is
    // never safe to put data in a dir others could have written before chmod.
    // Reject that case without opening a file; for common 0755/0750 defaults,
    // narrow the empty, non-writable directory before creating the payload.
    sftp.create_dir(directory)
        .await
        .map_err(|err| sftp_err("create remote staging directory", err))?;
    if let Err(err) = enforce_private_directory(sftp, directory).await {
        cleanup_stage(sftp, directory, None, None).await;
        return Err(err);
    }
    let marker = if connection.is_some() && replacing {
        let marker = NamespaceMarker {
            path: canonical_child(directory, "namespace"),
            nonce: uuid::Uuid::new_v4().simple().to_string(),
        };
        if let Err(err) = write_marker(sftp, &marker).await {
            cleanup_stage(sftp, directory, None, Some(&marker)).await;
            return Err(err);
        }
        Some(marker)
    } else {
        None
    };
    Ok((marker, None))
}

async fn enforce_private_directory(
    sftp: &SftpSession,
    directory: &str,
) -> Result<(), TransferError> {
    let mode = sftp
        .symlink_metadata(directory)
        .await
        .map_err(|err| sftp_err("stat remote staging directory", err))?
        .permissions;
    // Keep inherited setgid so group inheritance continues to follow the
    // original parent; no group/other access is allowed inside this directory.
    let Some(mode) = mode.filter(|mode| mode & TYPE_MASK == DIRECTORY_TYPE && mode & 0o022 == 0)
    else {
        return Err(TransferError::InvalidInput(
            "remote staging directory is not private and safe to use".into(),
        ));
    };
    let private = STAGING_DIR_MODE | (mode & 0o2000);
    if mode & 0o7777 != private {
        sftp.set_metadata(directory, attrs_with_mode(private))
            .await
            .map_err(|err| sftp_err("set private remote staging permissions", err))?;
        let actual = sftp
            .symlink_metadata(directory)
            .await
            .map_err(|err| sftp_err("verify private remote staging permissions", err))?;
        if actual.permissions.map(|mode| mode & (TYPE_MASK | 0o7777))
            != Some(DIRECTORY_TYPE | private)
        {
            return Err(TransferError::InvalidInput(
                "remote staging directory permissions could not be secured".into(),
            ));
        }
    }
    Ok(())
}

async fn write_marker(sftp: &SftpSession, marker: &NamespaceMarker) -> Result<(), TransferError> {
    let mut file = sftp
        .open_with_flags_and_attributes(
            &marker.path,
            EXCLUSIVE_WRITE,
            attrs_with_mode(PRIVATE_FILE_MODE),
        )
        .await
        .map_err(|err| sftp_err("create remote namespace marker", err))?;
    file.write_all(marker.nonce.as_bytes())
        .await
        .map_err(|err| remote_io_err("write remote namespace marker", err))?;
    file.shutdown()
        .await
        .map_err(|err| remote_io_err("close remote namespace marker", err))
}

async fn sftp_sees_marker(sftp: &SftpSession, marker: &NamespaceMarker) -> bool {
    let Ok(file) = sftp.open(&marker.path).await else {
        return false;
    };
    let mut seen = Vec::new();
    let mut file = file.take(marker.nonce.len() as u64 + 1);
    let matched = file.read_to_end(&mut seen).await.is_ok() && seen == marker.nonce.as_bytes();
    let _ = file.into_inner().shutdown().await;
    matched
}

async fn raw_sees_marker(raw: &RawSftpSession, marker: &NamespaceMarker) -> bool {
    let Ok(handle) = raw
        .open(&marker.path, OpenFlags::READ, FileAttributes::empty())
        .await
    else {
        return false;
    };
    let length = marker.nonce.len() as u32;
    let result = raw.read(&handle.handle, 0, length + 1).await;
    let matched = match result {
        Ok(data) if data.data == marker.nonce.as_bytes() => matches!(
            raw.read(&handle.handle, u64::from(length), 1).await,
            Err(SftpError::Status(status)) if status.status_code == StatusCode::Eof
        ),
        _ => false,
    };
    let _ = raw.close(handle.handle).await;
    matched
}

async fn cleanup_stage(
    sftp: &SftpSession,
    directory: &str,
    payload: Option<&str>,
    marker: Option<&NamespaceMarker>,
) {
    let _ = tokio::time::timeout(Duration::from_secs(2), async {
        if let Some(path) = payload {
            let _ = sftp.remove_file(path).await;
        }
        if let Some(marker) = marker {
            let _ = sftp.remove_file(&marker.path).await;
        }
        let _ = sftp.remove_dir(directory).await;
    })
    .await;
}

fn no_safe_replacement() -> TransferError {
    TransferError::Sftp("remote server does not support safe atomic file replacement".into())
}

fn publication_unknown(reason: impl std::fmt::Display) -> TransferError {
    // Deliberately not SftpTransport: an operation accepted by the server must
    // never be automatically replayed because its reply was lost.
    TransferError::Sftp(format!("remote publication outcome unknown: {reason}"))
}

fn is_uncertain_sftp_result(err: &SftpError) -> bool {
    crate::is_sftp_transport_error(err)
        || matches!(err, SftpError::Status(status)
        if matches!(status.status_code, StatusCode::NoConnection | StatusCode::ConnectionLost))
}

enum PosixRenameResult {
    Replaced,
    Unsupported,
    Rejected(String),
    Unknown(String),
}

fn posix_rename_payload(staging: &str, destination: &str) -> Result<Vec<u8>, TransferError> {
    let mut data = Vec::new();
    for path in [staging, destination] {
        let len = u32::try_from(path.len()).map_err(|_| {
            TransferError::InvalidInput("remote path exceeds SFTP maximum length".into())
        })?;
        data.extend_from_slice(&len.to_be_bytes());
        data.extend_from_slice(path.as_bytes());
    }
    Ok(data)
}

async fn posix_rename(raw: &RawSftpSession, staging: &str, destination: &str) -> PosixRenameResult {
    let data = match posix_rename_payload(staging, destination) {
        Ok(data) => data,
        Err(err) => return PosixRenameResult::Rejected(err.to_string()),
    };
    match raw.extended(POSIX_RENAME, data).await {
        Ok(Packet::Status(status)) => match status.status_code {
            StatusCode::Ok => PosixRenameResult::Replaced,
            StatusCode::OpUnsupported => PosixRenameResult::Unsupported,
            StatusCode::ConnectionLost | StatusCode::NoConnection => {
                PosixRenameResult::Unknown(status.error_message)
            }
            _ => PosixRenameResult::Rejected(format!(
                "remote replacement rejected: {} ({})",
                status.status_code, status.error_message
            )),
        },
        Ok(_) => PosixRenameResult::Unknown("unexpected SFTP replacement response".into()),
        Err(err) if is_uncertain_sftp_result(&err) => PosixRenameResult::Unknown(err.to_string()),
        Err(err) => PosixRenameResult::Rejected(err.to_string()),
    }
}

async fn shell_has_exact_mv(connection: &SshConnection, directory: &str) -> bool {
    // Probe semantics rather than GNU's --help. All probe files are inside the
    // private staging directory. Require directory refusal (not "move inside")
    // and actual replacement of an existing regular file with the same flags.
    let a = shell_quote(&canonical_child(directory, "mv-source"));
    let b = shell_quote(&canonical_child(directory, "mv-target"));
    let d = shell_quote(&canonical_child(directory, "mv-directory"));
    let script = format!(
        "(umask 077; set -C; printf source > {a} && printf target > {b} && mkdir {d}) || exit 1; \
         mv_status=0; command mv -fT -- {a} {d} 2>/dev/null || mv_status=$?; \
         replaced=1; [ \"$mv_status\" -ne 0 ] && [ -f {a} ] && [ -d {d} ] && \
         command mv -fT -- {a} {b} && [ ! -e {a} ] && [ \"$(cat {b})\" = source ] || replaced=0; \
         command rm -f -- {a} {b} {d}/mv-source; command rmdir {d} 2>/dev/null; [ \"$replaced\" = 1 ]"
    );
    connection
        .exec_command_timeout(&script, SSH_PROBE_TIMEOUT)
        .await
        .is_ok_and(|output| output.exit_code == Some(0))
}

#[cfg(test)]
#[path = "sftp_staging_tests.rs"]
mod staging_tests;

fn verify_length(expected: Option<u64>, actual: u64) -> Result<(), TransferError> {
    if let Some(expected) = expected {
        if actual != expected {
            return Err(TransferError::InvalidInput(format!(
                "transfer size changed: expected {expected} bytes, read {actual} bytes"
            )));
        }
    }
    Ok(())
}

fn conclude_attempt(store: &TransferProgressStore, id: &str, result: &Result<(), TransferError>) {
    match result {
        Ok(()) => store.finish(id, None),
        Err(err) if err.is_transport_error() => store.release_for_retry(id),
        Err(err) => store.finish(id, Some(err.to_string())),
    }
}

pub async fn upload_file_resumable(
    sftp: &SftpSession,
    local_path: &Path,
    remote_path: &str,
    transfer_id: &str,
    resume: bool,
    store: &TransferProgressStore,
) -> Result<(), TransferError> {
    let result =
        upload_file_resumable_once(sftp, local_path, remote_path, transfer_id, resume, store).await;
    if let Err(err) = &result {
        if err.is_transport_error() {
            store.finish(transfer_id, Some(err.to_string()));
        }
    }
    result
}

pub async fn upload_file_resumable_once(
    sftp: &SftpSession,
    local_path: &Path,
    remote_path: &str,
    transfer_id: &str,
    _resume: bool,
    store: &TransferProgressStore,
) -> Result<(), TransferError> {
    upload_file_once(sftp, None, local_path, remote_path, transfer_id, store).await
}

pub async fn upload_file_once(
    sftp: &SftpSession,
    ssh: Option<&SshConnection>,
    local_path: &Path,
    remote_path: &str,
    transfer_id: &str,
    store: &TransferProgressStore,
) -> Result<(), TransferError> {
    let mut source = tokio::fs::File::open(local_path).await?;
    let before = source.metadata().await?;
    if !before.is_file() {
        return Err(TransferError::InvalidInput(
            "upload source must be a regular file".into(),
        ));
    }
    let total = before.len();
    // Displayed progress is not a durable checkpoint. Each attempt has a new file.
    store.begin(transfer_id, total, false)?;
    let result = async {
        store.wait_if_paused(transfer_id).await?;
        let mut staged = match ssh {
            Some(connection) => {
                RemoteStagedFile::create_with_ssh(sftp, remote_path, connection).await?
            }
            None => RemoteStagedFile::create(sftp, remote_path).await?,
        };
        let result = async {
            let mut buf = vec![0u8; CHUNK_SIZE];
            let mut count = 0;
            loop {
                store.wait_if_paused(transfer_id).await?;
                let n = source.read(&mut buf).await?;
                if n == 0 {
                    break;
                }
                staged
                    .file_mut()
                    .write_all(&buf[..n])
                    .await
                    .map_err(|err| remote_io_err("write remote staging file", err))?;
                count += n as u64;
                store.add_loaded(transfer_id, n as u64);
            }
            verify_length(Some(total), count)?;
            staged.verify_size(count).await?;
            let after = source.metadata().await?;
            if after.len() != total || after.modified().ok() != before.modified().ok() {
                return Err(TransferError::InvalidInput(
                    "upload source changed during transfer".into(),
                ));
            }
            store.begin_commit(transfer_id).await?;
            staged.publish(ssh).await
        }
        .await;
        if result.is_err() {
            staged.cleanup().await;
        }
        result
    }
    .await;
    conclude_attempt(store, transfer_id, &result);
    result
}

pub async fn download_file_resumable(
    sftp: &SftpSession,
    remote_path: &str,
    local_path: &Path,
    transfer_id: &str,
    resume: bool,
    store: &TransferProgressStore,
) -> Result<(), TransferError> {
    let result =
        download_file_resumable_once(sftp, remote_path, local_path, transfer_id, resume, store)
            .await;
    if let Err(err) = &result {
        if err.is_transport_error() {
            store.finish(transfer_id, Some(err.to_string()));
        }
    }
    result
}

pub async fn download_file_resumable_once(
    sftp: &SftpSession,
    remote_path: &str,
    local_path: &Path,
    transfer_id: &str,
    _resume: bool,
    store: &TransferProgressStore,
) -> Result<(), TransferError> {
    store.begin(transfer_id, 0, false)?;
    let result =
        download_file_attempt(sftp, remote_path, local_path, transfer_id, store, true).await;
    conclude_attempt(store, transfer_id, &result);
    result
}

/// Runs one attempt for a caller-owned active progress entry. Each attempt uses a new
/// sibling staging file. Only the caller may finish the entry, after all retries.
pub async fn download_file_attempt(
    sftp: &SftpSession,
    remote_path: &str,
    local_path: &Path,
    transfer_id: &str,
    store: &TransferProgressStore,
    overwrite: bool,
) -> Result<(), TransferError> {
    store.wait_if_paused(transfer_id).await?;
    let mut source = download_remote_wait(store, transfer_id, async {
        sftp.open(remote_path)
            .await
            .map_err(|err| sftp_err("open download source", err))
    })
    .await?;
    let before = download_remote_wait(store, transfer_id, async {
        source
            .metadata()
            .await
            .map_err(|err| sftp_err("stat download source", err))
    })
    .await?;
    if before.is_dir() || before.permissions.is_some() && !before.is_regular() {
        return Err(TransferError::InvalidInput(
            "download source must be a regular file".into(),
        ));
    }
    store.reset_bytes(transfer_id, before.size.unwrap_or(0));
    let parent = local_path
        .parent()
        .filter(|path| !path.as_os_str().is_empty())
        .unwrap_or(Path::new("."));
    tokio::fs::create_dir_all(parent).await?;
    let (file, staged) = tempfile::Builder::new()
        .prefix(".chemssh-transfer-")
        .suffix(".part")
        .tempfile_in(parent)?
        .into_parts();
    let mut local_file = tokio::fs::File::from_std(file);
    let mut buf = vec![0u8; CHUNK_SIZE];
    let mut count = 0;
    loop {
        store.wait_if_paused(transfer_id).await?;
        let n = download_remote_wait(store, transfer_id, async {
            source
                .read(&mut buf)
                .await
                .map_err(|err| remote_io_err("read download source", err))
        })
        .await?;
        if n == 0 {
            break;
        }
        local_file.write_all(&buf[..n]).await?;
        count += n as u64;
        store.add_loaded(transfer_id, n as u64);
    }
    verify_length(before.size, count)?;
    local_file.flush().await?;
    local_file.sync_all().await?;
    verify_length(Some(count), local_file.metadata().await?.len())?;
    drop(local_file);
    download_remote_wait(store, transfer_id, verify_remote_source(&source, &before)).await?;
    drop(source);
    store.begin_commit(transfer_id).await?;
    publish_local_download(staged, local_path, overwrite)
}

async fn download_remote_wait<T>(
    store: &TransferProgressStore,
    id: &str,
    operation: impl std::future::Future<Output = Result<T, TransferError>>,
) -> Result<T, TransferError> {
    tokio::select! {
        biased;
        _ = store.wait_for_cancel(id) => Err(TransferError::Cancelled),
        result = tokio::time::timeout(Duration::from_secs(60), operation) => {
            result.map_err(|_| TransferError::SftpTransport("remote download I/O timed out".into()))?
        }
    }
}

fn publish_local_download(
    staged: tempfile::TempPath,
    local_path: &Path,
    overwrite: bool,
) -> Result<(), TransferError> {
    let result = if overwrite {
        staged.persist(local_path)
    } else {
        staged.persist_noclobber(local_path)
    };
    result.map_err(|err| err.error.into())
}

#[cfg(test)]
mod download_tests {
    use super::*;

    #[test]
    fn no_clobber_keeps_file_created_after_destination_selection() {
        let dir = tempfile::tempdir().unwrap();
        let target = dir.path().join("result.txt");
        std::fs::write(&target, b"previous").unwrap();
        let staged = tempfile::NamedTempFile::new_in(dir.path())
            .unwrap()
            .into_temp_path();
        std::fs::write(&staged, b"downloaded").unwrap();
        let staged_path = staged.to_path_buf();
        assert!(publish_local_download(staged, &target, false).is_err());
        assert_eq!(std::fs::read(target).unwrap(), b"previous");
        assert!(!staged_path.exists());
    }

    #[tokio::test]
    async fn cancellation_unblocks_pending_remote_io() {
        let store = TransferProgressStore::new();
        store.begin("download", 0, false).unwrap();
        let wait = download_remote_wait::<()>(&store, "download", std::future::pending());
        let cancel = async {
            tokio::task::yield_now().await;
            store.cancel("download", None).unwrap();
        };
        let (result, ()) = tokio::join!(wait, cancel);
        assert!(matches!(result, Err(TransferError::Cancelled)));
    }
}

async fn verify_remote_source(file: &File, before: &FileAttributes) -> Result<(), TransferError> {
    let after = file
        .metadata()
        .await
        .map_err(|err| sftp_err("recheck remote source", err))?;
    if before.size != after.size || before.mtime != after.mtime {
        return Err(TransferError::InvalidInput(
            "remote source changed during transfer".into(),
        ));
    }
    Ok(())
}

pub async fn copy_remote_to_remote(
    src_sftp: &SftpSession,
    src_path: &str,
    dst_sftp: &SftpSession,
    dst_path: &str,
    transfer_id: &str,
    store: &TransferProgressStore,
) -> Result<(), TransferError> {
    let result =
        copy_remote_to_remote_once(src_sftp, src_path, dst_sftp, dst_path, transfer_id, store)
            .await;
    if let Err(err) = &result {
        if err.is_transport_error() {
            store.finish(transfer_id, Some(err.to_string()));
        }
    }
    result
}

pub async fn copy_remote_to_remote_once(
    src_sftp: &SftpSession,
    src_path: &str,
    dst_sftp: &SftpSession,
    dst_path: &str,
    transfer_id: &str,
    store: &TransferProgressStore,
) -> Result<(), TransferError> {
    copy_remote_file_once(
        src_sftp,
        src_path,
        dst_sftp,
        None,
        dst_path,
        transfer_id,
        store,
    )
    .await
}

pub async fn copy_remote_file_once(
    src_sftp: &SftpSession,
    src_path: &str,
    dst_sftp: &SftpSession,
    dst_ssh: Option<&SshConnection>,
    dst_path: &str,
    transfer_id: &str,
    store: &TransferProgressStore,
) -> Result<(), TransferError> {
    let mut source = src_sftp
        .open(src_path)
        .await
        .map_err(|err| sftp_err("open copy source", err))?;
    let before = source
        .metadata()
        .await
        .map_err(|err| sftp_err("stat copy source", err))?;
    store.begin(transfer_id, before.size.unwrap_or(0), false)?;
    let result = async {
        store.wait_if_paused(transfer_id).await?;
        let mut staged = RemoteStagedFile::create_inner(
            dst_sftp,
            dst_path,
            before.permissions.unwrap_or(DEFAULT_NEW_FILE_MODE),
            None,
            dst_ssh,
        )
        .await?;
        let result = async {
            let mut buf = vec![0u8; CHUNK_SIZE];
            let mut count = 0;
            loop {
                store.wait_if_paused(transfer_id).await?;
                let n = source
                    .read(&mut buf)
                    .await
                    .map_err(|err| remote_io_err("read copy source", err))?;
                if n == 0 {
                    break;
                }
                staged
                    .file_mut()
                    .write_all(&buf[..n])
                    .await
                    .map_err(|err| remote_io_err("write remote staging file", err))?;
                count += n as u64;
                store.add_loaded(transfer_id, n as u64);
            }
            verify_length(before.size, count)?;
            staged.verify_size(count).await?;
            verify_remote_source(&source, &before).await?;
            store.begin_commit(transfer_id).await?;
            staged.publish(dst_ssh).await
        }
        .await;
        if result.is_err() {
            staged.cleanup().await;
        }
        result
    }
    .await;
    conclude_attempt(store, transfer_id, &result);
    result
}
