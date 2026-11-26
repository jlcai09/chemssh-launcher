use std::time::Duration;

use core_ssh::{shell_quote, SshConnection};

fn marker_check_command(path: &str, nonce: &str) -> Option<String> {
    if !path.starts_with('/')
        || path.contains('\0')
        || nonce.is_empty()
        || !nonce.bytes().all(|byte| byte.is_ascii_alphanumeric())
    {
        return None;
    }
    // Compare exact bytes; do not copy the marker to SSH stdout, assume a
    // content length, or use command substitution (which strips newlines).
    Some(format!(
        "p={}; [ -f \"$p\" ] && [ ! -L \"$p\" ] && printf '%s' {} | cmp -s - \"$p\"",
        shell_quote(path),
        shell_quote(nonce),
    ))
}

/// Prove that the SSH shell sees a fresh, exclusively created SFTP marker
/// at its exact absolute path. The caller owns the marker and its cleanup.
pub async fn sftp_shell_sees_marker(connection: &SshConnection, path: &str, nonce: &str) -> bool {
    let Some(command) = marker_check_command(path, nonce) else {
        return false;
    };
    connection
        .exec_command_timeout(&command, Duration::from_secs(3))
        .await
        .is_ok_and(|output| output.exit_code == Some(0))
}

#[cfg(all(test, unix))]
mod tests {
    use super::*;
    use std::os::unix::fs::symlink;
    use std::process::Command;

    #[test]
    fn shell_marker_requires_exact_bytes_and_rejects_links() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("it's a marker");
        let link = directory.path().join("link");
        let matches = |path: &std::path::Path| {
            let command = marker_check_command(path.to_str().unwrap(), "abcdef42").unwrap();
            Command::new("sh")
                .arg("-c")
                .arg(command)
                .status()
                .unwrap()
                .success()
        };
        std::fs::write(&path, b"abcdef42").unwrap();
        symlink(&path, &link).unwrap();
        assert!(matches(&path));
        assert!(!matches(&link));
        std::fs::write(&path, b"abcdef42\n").unwrap();
        assert!(!matches(&path));
        assert!(marker_check_command("relative", "abcdef42").is_none());
        assert!(marker_check_command("/absolute", "not a nonce").is_none());
    }
}
