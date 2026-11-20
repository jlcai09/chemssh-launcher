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
    let mut argv = match format {
        FORMAT_TAR_ZST => vec![
            "tar".into(),
            "--zstd".into(),
            "-cf".into(),
            archive_name.into(),
        ],
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
    Ok(ArchiveStep { argv })
}

pub fn build_extract_step(archive_name: &str, dest_dir: &str) -> Result<ArchiveStep, TransferError> {
    let lower = archive_name.to_ascii_lowercase();
    let argv = if lower.ends_with(".tar.zst") || lower.ends_with(".tzst") {
        vec![
            "tar".into(),
            "--zstd".into(),
            "-xf".into(),
            archive_name.into(),
            "-C".into(),
            dest_dir.into(),
        ]
    } else if lower.ends_with(".tar.gz") || lower.ends_with(".tgz") {
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
    Ok(ArchiveStep { argv })
}

pub async fn detect_local_tools() -> ArchiveTools {
    let mut tools = ArchiveTools::default();
    tools.tar = command_exists_local("tar").await;
    tools.zstd = command_exists_local("zstd").await;
    tools.gzip = command_exists_local("gzip").await;
    tools.xz = command_exists_local("xz").await;
    tools.zip = command_exists_local("zip").await;
    tools.unzip = command_exists_local("unzip").await;
    tools.seven_zip = command_exists_local("7z").await;
    tools
}

async fn command_exists_local(bin: &str) -> bool {
    #[cfg(target_os = "windows")]
    let probe = Command::new("where").arg(bin).output().await;
    #[cfg(not(target_os = "windows"))]
    let probe = Command::new("sh")
        .arg("-c")
        .arg(format!("command -v {}", shell_quote(bin)))
        .output()
        .await;
    probe.map(|o| o.status.success()).unwrap_or(false)
}

pub async fn run_local_step(cwd: &Path, step: &ArchiveStep) -> Result<(), TransferError> {
    let (program, args) = step
        .argv
        .split_first()
        .ok_or_else(|| TransferError::InvalidInput("empty archive command".into()))?;
    let output = Command::new(program)
        .args(args)
        .current_dir(cwd)
        .output()
        .await?;
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
        .map_err(|e| TransferError::Archive(e.to_string()))?;
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

pub async fn run_remote_step(
    conn: &SshConnection,
    cwd: &str,
    step: &ArchiveStep,
) -> Result<(), TransferError> {
    let quoted_argv: Vec<String> = step.argv.iter().map(|a| shell_quote(a)).collect();
    let cmd = format!("cd {} && {}", shell_quote(cwd), quoted_argv.join(" "));
    let out = conn
        .exec_command(&cmd)
        .await
        .map_err(|e| TransferError::Archive(e.to_string()))?;
    if out.exit_code.unwrap_or(0) != 0 {
        return Err(TransferError::Archive(format!(
            "remote archive command failed (exit {:?}): {}",
            out.exit_code,
            out.stderr.trim()
        )));
    }
    Ok(())
}
