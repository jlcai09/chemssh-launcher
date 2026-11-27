pub mod archive;
pub mod local_fs;
pub mod progress;
mod sftp_namespace;
pub mod sftp_ops;

use thiserror::Error;

pub use archive::{
    archive_stem, build_compress_step, build_extract_step, detect_local_tools, detect_remote_tools,
    next_archive_name, run_local_step, run_remote_step, ArchiveStep, ArchiveTools, ARCHIVE_PREFIX,
    FORMAT_SEVEN_ZIP, FORMAT_TAR, FORMAT_TAR_GZ, FORMAT_TAR_XZ, FORMAT_TAR_ZST, FORMAT_ZIP,
};
pub use local_fs::{
    list_local_dir, local_copy, local_create_file, local_delete, local_home, local_mkdir,
    local_rename, FsEntry,
};
pub use progress::{TransferProgress, TransferProgressStore};
pub use sftp_namespace::sftp_shell_sees_marker;
pub use sftp_ops::{
    copy_remote_file_once, copy_remote_to_remote, copy_remote_to_remote_once, delete_remote_path,
    download_file_attempt, download_file_resumable, download_file_resumable_once, list_remote_dir,
    upload_file_once,
    upload_file_resumable, upload_file_resumable_once, RemoteStagedFile,
};

#[derive(Debug, Error)]
pub enum TransferError {
    #[error("transfer already running: {0}")]
    AlreadyRunning(String),
    #[error("transfer not found: {0}")]
    NotFound(String),
    #[error("transfer cancelled")]
    Cancelled,
    #[error("invalid input: {0}")]
    InvalidInput(String),
    #[error("sftp error: {0}")]
    Sftp(String),
    #[error("sftp error: {0}")]
    SftpTransport(String),
    #[error("archive error: {0}")]
    Archive(String),
    #[error("internal error: {0}")]
    Internal(String),
    #[error("io error: {0}")]
    Io(#[from] std::io::Error),
}

impl TransferError {
    pub fn is_transport_error(&self) -> bool {
        matches!(self, Self::SftpTransport(_))
    }
}

/// Reports whether a `russh-sftp` error reflects a broken/timed-out SSH transport
/// (recoverable via reconnect) rather than a server-side SFTP status rejection.
pub fn is_sftp_transport_error(err: &russh_sftp::client::error::Error) -> bool {
    !matches!(
        err,
        russh_sftp::client::error::Error::Status(_) | russh_sftp::client::error::Error::Limited(_)
    )
}

pub fn sftp_err(
    context: impl std::fmt::Display,
    err: russh_sftp::client::error::Error,
) -> TransferError {
    let msg = format!("{context}: {err}");
    if is_sftp_transport_error(&err) {
        TransferError::SftpTransport(msg)
    } else {
        TransferError::Sftp(msg)
    }
}

/// Maps `std::io::Error` from remote `russh-sftp` file operations into transport vs status errors.
/// Only use on remote `SftpRemoteFile` handles, never on local filesystem handles.
pub fn remote_io_err(context: impl std::fmt::Display, err: std::io::Error) -> TransferError {
    if let Some(sftp_err_ref) = err
        .get_ref()
        .and_then(|inner| inner.downcast_ref::<russh_sftp::client::error::Error>())
    {
        let msg = format!("{context}: {err}");
        if is_sftp_transport_error(sftp_err_ref) {
            return TransferError::SftpTransport(msg);
        }
        return TransferError::Sftp(msg);
    }

    let is_transport_kind = matches!(
        err.kind(),
        std::io::ErrorKind::BrokenPipe
            | std::io::ErrorKind::ConnectionReset
            | std::io::ErrorKind::ConnectionAborted
            | std::io::ErrorKind::NotConnected
            | std::io::ErrorKind::UnexpectedEof
            | std::io::ErrorKind::TimedOut
    );
    let msg = format!("{context}: {err}");
    if is_transport_kind {
        TransferError::SftpTransport(msg)
    } else {
        TransferError::Sftp(msg)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Arc;
    use tempfile::tempdir;

    #[test]
    fn resumable_progress_preserves_offset_and_blocks_cancelled_resume() {
        let store = TransferProgressStore::new();
        assert_eq!(store.begin("t1", 1000, false).unwrap(), 0);
        store.add_loaded("t1", 400);
        store.finish("t1", Some("network drop".into()));

        // Resume starts from 400.
        assert_eq!(store.begin("t1", 1000, true).unwrap(), 400);
        store.cancel("t1", Some("user cancelled")).unwrap();

        store.finish("t1", None);
        // Cancelled transfer cannot resume.
        assert!(matches!(
            store.begin("t1", 1000, true),
            Err(TransferError::Cancelled)
        ));
    }

    #[tokio::test]
    async fn pause_and_resume_unblocks_waiting_worker() {
        let store = Arc::new(TransferProgressStore::new());
        store.begin("t2", 500, false).unwrap();
        store.pause("t2").unwrap();

        let store_bg = Arc::clone(&store);
        tokio::spawn(async move {
            tokio::time::sleep(std::time::Duration::from_millis(30)).await;
            store_bg.resume("t2").unwrap();
        });

        store.wait_if_paused("t2").await.unwrap();
        assert!(!store.get("t2").unwrap().paused);
    }

    #[test]
    fn archive_formats_and_plans() {
        let tools = ArchiveTools {
            tar: true,
            zstd: true,
            gzip: true,
            xz: false,
            zip: true,
            unzip: true,
            seven_zip: false,
        };
        let formats = tools.supported_formats();
        assert_eq!(
            formats,
            vec![FORMAT_TAR_ZST, FORMAT_TAR_GZ, FORMAT_TAR, FORMAT_ZIP]
        );

        let step = build_compress_step(FORMAT_TAR_GZ, "out.tar.gz", &["a.txt".into()]).unwrap();
        assert_eq!(step.argv, vec!["tar", "-czf", "out.tar.gz", "a.txt"]);
        assert!(step.pipe_to.is_none());

        let zst_comp = build_compress_step(FORMAT_TAR_ZST, "old.tar.zst", &["old".into()]).unwrap();
        assert_eq!(zst_comp.argv, vec!["tar", "-cf", "-", "old"]);
        assert_eq!(
            zst_comp.pipe_to.as_deref(),
            Some(&["zstd", "-1", "-T4", "-f", "-o", "old.tar.zst"].map(String::from)[..])
        );

        let ext = build_extract_step("out.tar.zst", "dest").unwrap();
        assert_eq!(ext.argv, vec!["zstd", "-dc", "out.tar.zst"]);
        assert_eq!(
            ext.pipe_to.as_deref(),
            Some(&["tar", "-xf", "-", "-C", "dest"].map(String::from)[..])
        );
    }

    #[test]
    fn next_archive_name_avoids_overwriting_existing_archives() {
        let single = vec!["notes.txt".to_string()];
        let multi = vec!["a.txt".to_string(), "b.txt".to_string()];

        assert_eq!(archive_stem(&single), "notes.txt");
        assert_eq!(archive_stem(&multi), "archive");
        assert_eq!(archive_stem(&["folder/".to_string()]), "folder");

        // Multi-select uses archive.<format> and appends _new on every collision.
        let mut taken = std::collections::HashSet::new();
        let first =
            next_archive_name(FORMAT_TAR_ZST, &multi, None, |name| taken.contains(name)).unwrap();
        assert_eq!(first, "archive.tar.zst");
        taken.insert(first);

        let second =
            next_archive_name(FORMAT_TAR_ZST, &multi, None, |name| taken.contains(name)).unwrap();
        assert_eq!(second, "archive_new.tar.zst");
        taken.insert(second);

        let third =
            next_archive_name(FORMAT_TAR_ZST, &multi, None, |name| taken.contains(name)).unwrap();
        assert_eq!(third, "archive_new_new.tar.zst");

        // Explicit archive_name also appends _new before .<format> when taken.
        let explicit = next_archive_name(
            FORMAT_TAR_ZST,
            &multi,
            Some("archive.tar.zst"),
            |name| taken.contains(name),
        )
        .unwrap();
        assert_eq!(explicit, "archive_new_new.tar.zst");

        // Single-entry keeps original name and appends _new before .<format> when taken.
        let mut single_taken = std::collections::HashSet::from(["notes.txt.tar.zst".to_string()]);
        let single_next = next_archive_name(FORMAT_TAR_ZST, &single, None, |name| {
            single_taken.contains(name)
        })
        .unwrap();
        assert_eq!(single_next, "notes.txt_new.tar.zst");
        single_taken.insert(single_next);
        assert_eq!(
            next_archive_name(FORMAT_TAR_ZST, &single, None, |name| {
                single_taken.contains(name)
            })
            .unwrap(),
            "notes.txt_new_new.tar.zst"
        );

        assert!(next_archive_name("rar", &multi, None, |_| false).is_err());
    }

    #[test]
    fn local_fs_crud_and_copy() {
        let dir = tempdir().unwrap();
        let sub = dir.path().join("folder");
        local_mkdir(&sub).unwrap();
        let f1 = sub.join("hello.txt");
        local_create_file(&f1).unwrap();
        std::fs::write(&f1, b"abc").unwrap();

        let f2 = sub.join("renamed.txt");
        local_rename(&f1, &f2).unwrap();

        let copy_dst = dir.path().join("folder_copy");
        local_copy(&sub, &copy_dst).unwrap();
        assert_eq!(std::fs::read(copy_dst.join("renamed.txt")).unwrap(), b"abc");

        let list = list_local_dir(dir.path()).unwrap();
        assert_eq!(list.len(), 2);
        assert!(list.iter().all(|e| e.is_dir));

        local_delete(&copy_dst).unwrap();
        assert_eq!(list_local_dir(dir.path()).unwrap().len(), 1);
    }

    #[test]
    fn sftp_error_classification_distinguishes_transport_from_status() {
        use russh_sftp::client::error::Error as SftpClientErr;
        use russh_sftp::protocol::{Status, StatusCode};

        let status_err = SftpClientErr::Status(Status {
            id: 1,
            status_code: StatusCode::PermissionDenied,
            error_message: "permission denied".into(),
            language_tag: String::new(),
        });
        assert!(!is_sftp_transport_error(&status_err));
        assert!(!sftp_err("stat /home/user/a", status_err.clone()).is_transport_error());

        let closed_err = SftpClientErr::UnexpectedBehavior("session closed".into());
        assert!(is_sftp_transport_error(&closed_err));
        assert!(sftp_err("list /home/user", closed_err.clone()).is_transport_error());

        assert!(sftp_err("read", SftpClientErr::Timeout).is_transport_error());
        assert!(!sftp_err("read", SftpClientErr::Limited("too large".into())).is_transport_error());

        let wrapped_closed = std::io::Error::from(closed_err);
        assert!(remote_io_err("write", wrapped_closed).is_transport_error());

        let status_io = std::io::Error::other("permission denied");
        assert!(!remote_io_err("write", status_io).is_transport_error());

        let broken_pipe = std::io::Error::new(std::io::ErrorKind::BrokenPipe, "broken pipe");
        assert!(remote_io_err("write", broken_pipe).is_transport_error());
    }

    #[test]
    fn release_for_retry_preserves_offset_without_marking_done() {
        let store = TransferProgressStore::new();
        assert_eq!(store.begin("t-retry", 1000, false).unwrap(), 0);
        store.add_loaded("t-retry", 350);
        assert_eq!(store.active_count(), 1);

        store.release_for_retry("t-retry");
        let snap = store.get("t-retry").unwrap();
        assert!(!snap.done);
        assert!(!snap.cancelled);
        assert!(snap.error.is_empty());
        assert_eq!(snap.loaded, 350);
        assert_eq!(store.active_count(), 0);

        // Next resume attempt reclaims from 350.
        assert_eq!(store.begin("t-retry", 1000, true).unwrap(), 350);
        assert!(!store.is_cancelled("t-retry"));
    }
}
