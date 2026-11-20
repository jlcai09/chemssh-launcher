pub mod archive;
pub mod local_fs;
pub mod progress;
pub mod sftp_ops;

use thiserror::Error;

pub use archive::{
    build_compress_step, build_extract_step, detect_local_tools, detect_remote_tools,
    run_local_step, run_remote_step, ArchiveStep, ArchiveTools, FORMAT_SEVEN_ZIP, FORMAT_TAR,
    FORMAT_TAR_GZ, FORMAT_TAR_XZ, FORMAT_TAR_ZST, FORMAT_ZIP,
};
pub use local_fs::{
    list_local_dir, local_copy, local_create_file, local_delete, local_home, local_mkdir,
    local_rename, FsEntry,
};
pub use progress::{TransferProgress, TransferProgressStore};
pub use sftp_ops::{
    copy_remote_to_remote, download_file_resumable, list_remote_dir, upload_file_resumable,
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
    #[error("archive error: {0}")]
    Archive(String),
    #[error("internal error: {0}")]
    Internal(String),
    #[error("io error: {0}")]
    Io(#[from] std::io::Error),
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
        assert_eq!(formats, vec![FORMAT_TAR_ZST, FORMAT_TAR_GZ, FORMAT_TAR, FORMAT_ZIP]);

        let step = build_compress_step(FORMAT_TAR_GZ, "out.tar.gz", &["a.txt".into()]).unwrap();
        assert_eq!(step.argv, vec!["tar", "-czf", "out.tar.gz", "a.txt"]);

        let ext = build_extract_step("out.tar.zst", "dest").unwrap();
        assert_eq!(ext.argv, vec!["tar", "--zstd", "-xf", "out.tar.zst", "-C", "dest"]);
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
}
