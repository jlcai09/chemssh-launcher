use core_config::{EnvPair, Profile, DEFAULT_START_COMMAND};
use regex::Regex;

/// Assembles the remote shell script executed over SSH.
/// Ordering invariant: `set -e` -> proxy exports -> `pre_start_commands` -> managed start command.
pub fn assemble_remote_command(profile: &Profile, proxy_env: &[EnvPair]) -> String {
    let mut out = String::new();
    out.push_str("set -e\n");
    write_proxy_exports(&mut out, proxy_env);
    if !profile.pre_start_commands.trim().is_empty() {
        out.push_str(profile.pre_start_commands.trim_end_matches(['\r', '\n']));
        out.push('\n');
    }
    out.push_str("__chemssh_launcher_pid=\n");
    out.push_str("__chemssh_launcher_cleanup() {\n");
    out.push_str("  if [ -n \"$__chemssh_launcher_pid\" ]; then\n");
    out.push_str("    kill -TERM \"$__chemssh_launcher_pid\" 2>/dev/null || true\n");
    out.push_str("    i=0\n");
    out.push_str("    while kill -0 \"$__chemssh_launcher_pid\" 2>/dev/null && [ \"$i\" -lt 5 ]; do\n");
    out.push_str("      i=$((i + 1))\n");
    out.push_str("      sleep 1\n");
    out.push_str("    done\n");
    out.push_str("    kill -KILL \"$__chemssh_launcher_pid\" 2>/dev/null || true\n");
    out.push_str("    wait \"$__chemssh_launcher_pid\" 2>/dev/null || true\n");
    out.push_str("  fi\n");
    out.push_str("}\n");
    out.push_str("trap '__chemssh_launcher_cleanup; exit 143' INT TERM HUP\n");
    out.push_str(&background_last_command(&effective_start_command(profile)));
    out.push_str("__chemssh_launcher_pid=$!\n");
    out.push_str("set +e\n");
    out.push_str("wait \"$__chemssh_launcher_pid\"\n");
    out.push_str("__chemssh_launcher_status=$?\n");
    out.push_str("set -e\n");
    out.push_str("trap - INT TERM HUP\n");
    out.push_str("__chemssh_launcher_cleanup\n");
    out.push_str("exit \"$__chemssh_launcher_status\"\n");
    out
}

pub fn assemble_kill_pid_command(pid: u32) -> String {
    let pid_val = shell_quote(&pid.to_string());
    [
        "set +e".to_string(),
        format!("__chemssh_launcher_pid={pid_val}"),
        "kill -TERM \"$__chemssh_launcher_pid\" 2>/dev/null || true".to_string(),
        "i=0".to_string(),
        "while kill -0 \"$__chemssh_launcher_pid\" 2>/dev/null && [ \"$i\" -lt 5 ]; do".to_string(),
        "  i=$((i + 1))".to_string(),
        "  sleep 1".to_string(),
        "done".to_string(),
        "kill -KILL \"$__chemssh_launcher_pid\" 2>/dev/null || true".to_string(),
        "exit 0".to_string(),
        String::new(),
    ]
    .join("\n")
}

pub fn assemble_check_port_command(profile: &Profile, proxy_env: &[EnvPair]) -> String {
    let mut out = String::new();
    out.push_str("set -e\n");
    write_proxy_exports(&mut out, proxy_env);
    if !profile.pre_start_commands.trim().is_empty() {
        out.push_str(profile.pre_start_commands.trim_end_matches(['\r', '\n']));
        out.push('\n');
    }
    out.push_str(&effective_check_port_command(profile));
    out.push('\n');
    out
}

pub fn effective_check_port_command(profile: &Profile) -> String {
    let command = effective_start_command(profile);
    let mut lines: Vec<String> = command.split('\n').map(ToString::to_string).collect();
    if let Some(last) = lines.iter().rposition(|l| !l.trim().is_empty()) {
        if !has_flag(&lines[last], "check-port") {
            lines[last].push_str(" --check-port");
        }
    }
    lines.join("\n")
}

pub fn effective_start_command(profile: &Profile) -> String {
    let trimmed = profile.start_command.trim_end_matches(['\r', '\n']);
    let command = if trimmed.trim().is_empty() {
        DEFAULT_START_COMMAND
    } else {
        trimmed
    };
    append_start_command_address(command, &profile.remote_host, profile.remote_port)
}

pub fn effective_local_start_command(profile: &Profile) -> String {
    let trimmed = profile.start_command.trim_end_matches(['\r', '\n']);
    if trimmed.trim().is_empty() {
        return trimmed.to_string();
    }
    append_start_command_address(trimmed, &profile.local_host, profile.local_port)
}

pub fn assemble_local_command(profile: &Profile) -> String {
    let mut parts = Vec::new();
    if !profile.pre_start_commands.trim().is_empty() {
        parts.push(
            profile
                .pre_start_commands
                .trim_end_matches(['\r', '\n'])
                .to_string(),
        );
    }
    let start = effective_local_start_command(profile);
    if !start.trim().is_empty() {
        parts.push(start);
    }
    parts.join("\n").trim().to_string()
}

pub fn port_warning(profile: &Profile) -> Option<String> {
    if profile.remote_port == 0 || profile.start_command.is_empty() {
        return None;
    }
    let re = Regex::new(r"(?i)(--port[= ]+|:)(\d{2,5})").ok()?;
    for cap in re.captures_iter(&profile.start_command) {
        if let Some(m) = cap.get(2) {
            if let Ok(port) = m.as_str().parse::<u16>() {
                if port != profile.remote_port {
                    return Some(format!(
                        "start command appears to use port {} while profile remote port is {}",
                        m.as_str(),
                        profile.remote_port
                    ));
                }
            }
        }
    }
    None
}

pub fn shell_quote(value: &str) -> String {
    if value.is_empty() {
        return "''".to_string();
    }
    if value
        .bytes()
        .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'_' | b'.' | b'/' | b':' | b'-'))
    {
        return value.to_string();
    }
    format!("'{}'", value.replace('\'', "'\"'\"'"))
}

fn append_start_command_address(command: &str, host: &str, port: u16) -> String {
    let mut lines: Vec<String> = command.split('\n').map(ToString::to_string).collect();
    let Some(last) = lines.iter().rposition(|l| !l.trim().is_empty()) else {
        return command.to_string();
    };
    if !has_flag(&lines[last], "host") {
        lines[last].push_str(" --host ");
        lines[last].push_str(&shell_quote(host));
    }
    if !has_flag(&lines[last], "port") {
        lines[last].push_str(" --port ");
        lines[last].push_str(&port.to_string());
    }
    lines.join("\n")
}

fn has_flag(command: &str, name: &str) -> bool {
    let pattern = format!(r"(^|\s)--{}($|\s|=)", regex::escape(name));
    Regex::new(&pattern)
        .map(|re| re.is_match(command))
        .unwrap_or(false)
}

fn write_proxy_exports(out: &mut String, proxy_env: &[EnvPair]) {
    for pair in proxy_env {
        out.push_str("export ");
        out.push_str(pair.key);
        out.push('=');
        out.push_str(&shell_quote(&pair.value));
        out.push('\n');
    }
}

fn background_last_command(command: &str) -> String {
    let mut lines: Vec<String> = command
        .trim_end_matches(['\r', '\n'])
        .split('\n')
        .map(ToString::to_string)
        .collect();
    if let Some(last) = lines.iter().rposition(|l| !l.trim().is_empty()) {
        lines[last].push_str(" &");
        return format!("{}\n", lines.join("\n"));
    }
    "true &\n".to_string()
}
