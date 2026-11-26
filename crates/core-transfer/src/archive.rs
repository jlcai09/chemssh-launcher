use std::path::Path;

use core_ssh::{shell_quote, SshConnection};
use serde::{Deserialize, Serialize};
use tokio::process::Command;

use crate::TransferError;

pub const FORMAT_TAR_ZST: &str = "tar.zst";
pub const FORMAT_TAR_GZ: &str = "tar.gz";
pub const FORMAT_TAR_XZ: &str = "tar.xz";
pub const FORMAT_TAR: &str = "tar";
pub const FORMAT_ZIP: &str = "zip";
pub const FORMAT_SEVEN_ZIP: &str = "7z";

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct ArchiveTools {
    pub tar: bool,
    pub zstd: bool,
    pub gzip: bool,
    pub xz: bool,
    pub zip: bool,
    pub unzip: bool,
    pub seven_zip: bool,
}

impl ArchiveTools {
    pub fn supported_formats(&self) -> Vec<&'static str> {
        let mut formats = Vec::with_capacity(6);
        if self.tar && self.zstd {
            formats.push(FORMAT_TAR_ZST);
        }
        if self.tar && self.gzip {
            formats.push(FORMAT_TAR_GZ);
        }
        if self.tar && self.xz {
            formats.push(FORMAT_TAR_XZ);
        }
        if self.tar {
            formats.push(FORMAT_TAR);
        }
        if self.zip {
            formats.push(FORMAT_ZIP);
        }
        if self.seven_zip {
            formats.push(FORMAT_SEVEN_ZIP);
        }
        formats
    }

    pub fn supports(&self, format: &str) -> bool {
        self.supported_formats().contains(&format)
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ArchiveStep {
    pub argv: Vec<String>,
    /// Optional second stage in a two-command pipeline (`argv | pipe_to`),
    /// used for `tar.zst` so legacy `tar` versions without `--zstd` or
    /// parameterized `-I 'zstd ...'` work everywhere.
    pub pipe_to: Option<Vec<String>>,
}

pub fn build_compress_step(
    format: &str,
    archive_name: &str,
    entries: &[String],
) -> Result<ArchiveStep, TransferError> {
    if entries.is_empty() {
        return Err(TransferError::InvalidInput(
            "at least one entry is required to compress".into(),
        ));
    }
    if format == FORMAT_TAR_ZST {
        let mut argv = vec!["tar".into(), "-cf".into(), "-".into()];
        argv.extend(entries.iter().cloned());
        let pipe_to = vec![
            "zstd".into(),
            "-1".into(),
            "-T4".into(),
            "-f".into(),
            "-o".into(),
            archive_name.into(),
        ];
        return Ok(ArchiveStep {
            argv,
            pipe_to: Some(pipe_to),
        });
    }
    let mut argv = match format {
        FORMAT_TAR_GZ => vec!["tar".into(), "-czf".into(), archive_name.into()],
        FORMAT_TAR_XZ => vec!["tar".into(), "-cJf".into(), archive_name.into()],
        FORMAT_TAR => vec!["tar".into(), "-cf".into(), archive_name.into()],
        FORMAT_ZIP => vec!["zip".into(), "-r".into(), archive_name.into()],
        FORMAT_SEVEN_ZIP => vec!["7z".into(), "a".into(), archive_name.into()],
        other => {
            return Err(TransferError::InvalidInput(format!(
                "unsupported archive format: {other}"
            )));
        }
    };
    argv.extend(entries.iter().cloned());
    Ok(ArchiveStep {
        argv,
        pipe_to: None,
    })
}

pub fn build_extract_step(archive_name: &str, dest_dir: &str) -> Result<ArchiveStep, TransferError> {
    let lower = archive_name.to_ascii_lowercase();
    if lower.ends_with(".tar.zst") || lower.ends_with(".tzst") {
        return Ok(ArchiveStep {
            argv: vec!["zstd".into(), "-dc".into(), archive_name.into()],
            pipe_to: Some(vec![
                "tar".into(),
                "-xf".into(),
                "-".into(),
                "-C".into(),
                dest_dir.into(),
            ]),
        });
    }
    let argv = if lower.ends_with(".tar.gz") || lower.ends_with(".tgz") {
        vec![
            "tar".into(),
            "-xzf".into(),
            archive_name.into(),
            "-C".into(),
            dest_dir.into(),
        ]
    } else if lower.ends_with(".tar.xz") || lower.ends_with(".txz") {
        vec![
            "tar".into(),
            "-xJf".into(),
            archive_name.into(),
            "-C".into(),
            dest_dir.into(),
        ]
    } else if lower.ends_with(".tar") {
        vec![
            "tar".into(),
            "-xf".into(),
            archive_name.into(),
            "-C".into(),
            dest_dir.into(),
        ]
    } else if lower.ends_with(".zip") {
        vec![
            "unzip".into(),
            "-o".into(),
            archive_name.into(),
            "-d".into(),
            dest_dir.into(),
        ]
    } else if lower.ends_with(".7z") {
        vec![
            "7z".into(),
            "x".into(),
            "-y".into(),
            format!("-o{dest_dir}"),
            archive_name.into(),
        ]
    } else {
        return Err(TransferError::InvalidInput(format!(
            "unsupported archive filename: {archive_name}"
        )));
    };
    Ok(ArchiveStep {
        argv,
        pipe_to: None,
    })
}

pub async fn detect_local_tools() -> ArchiveTools {
    ArchiveTools {
        tar: command_exists_local("tar"),
        zstd: command_exists_local("zstd"),
        gzip: command_exists_local("gzip"),
        xz: command_exists_local("xz"),
        zip: command_exists_local("zip"),
        unzip: command_exists_local("unzip"),
        seven_zip: command_exists_local("7z"),
    }
}

fn command_exists_local(bin: &str) -> bool {
    let Some(path_var) = std::env::var_os("PATH") else {
        return false;
    };
    #[cfg(target_os = "windows")]
    let exts: Vec<String> = std::env::var("PATHEXT")
        .unwrap_or_else(|_| ".COM;.EXE;.BAT;.CMD".into())
        .split(';')
        .filter(|s| !s.is_empty())
        .map(|s| s.to_string())
        .collect();

    for dir in std::env::split_paths(&path_var) {
        #[cfg(target_os = "windows")]
        {
            if dir.join(bin).is_file() {
                return true;
            }
            for ext in &exts {
                if dir.join(format!("{bin}{ext}")).is_file() {
                    return true;
                }
            }
        }
        #[cfg(not(target_os = "windows"))]
        {
            let candidate = dir.join(bin);
            if candidate.is_file() {
                return true;
            }
        }
    }
    false
}

pub async fn run_local_step(cwd: &Path, step: &ArchiveStep) -> Result<(), TransferError> {
    let (program, args) = step
        .argv
        .split_first()
        .ok_or_else(|| TransferError::InvalidInput("empty archive command".into()))?;
    if let Some(second_argv) = &step.pipe_to {
        let (prog2, args2) = second_argv
            .split_first()
            .ok_or_else(|| TransferError::InvalidInput("empty pipeline stage".into()))?;
        let mut cmd1 = Command::new(program);
        cmd1.args(args)
            .current_dir(cwd)
            .stdout(std::process::Stdio::piped())
            .stderr(std::process::Stdio::piped());
        let mut cmd2 = Command::new(prog2);
        cmd2.args(args2)
            .current_dir(cwd)
            .stdin(std::process::Stdio::piped())
            .stdout(std::process::Stdio::piped())
            .stderr(std::process::Stdio::piped());
        #[cfg(target_os = "windows")]
        {
            cmd1.creation_flags(0x0800_0000);
            cmd2.creation_flags(0x0800_0000);
        }
        let mut child1 = cmd1.spawn()?;
        let mut child2 = cmd2.spawn()?;
        let mut stdout1 = child1
            .stdout
            .take()
            .ok_or_else(|| TransferError::Archive("missing pipeline stdout".into()))?;
        let mut stdin2 = child2
            .stdin
            .take()
            .ok_or_else(|| TransferError::Archive("missing pipeline stdin".into()))?;
        let copy_task = tokio::spawn(async move {
            let res = tokio::io::copy(&mut stdout1, &mut stdin2).await;
            drop(stdin2);
            res
        });
        let (out1, out2) = tokio::try_join!(child1.wait_with_output(), child2.wait_with_output())?;
        let _ = copy_task.await;
        if !out1.status.success() {
            let stderr = String::from_utf8_lossy(&out1.stderr);
            return Err(TransferError::Archive(format!(
                "{program} failed: {}",
                stderr.trim()
            )));
        }
        if !out2.status.success() {
            let stderr = String::from_utf8_lossy(&out2.stderr);
            return Err(TransferError::Archive(format!(
                "{prog2} failed: {}",
                stderr.trim()
            )));
        }
        return Ok(());
    }
    let mut cmd = Command::new(program);
    cmd.args(args).current_dir(cwd);
    #[cfg(target_os = "windows")]
    {
        // CREATE_NO_WINDOW = 0x08000000: prevent spawning a visible console window
        // when running from a GUI subsystem application on Windows.
        cmd.creation_flags(0x0800_0000);
    }
    let output = cmd.output().await?;
    if !output.status.success() {
        let stderr = String::from_utf8_lossy(&output.stderr);
        return Err(TransferError::Archive(format!(
            "{program} failed: {}",
            stderr.trim()
        )));
    }
    Ok(())
}

pub async fn detect_remote_tools(conn: &SshConnection) -> Result<ArchiveTools, TransferError> {
    let script = "for b in tar zstd gzip xz zip unzip 7z; do command -v $b >/dev/null 2>&1 && echo $b; done";
    let out = conn
        .exec_command(script)
        .await
        .map_err(|e| map_exec_archive_err(e.to_string()))?;
    let mut tools = ArchiveTools::default();
    for line in out.stdout.lines().map(str::trim) {
        match line {
            "tar" => tools.tar = true,
            "zstd" => tools.zstd = true,
            "gzip" => tools.gzip = true,
            "xz" => tools.xz = true,
            "zip" => tools.zip = true,
            "unzip" => tools.unzip = true,
            "7z" => tools.seven_zip = true,
            _ => {}
        }
    }
    Ok(tools)
}

pub fn build_remote_shell_command(cwd: &str, step: &ArchiveStep) -> String {
    let first: Vec<String> = step.argv.iter().map(|a| shell_quote(a)).collect();
    if let Some(second_argv) = &step.pipe_to {
        let second: Vec<String> = second_argv.iter().map(|a| shell_quote(a)).collect();
        // Enable pipefail when the shell supports it (bash/zsh/ksh) so errors in the
        // producer stage (e.g. missing input file or corrupt zstd stream) are never masked.
        format!(
            "cd {} && ( set -o pipefail 2>/dev/null || true; {} | {} )",
            shell_quote(cwd),
            first.join(" "),
            second.join(" ")
        )
    } else {
        format!("cd {} && {}", shell_quote(cwd), first.join(" "))
    }
}

pub async fn run_remote_step(
    conn: &SshConnection,
    cwd: &str,
    step: &ArchiveStep,
) -> Result<(), TransferError> {
    let cmd = build_remote_shell_command(cwd, step);
    let out = conn
        .exec_command(&cmd)
        .await
        .map_err(|e| map_exec_archive_err(e.to_string()))?;
    if out.exit_code.unwrap_or(0) != 0 {
        return Err(TransferError::Archive(format!(
            "remote archive command failed (exit {:?}): {}",
            out.exit_code,
            out.stderr.trim()
        )));
    }
    Ok(())
}

fn map_exec_archive_err(msg: String) -> TransferError {
    if msg.contains("remote command execution rejected") {
        TransferError::Archive(msg)
    } else {
        TransferError::SftpTransport(msg)
    }
}
