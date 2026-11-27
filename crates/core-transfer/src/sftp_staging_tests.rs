use super::*;
use russh_sftp::protocol::{Attrs, Data, File as WireFile, Handle, Name, Status};
use russh_sftp::server::{self, Handler};
use std::collections::HashMap;
use std::sync::{Arc, Mutex};

#[derive(Clone)]
struct Entry {
    mode: u32,
    bytes: Vec<u8>,
}

type Entries = Arc<Mutex<HashMap<String, Entry>>>;

struct FakeServer {
    entries: Entries,
    creation_mask: u32,
    allow_overwrite: bool,
    allow_chmod: bool,
    extension_response: StatusCode,
}

fn status(id: u32) -> Status {
    Status {
        id,
        status_code: StatusCode::Ok,
        error_message: String::new(),
        language_tag: String::new(),
    }
}

impl Handler for FakeServer {
    type Error = StatusCode;

    fn unimplemented(&self) -> Self::Error {
        StatusCode::OpUnsupported
    }

    async fn realpath(&mut self, id: u32, path: String) -> Result<Name, Self::Error> {
        Ok(Name {
            id,
            files: vec![WireFile::dummy(if path == "." {
                "/remote".into()
            } else {
                path
            })],
        })
    }

    async fn mkdir(
        &mut self,
        id: u32,
        path: String,
        attrs: FileAttributes,
    ) -> Result<Status, Self::Error> {
        let mut entries = self.entries.lock().unwrap();
        if entries.contains_key(&path) {
            return Err(StatusCode::Failure);
        }
        entries.insert(
            path,
            Entry {
                mode: DIRECTORY_TYPE | (attrs.permissions.unwrap_or(0o777) & !self.creation_mask),
                bytes: vec![],
            },
        );
        Ok(status(id))
    }

    async fn rmdir(&mut self, id: u32, path: String) -> Result<Status, Self::Error> {
        let mut entries = self.entries.lock().unwrap();
        if entries
            .keys()
            .any(|key| key.starts_with(&format!("{path}/")))
        {
            return Err(StatusCode::Failure);
        }
        entries.remove(&path).ok_or(StatusCode::NoSuchFile)?;
        Ok(status(id))
    }

    async fn open(
        &mut self,
        id: u32,
        filename: String,
        flags: OpenFlags,
        attrs: FileAttributes,
    ) -> Result<Handle, Self::Error> {
        let mut entries = self.entries.lock().unwrap();
        if flags.contains(EXCLUSIVE_WRITE) {
            if entries.contains_key(&filename) {
                return Err(StatusCode::Failure);
            }
            let parent = filename.rsplit_once('/').unwrap().0;
            assert_eq!(entries[parent].mode & 0o777, STAGING_DIR_MODE);
            entries.insert(
                filename.clone(),
                Entry {
                    mode: REGULAR_TYPE | (attrs.permissions.unwrap_or(0o666) & !self.creation_mask),
                    bytes: vec![],
                },
            );
        } else if !entries.contains_key(&filename) {
            return Err(StatusCode::NoSuchFile);
        }
        Ok(Handle {
            id,
            handle: filename,
        })
    }

    async fn close(&mut self, id: u32, _handle: String) -> Result<Status, Self::Error> {
        Ok(status(id))
    }

    async fn write(
        &mut self,
        id: u32,
        handle: String,
        offset: u64,
        data: Vec<u8>,
    ) -> Result<Status, Self::Error> {
        let mut entries = self.entries.lock().unwrap();
        let entry = entries.get_mut(&handle).ok_or(StatusCode::NoSuchFile)?;
        let end = offset as usize + data.len();
        entry.bytes.resize(entry.bytes.len().max(end), 0);
        entry.bytes[offset as usize..end].copy_from_slice(&data);
        Ok(status(id))
    }

    async fn read(
        &mut self,
        id: u32,
        handle: String,
        offset: u64,
        len: u32,
    ) -> Result<Data, Self::Error> {
        let entries = self.entries.lock().unwrap();
        let entry = entries.get(&handle).ok_or(StatusCode::NoSuchFile)?;
        let offset = offset as usize;
        if offset >= entry.bytes.len() {
            return Err(StatusCode::Eof);
        }
        Ok(Data {
            id,
            data: entry.bytes[offset..(offset + len as usize).min(entry.bytes.len())].to_vec(),
        })
    }

    async fn lstat(&mut self, id: u32, path: String) -> Result<Attrs, Self::Error> {
        let entries = self.entries.lock().unwrap();
        let entry = entries.get(&path).ok_or(StatusCode::NoSuchFile)?;
        let mut attrs = attrs_with_mode(entry.mode);
        attrs.size = Some(entry.bytes.len() as u64);
        Ok(Attrs { id, attrs })
    }

    async fn stat(&mut self, id: u32, path: String) -> Result<Attrs, Self::Error> {
        self.lstat(id, path).await
    }

    async fn setstat(
        &mut self,
        id: u32,
        path: String,
        attrs: FileAttributes,
    ) -> Result<Status, Self::Error> {
        let mut entries = self.entries.lock().unwrap();
        let entry = entries.get_mut(&path).ok_or(StatusCode::NoSuchFile)?;
        if !self.allow_chmod && entry.mode & TYPE_MASK == REGULAR_TYPE {
            return Err(StatusCode::PermissionDenied);
        }
        entry.mode = entry.mode & TYPE_MASK | attrs.permissions.unwrap();
        Ok(status(id))
    }

    async fn rename(
        &mut self,
        id: u32,
        oldpath: String,
        newpath: String,
    ) -> Result<Status, Self::Error> {
        let mut entries = self.entries.lock().unwrap();
        if entries.contains_key(&newpath) && !self.allow_overwrite {
            return Err(StatusCode::Failure);
        }
        let old = entries.remove(&oldpath).ok_or(StatusCode::NoSuchFile)?;
        entries.insert(newpath, old);
        Ok(status(id))
    }

    async fn remove(&mut self, id: u32, filename: String) -> Result<Status, Self::Error> {
        self.entries
            .lock()
            .unwrap()
            .remove(&filename)
            .ok_or(StatusCode::NoSuchFile)?;
        Ok(status(id))
    }

    async fn extended(
        &mut self,
        id: u32,
        request: String,
        mut data: Vec<u8>,
    ) -> Result<Packet, Self::Error> {
        assert_eq!(request, POSIX_RENAME);
        if self.extension_response != StatusCode::Ok {
            return Err(self.extension_response);
        }
        fn path(data: &mut Vec<u8>) -> String {
            let length =
                u32::from_be_bytes(data.drain(..4).as_slice().try_into().unwrap()) as usize;
            String::from_utf8(data.drain(..length).collect()).unwrap()
        }
        let old = path(&mut data);
        let new = path(&mut data);
        assert!(data.is_empty());
        let mut entries = self.entries.lock().unwrap();
        let entry = entries.remove(&old).ok_or(StatusCode::NoSuchFile)?;
        entries.insert(new, entry);
        Ok(status(id).into())
    }
}

async fn setup(mask: u32, allow_overwrite: bool, allow_chmod: bool) -> (SftpSession, Entries) {
    let entries = Entries::default();
    let (client, stream) = tokio::io::duplex(64 * 1024);
    server::run(
        stream,
        FakeServer {
            entries: Arc::clone(&entries),
            creation_mask: mask,
            allow_overwrite,
            allow_chmod,
            extension_response: StatusCode::OpUnsupported,
        },
    )
    .await;
    (SftpSession::new(client).await.unwrap(), entries)
}

fn add(entries: &Entries, path: &str, mode: u32, bytes: &[u8]) {
    entries.lock().unwrap().insert(
        path.into(),
        Entry {
            mode,
            bytes: bytes.into(),
        },
    );
}

async fn payload(staged: &mut RemoteStagedFile<&SftpSession>) {
    staged.file_mut().write_all(b"new").await.unwrap();
    staged.verify_size(3).await.unwrap();
}

#[tokio::test]
async fn new_file_uses_server_mask_and_cross_copy_requests_source_mode() {
    let (sftp, entries) = setup(0o027, false, true).await;
    for (name, requested, expected) in [
        ("upload", DEFAULT_NEW_FILE_MODE, 0o640),
        ("copy", 0o107755, 0o750),
    ] {
        let mut staged = RemoteStagedFile::create_inner(&sftp, name, requested, None, None)
            .await
            .unwrap();
        payload(&mut staged).await;
        assert!(!entries
            .lock()
            .unwrap()
            .contains_key(&format!("/remote/{name}")));
        let directory = staged.directory.clone();
        staged.commit().await.unwrap();
        let entries = entries.lock().unwrap();
        assert_eq!(
            entries[&format!("/remote/{name}")].mode,
            REGULAR_TYPE | expected
        );
        assert!(!entries.contains_key(&directory));
    }
}

#[tokio::test]
async fn overwrite_preserves_permissions_and_leaves_original_until_commit() {
    let (sftp, entries) = setup(0o022, true, true).await;
    for mode in [0o755, 0o644, 0o664, 0o4755] {
        let path = "/remote/job.sh";
        add(&entries, path, REGULAR_TYPE | mode, b"old contents");
        let mut staged = RemoteStagedFile::create(&sftp, path).await.unwrap();
        let marker = NamespaceMarker {
            path: canonical_child(&staged.directory, "namespace"),
            nonce: "abcdef0123456789".into(),
        };
        add(
            &entries,
            &marker.path,
            REGULAR_TYPE | 0o600,
            marker.nonce.as_bytes(),
        );
        let (client, stream) = tokio::io::duplex(64 * 1024);
        server::run(
            stream,
            FakeServer {
                entries: Arc::clone(&entries),
                creation_mask: 0o022,
                allow_overwrite: false,
                allow_chmod: true,
                extension_response: StatusCode::Ok,
            },
        )
        .await;
        let raw = RawSftpSession::new(client);
        raw.init().await.unwrap();
        staged.marker = Some(marker);
        staged.posix_session = Some(Arc::new(raw));
        payload(&mut staged).await;
        assert_eq!(entries.lock().unwrap()[path].bytes, b"old contents");
        staged.commit().await.unwrap();
        let entries = entries.lock().unwrap();
        assert_eq!(entries[path].bytes, b"new");
        assert_eq!(entries[path].mode, REGULAR_TYPE | mode & 0o777);
        assert_eq!(entries.len(), 1);
    }
}

#[tokio::test]
async fn destination_preflight_rejects_symlink_and_recheck_refuses_changes() {
    let (sftp, entries) = setup(0o022, true, true).await;
    let target = "/remote/target";
    add(&entries, target, 0o120777, b"link");
    assert!(RemoteStagedFile::create(&sftp, target).await.is_err());
    add(&entries, target, REGULAR_TYPE | 0o644, b"original");
    let mut staged = RemoteStagedFile::create(&sftp, target).await.unwrap();
    payload(&mut staged).await;
    add(&entries, target, REGULAR_TYPE | 0o664, b"original");
    assert!(staged.commit().await.is_err());
    staged.cleanup().await;
    let entries = entries.lock().unwrap();
    assert_eq!(entries[target].mode, REGULAR_TYPE | 0o664);
    assert_eq!(entries[target].bytes, b"original");
    assert_eq!(entries.len(), 1);
}

#[tokio::test]
async fn new_target_appearing_before_commit_is_not_clobbered() {
    let (sftp, entries) = setup(0o022, false, true).await;
    let target = "/remote/target";
    let mut staged = RemoteStagedFile::create(&sftp, target).await.unwrap();
    payload(&mut staged).await;
    add(&entries, target, REGULAR_TYPE | 0o600, b"racer");
    assert!(staged.commit().await.is_err());
    staged.cleanup().await;
    assert_eq!(entries.lock().unwrap()[target].bytes, b"racer");
}

#[tokio::test]
async fn no_safe_rename_or_failed_chmod_never_unlinks_target() {
    for allow_chmod in [true, false] {
        let (sftp, entries) = setup(0o022, false, allow_chmod).await;
        let target = "/remote/target";
        add(&entries, target, REGULAR_TYPE | 0o755, b"original");
        let mut staged = RemoteStagedFile::create(&sftp, target).await.unwrap();
        payload(&mut staged).await;
        let error = staged.commit().await.unwrap_err().to_string();
        assert!(error.contains(if allow_chmod {
            "safe atomic"
        } else {
            "preserve remote target permissions"
        }));
        staged.cleanup().await;
        let entries = entries.lock().unwrap();
        assert_eq!(entries[target].bytes, b"original");
        assert_eq!(entries.len(), 1);
    }
}

#[tokio::test]
async fn unsafe_public_mkdir_default_aborts_before_creating_payload() {
    let (sftp, entries) = setup(0, false, true).await;
    assert!(RemoteStagedFile::create(&sftp, "/remote/target")
        .await
        .is_err());
    assert!(entries.lock().unwrap().is_empty());
}

#[tokio::test]
async fn namespace_probe_requires_exact_nonce_across_sftp_channels() {
    let (sftp, entries) = setup(0o022, false, true).await;
    let marker = NamespaceMarker {
        path: "/remote/private/namespace".into(),
        nonce: "abcdef0123456789".into(),
    };
    let (client, stream) = tokio::io::duplex(64 * 1024);
    server::run(
        stream,
        FakeServer {
            entries: Arc::clone(&entries),
            creation_mask: 0o022,
            allow_overwrite: false,
            allow_chmod: true,
            extension_response: StatusCode::OpUnsupported,
        },
    )
    .await;
    let raw = RawSftpSession::new(client);
    raw.init().await.unwrap();
    assert!(!raw_sees_marker(&raw, &marker).await);
    assert!(!sftp_sees_marker(&sftp, &marker).await);
    add(
        &entries,
        &marker.path,
        REGULAR_TYPE | 0o600,
        b"abcdef0123456789extra",
    );
    assert!(!raw_sees_marker(&raw, &marker).await);
    assert!(!sftp_sees_marker(&sftp, &marker).await);
    add(
        &entries,
        &marker.path,
        REGULAR_TYPE | 0o600,
        marker.nonce.as_bytes(),
    );
    assert!(raw_sees_marker(&raw, &marker).await);
    assert!(sftp_sees_marker(&sftp, &marker).await);
}

#[tokio::test]
async fn raw_posix_rename_uses_two_strings_and_requires_a_definitive_status() {
    for extension_response in [
        StatusCode::Ok,
        StatusCode::OpUnsupported,
        StatusCode::ConnectionLost,
    ] {
        let (client, stream) = tokio::io::duplex(64 * 1024);
        let entries = Entries::default();
        let staging = "/remote/路径.part";
        let destination = "/remote/a ' b";
        add(&entries, staging, REGULAR_TYPE | 0o755, b"new");
        add(&entries, destination, REGULAR_TYPE | 0o755, b"old");
        server::run(
            stream,
            FakeServer {
                entries: Arc::clone(&entries),
                creation_mask: 0o022,
                allow_overwrite: false,
                allow_chmod: true,
                extension_response,
            },
        )
        .await;
        let raw = RawSftpSession::new(client);
        raw.init().await.unwrap();
        let result = posix_rename(&raw, staging, destination).await;
        let entries = entries.lock().unwrap();
        match extension_response {
            StatusCode::Ok => {
                assert!(matches!(result, PosixRenameResult::Replaced));
                assert_eq!(entries[destination].bytes, b"new");
                assert!(!entries.contains_key(staging));
            }
            StatusCode::OpUnsupported => {
                assert!(matches!(result, PosixRenameResult::Unsupported));
                assert_eq!(entries[destination].bytes, b"old");
            }
            StatusCode::ConnectionLost => {
                assert!(matches!(result, PosixRenameResult::Unknown(_)));
                assert!(!publication_unknown("test").is_transport_error());
            }
            _ => unreachable!(),
        }
    }
}
