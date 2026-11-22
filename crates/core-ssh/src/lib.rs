pub mod client;
pub mod hostkey;
pub mod proxy_bridge;
pub mod remote_command;
pub mod shell;
pub mod tunnel;

use thiserror::Error;

pub use client::{ExecOutput, SshConnection, SshCredentials};
pub use hostkey::{
    fingerprint_sha256, normalize_host_address, HostKeyPolicy, HostKeyVerificationError,
    KnownHostsVerifier,
};
pub use proxy_bridge::{
    proxy_bridge_path, remove_proxy_bridge, write_proxy_bridge, ProxyBridgeFile,
    PROXY_BRIDGE_DIR_NAME, PROXY_BRIDGE_VERSION,
};
pub use remote_command::{
    assemble_check_port_command, assemble_kill_pid_command, assemble_local_command,
    assemble_remote_command, effective_check_port_command, effective_local_start_command,
    effective_start_command, port_warning, shell_quote,
};
pub use shell::{PtyShellSession, SendInputResult, TerminalEvent};
pub use tunnel::{LocalTunnel, ReverseProxyChannel};

#[derive(Debug, Error)]
pub enum SshError {
    #[error("ssh connect failed: {0}")]
    Connect(String),
    #[error("ssh authentication failed: {0}")]
    Auth(String),
    #[error("{0}")]
    HostKeyVerification(HostKeyVerificationError),
    #[error("host key verification internal error: {0}")]
    HostKeyInternal(String),
    #[error("ssh exec failed: {0}")]
    Exec(String),
    #[error("ssh tunnel failed: {0}")]
    Tunnel(String),
    #[error("reverse proxy channel failed: {0}")]
    ReverseProxy(String),
    #[error("proxy bridge failed: {0}")]
    ProxyBridge(String),
    #[error("sftp session failed: {0}")]
    Sftp(String),
    #[error("pty shell failed: {0}")]
    Shell(String),
    #[error("io error: {0}")]
    Io(#[from] std::io::Error),
}

#[cfg(test)]
mod tests {
    use super::*;
    use core_config::{proxy_env_pairs, Profile};
    use russh::keys::PublicKey;
    use tempfile::tempdir;

    #[test]
    fn remote_command_places_proxy_exports_after_set_e_and_before_pre_start() {
        let mut p = Profile::remote_defaults();
        p.pre_start_commands = "cd /home/user/chemssh\nsource .venv/bin/activate".into();
        p.start_command = "chemssh --config config.yaml".into();
        p.remote_host = "127.0.0.1".into();
        p.remote_port = 8888;

        let pairs = proxy_env_pairs("http://127.0.0.1:43210", "localhost,127.0.0.1");
        let cmd = assemble_remote_command(&p, &pairs);

        let set_e_pos = cmd.find("set -e\n").unwrap();
        let export_pos = cmd.find("export http_proxy=http://127.0.0.1:43210\n").unwrap();
        let pre_start_pos = cmd.find("cd /home/user/chemssh\n").unwrap();
        let start_pos = cmd
            .find("chemssh --config config.yaml --host 127.0.0.1 --port 8888 &")
            .unwrap();

        assert!(set_e_pos < export_pos);
        assert!(export_pos < pre_start_pos);
        assert!(pre_start_pos < start_pos);
    }

    #[test]
    fn port_warning_detects_mismatched_port_in_custom_command() {
        let mut p = Profile::remote_defaults();
        p.remote_port = 8888;
        p.start_command = "chemssh --config config.yaml --port 9999".into();
        assert!(port_warning(&p).is_some());

        p.start_command = "chemssh --config config.yaml --port 8888".into();
        assert!(port_warning(&p).is_none());
    }

    #[test]
    fn proxy_bridge_file_matches_chemssh_contract() {
        let bridge = ProxyBridgeFile::new("http://127.0.0.1:40001", "localhost,127.0.0.1,::1");
        assert_eq!(bridge.version, 1);
        assert_eq!(bridge.proxy_url, "http://127.0.0.1:40001");
        assert_eq!(bridge.env.get("http_proxy").map(String::as_str), Some("http://127.0.0.1:40001"));
        assert_eq!(bridge.env.get("HTTP_PROXY").map(String::as_str), Some("http://127.0.0.1:40001"));
        assert_eq!(bridge.env.get("NO_PROXY").map(String::as_str), Some("localhost,127.0.0.1,::1"));
        assert_eq!(
            proxy_bridge_path("/home/user", 8888),
            "/home/user/.chemssh-launcher/terminal-proxy-8888.json"
        );
    }

    #[test]
    fn known_hosts_strict_accept_new_and_replace_changed() {
        let dir = tempdir().unwrap();
        let path = dir.path().join("known_hosts");

        let key1 = PublicKey::from_openssh(
            "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl test1@example.com",
        )
        .unwrap();
        let key2 = PublicKey::from_openssh(
            "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIG4rT3vTt99Ox5kndS4HmgTrKBT8SKzhK4rhGkEVGlCI test2@example.com",
        )
        .unwrap();

        // Strict rejects unknown host.
        let strict = KnownHostsVerifier::new(&path, "example.com", 22, HostKeyPolicy::Strict);
        let err = strict.verify(&key1).unwrap_err();
        assert!(matches!(err, SshError::HostKeyVerification(ref e) if !e.mismatch));

        // AcceptNew saves first key and then Strict passes.
        let accept = KnownHostsVerifier::new(&path, "example.com", 22, HostKeyPolicy::AcceptNew);
        accept.verify(&key1).unwrap();
        strict.verify(&key1).unwrap();

        // AcceptNew rejects changed key as mismatch.
        let mismatch_err = accept.verify(&key2).unwrap_err();
        assert!(matches!(mismatch_err, SshError::HostKeyVerification(ref e) if e.mismatch));

        // ReplaceChanged replaces stored key and then Strict passes for key2.
        let replace =
            KnownHostsVerifier::new(&path, "example.com", 22, HostKeyPolicy::ReplaceChanged);
        replace.verify(&key2).unwrap();
        strict.verify(&key2).unwrap();
    }

    #[test]
    fn known_hosts_prioritizes_existing_host_key_algorithm() {
        let dir = tempdir().unwrap();
        let path = dir.path().join("known_hosts");
        std::fs::write(
            &path,
            "example.com ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBF9d\n",
        )
        .unwrap();

        let verifier = KnownHostsVerifier::new(&path, "example.com", 22, HostKeyPolicy::Strict);
        let algos = verifier.preferred_key_algorithms().unwrap();
        assert_eq!(
            algos.first(),
            Some(&russh::keys::Algorithm::Ecdsa {
                curve: russh::keys::EcdsaCurve::NistP256,
            })
        );
    }
}
