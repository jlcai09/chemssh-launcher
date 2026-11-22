use std::path::Path;

use core_config::EnvPair;

use crate::remote_command::shell_quote;

pub const SHELL_INTEGRATION_SHIM_MARKER: &str = "CHEMSSH_LAUNCHER_SHIM=";
pub const SHELL_INTEGRATION_SHELL_MARKER: &str = "CHEMSSH_LAUNCHER_SHELL=";

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ShellIntegration {
    pub dir: String,
    pub init_file: String,
    pub pid_file: String,
    pub shell_path: String,
    pub supports_bash_rcfile: bool,
}

impl ShellIntegration {
    pub fn bin_dir(&self) -> String {
        format!("{}/bin", self.dir)
    }

    pub fn interactive_command(&self, proxy_env: &[EnvPair]) -> String {
        interactive_shell_command(self, proxy_env)
    }
}

pub fn build_shell_integration_setup_script(proxy_env: &[EnvPair]) -> String {
    let mut proxy_exports = String::new();
    for pair in proxy_env {
        proxy_exports.push_str(&format!(
            "export {}={}\n",
            pair.key,
            shell_quote(&pair.value)
        ));
    }

    format!(
        r#"set -e
umask 077
shell=${{SHELL:-}}
if [ -z "$shell" ]; then
  shell=$(getent passwd "$(id -u)" 2>/dev/null | cut -d: -f7 || true)
fi
[ -n "$shell" ] || shell=/bin/bash
case "$shell" in
  /*) ;;
  *) shell=/bin/bash ;;
esac
[ -x "$shell" ] || shell=/bin/sh

dir=$(mktemp -d "${{TMPDIR:-/tmp}}/chemssh-launcher.XXXXXX" 2>/dev/null) || dir=$(mktemp -d 2>/dev/null) || exit 1
mkdir -p "$dir/bin" || exit 1

cat > "$dir/bin/_b64" <<'__CHEMSSH_EOF__'
#!/bin/sh
if command -v base64 >/dev/null 2>&1; then
  printf '%s' "$1" | base64 | tr -d '\r\n'
elif command -v python3 >/dev/null 2>&1; then
  python3 -c 'import base64,sys;sys.stdout.write(base64.b64encode(sys.argv[1].encode()).decode())' "$1"
elif command -v python >/dev/null 2>&1; then
  python -c 'import base64,sys;sys.stdout.write(base64.b64encode(sys.argv[1].encode("utf-8")).decode("ascii"))' "$1"
elif command -v openssl >/dev/null 2>&1; then
  printf '%s' "$1" | openssl base64 -A
else
  exit 1
fi
__CHEMSSH_EOF__

cat > "$dir/bin/sz" <<'__CHEMSSH_EOF__'
#!/bin/sh
_dir=$(dirname -- "$0")
_cwd=${{PWD:-}}
[ -n "$_cwd" ] || _cwd=$(pwd)
_cwd_b64=$("$_dir/_b64" "$_cwd") || exit 1
_payload="v1|download|$_cwd_b64"
for _arg in "$@"; do
  _arg_b64=$("$_dir/_b64" "$_arg") || exit 1
  _payload="$_payload|$_arg_b64"
done
printf '\033]777;chemssh-transfer;%s\007' "$_payload"
__CHEMSSH_EOF__

cat > "$dir/bin/rz" <<'__CHEMSSH_EOF__'
#!/bin/sh
_dir=$(dirname -- "$0")
_cwd=${{PWD:-}}
[ -n "$_cwd" ] || _cwd=$(pwd)
_cwd_b64=$("$_dir/_b64" "$_cwd") || exit 1
_payload="v1|upload|$_cwd_b64"
for _arg in "$@"; do
  _arg_b64=$("$_dir/_b64" "$_arg") || exit 1
  _payload="$_payload|$_arg_b64"
done
printf '\033]777;chemssh-transfer;%s\007' "$_payload"
__CHEMSSH_EOF__

cat > "$dir/bin/vi" <<'__CHEMSSH_EOF__'
#!/bin/sh
_shim_bin=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
_clean_path=
_old_ifs=$IFS
IFS=:
for _item in $PATH; do
  [ "$_item" = "$_shim_bin" ] && continue
  if [ -z "$_clean_path" ]; then
    _clean_path=$_item
  else
    _clean_path=$_clean_path:$_item
  fi
done
IFS=$_old_ifs
PATH=$_clean_path
export PATH
_real=$(command -v vi 2>/dev/null || true)
if [ -z "$_real" ]; then
  echo "vi: command not found" >&2
  exit 127
fi
if "$_real" --version 2>/dev/null | sed -n '1p' | grep -qi 'vim'; then
  exec "$_real" --cmd 'set t_RV= t_u7= t_RF= t_RB= t_RK=' "$@"
fi
exec "$_real" "$@"
__CHEMSSH_EOF__

cat > "$dir/bin/vim" <<'__CHEMSSH_EOF__'
#!/bin/sh
_shim_bin=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
_clean_path=
_old_ifs=$IFS
IFS=:
for _item in $PATH; do
  [ "$_item" = "$_shim_bin" ] && continue
  if [ -z "$_clean_path" ]; then
    _clean_path=$_item
  else
    _clean_path=$_clean_path:$_item
  fi
done
IFS=$_old_ifs
PATH=$_clean_path
export PATH
_real=$(command -v vim 2>/dev/null || true)
if [ -z "$_real" ]; then
  echo "vim: command not found" >&2
  exit 127
fi
if "$_real" --version 2>/dev/null | sed -n '1p' | grep -qi 'vim'; then
  exec "$_real" --cmd 'set t_RV= t_u7= t_RF= t_RB= t_RK=' "$@"
fi
exec "$_real" "$@"
__CHEMSSH_EOF__

chmod 700 "$dir" "$dir/bin" "$dir/bin/_b64" "$dir/bin/sz" "$dir/bin/rz" "$dir/bin/vi" "$dir/bin/vim" || exit 1

cat > "$dir/init.sh" <<'__CHEMSSH_INIT_EOF__'
if [[ -z ${{CHEMSSH_LAUNCHER_INIT_DONE:-}} ]]; then
  CHEMSSH_LAUNCHER_INIT_DONE=1
  if [[ -r /etc/profile ]]; then
    source /etc/profile
  fi
  if [[ -n ${{HOME:-}} ]]; then
    for _chemssh_launcher_profile in "${{HOME}}/.bash_profile" "${{HOME}}/.bash_login" "${{HOME}}/.profile"; do
      if [[ -r "${{_chemssh_launcher_profile}}" ]]; then
        source "${{_chemssh_launcher_profile}}"
        break
      fi
    done
    unset _chemssh_launcher_profile
  fi
fi
_chemssh_launcher_dir=$(CDPATH= cd -- "$(dirname -- "${{BASH_SOURCE[0]}}")" && pwd)
case ":$PATH:" in
  *":$_chemssh_launcher_dir/bin:"*) ;;
  *) export PATH="$_chemssh_launcher_dir/bin:$PATH" ;;
esac
_chemssh_launcher_marker='printf "\033]633;P;Cwd=%s\007" "$PWD"'
case "${{PROMPT_COMMAND:-}}" in
  *"]633;P;Cwd="*) ;;
  *)
    if [[ -n "${{PROMPT_COMMAND:-}}" ]]; then
      PROMPT_COMMAND="${{_chemssh_launcher_marker}}; ${{PROMPT_COMMAND}}"
    else
      PROMPT_COMMAND="${{_chemssh_launcher_marker}}"
    fi
    ;;
esac
export PROMPT_COMMAND
unset _chemssh_launcher_marker _chemssh_launcher_dir
__CHEMSSH_INIT_EOF__
{proxy_exports_append}
chmod 600 "$dir/init.sh" || exit 1
if ! "$dir/bin/sz" >/dev/null 2>&1; then
  rm -rf -- "$dir"
  exit 1
fi
printf '%s%s\n' '{shell_marker}' "$shell"
printf '%s%s\n' '{shim_marker}' "$dir"
"#,
        proxy_exports_append = if proxy_exports.is_empty() {
            String::new()
        } else {
            format!(
                "cat >> \"$dir/init.sh\" <<'__CHEMSSH_PROXY_EOF__'\n\n# Launcher proxy environment, applied after login profiles so it wins.\n{proxy_exports}__CHEMSSH_PROXY_EOF__\n"
            )
        },
        shell_marker = SHELL_INTEGRATION_SHELL_MARKER,
        shim_marker = SHELL_INTEGRATION_SHIM_MARKER,
    )
}

pub fn parse_shell_integration_output(output: &str) -> Option<ShellIntegration> {
    let mut dir: Option<String> = None;
    let mut shell_path: Option<String> = None;

    for raw_line in output.lines() {
        let line = raw_line.trim();
        if let Some(val) = line.strip_prefix(SHELL_INTEGRATION_SHIM_MARKER) {
            let trimmed = val.trim();
            if valid_remote_abs_path(trimmed) {
                dir = Some(trimmed.to_string());
            }
        } else if let Some(val) = line.strip_prefix(SHELL_INTEGRATION_SHELL_MARKER) {
            let trimmed = val.trim();
            if valid_remote_abs_path(trimmed) {
                shell_path = Some(trimmed.to_string());
            }
        }
    }

    let dir = dir?;
    let shell_path = shell_path?;
    let shell_name = Path::new(&shell_path)
        .file_name()
        .and_then(|s| s.to_str())
        .unwrap_or_default();
    let supports_bash_rcfile = shell_name.contains("bash");

    Some(ShellIntegration {
        init_file: format!("{dir}/init.sh"),
        pid_file: format!("{dir}/shell.pid"),
        dir,
        shell_path,
        supports_bash_rcfile,
    })
}

fn valid_remote_abs_path(value: &str) -> bool {
    value.starts_with('/') && !value.contains(['\r', '\n', '\0'])
}

fn env_assignments(env: &[EnvPair]) -> String {
    env.iter()
        .map(|pair| format!("{}={}", pair.key, shell_quote(&pair.value)))
        .collect::<Vec<_>>()
        .join(" ")
}

pub fn proxy_login_shell_command(shell_path: &str, proxy_env: &[EnvPair]) -> String {
    let shell_trimmed = shell_path.trim();
    let effective_shell = if shell_trimmed.is_empty() {
        "/bin/sh"
    } else {
        shell_trimmed
    };

    if proxy_env.is_empty() {
        return format!("exec {} -l", shell_quote(effective_shell));
    }

    format!(
        "exec env {} {} -l",
        env_assignments(proxy_env),
        shell_quote(effective_shell)
    )
}

pub fn interactive_shell_exec_command(
    integration: &ShellIntegration,
    proxy_env: &[EnvPair],
) -> String {
    if integration.supports_bash_rcfile {
        let prefix = if proxy_env.is_empty() {
            String::new()
        } else {
            format!("env {} ", env_assignments(proxy_env))
        };
        return format!(
            "exec {prefix}{} --rcfile {} -i",
            shell_quote(&integration.shell_path),
            shell_quote(&integration.init_file)
        );
    }

    let mut assignments = env_assignments(proxy_env);
    if !assignments.is_empty() {
        assignments.push(' ');
    }
    format!(
        "exec env {assignments}PATH={}:\"$PATH\" {} -l",
        shell_quote(&integration.bin_dir()),
        shell_quote(&integration.shell_path)
    )
}

pub fn interactive_shell_command(
    integration: &ShellIntegration,
    proxy_env: &[EnvPair],
) -> String {
    let exec_cmd = interactive_shell_exec_command(integration, proxy_env);
    let script = format!(
        "printf '%s' \"$$\" > {}\n{}",
        shell_quote(&integration.pid_file),
        exec_cmd
    );
    format!("exec /bin/sh -c {}", shell_quote(&script))
}

pub fn build_query_cwd_command(pid_file: &str) -> String {
    let inner = format!(
        "pid=$(cat -- {} 2>/dev/null) || exit 1; \
         case \"$pid\" in \"\"|*[!0-9]*) exit 1 ;; esac; \
         cwd=$(readlink -- \"/proc/$pid/cwd\" 2>/dev/null) || exit 1; \
         printf '\\000%s\\000' \"$cwd\"",
        shell_quote(pid_file)
    );
    format!("/bin/sh -c {}", shell_quote(&inner))
}

pub fn parse_query_cwd_output(bytes: &[u8]) -> Option<String> {
    let first = bytes.iter().position(|&b| b == 0)?;
    let rest = &bytes[first + 1..];
    let second = rest.iter().position(|&b| b == 0)?;
    let path = std::str::from_utf8(&rest[..second]).ok()?.trim();
    if path.is_empty() {
        None
    } else {
        Some(path.to_string())
    }
}

pub fn cleanup_shell_integration_command(dir: &str) -> Option<String> {
    let trimmed = dir.trim();
    if trimmed.is_empty()
        || !trimmed.starts_with('/')
        || trimmed.contains(['\n', '\r', '\0'])
        || trimmed.split('/').any(|seg| seg == "..")
        || !trimmed.contains("chemssh-launcher.")
    {
        return None;
    }
    Some(format!("rm -rf -- {}", shell_quote(trimmed)))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn setup_script_guards_init_and_injects_proxy_at_end() {
        let env = vec![
            EnvPair {
                key: "http_proxy",
                value: "http://127.0.0.1:41234".into(),
            },
            EnvPair {
                key: "NO_PROXY",
                value: "localhost,127.0.0.1".into(),
            },
        ];
        let script = build_shell_integration_setup_script(&env);
        assert!(script.contains("CHEMSSH_LAUNCHER_INIT_DONE"));
        assert!(script.contains("getent passwd"));
        assert!(script.contains("grep -qi 'vim'"));

        let init_pos = script.find("/etc/profile").expect("init script");
        let prompt_pos = script.find("PROMPT_COMMAND").expect("prompt hook");
        let proxy_pos = script
            .find("export http_proxy=http://127.0.0.1:41234")
            .expect("proxy export");
        assert!(init_pos < prompt_pos);
        assert!(prompt_pos < proxy_pos);
    }

    #[test]
    fn parse_integration_wraps_shell_pid_and_preserves_shim_path() {
        let out = "noise\nCHEMSSH_LAUNCHER_SHELL=/bin/bash\nCHEMSSH_LAUNCHER_SHIM=/tmp/chemssh-launcher.abc123\n";
        let integration = parse_shell_integration_output(out).expect("parsed");
        assert!(integration.supports_bash_rcfile);
        assert_eq!(integration.init_file, "/tmp/chemssh-launcher.abc123/init.sh");
        assert_eq!(integration.pid_file, "/tmp/chemssh-launcher.abc123/shell.pid");

        let env = vec![EnvPair {
            key: "http_proxy",
            value: "http://127.0.0.1:9000".into(),
        }];
        let bash_cmd = integration.interactive_command(&env);
        assert!(bash_cmd.starts_with("exec /bin/sh -c "));
        assert!(bash_cmd.contains("shell.pid"));
        assert!(bash_cmd.contains("--rcfile"));

        let zsh_out = "CHEMSSH_LAUNCHER_SHELL=/usr/bin/zsh\nCHEMSSH_LAUNCHER_SHIM=/tmp/chemssh-launcher.xyz\n";
        let zsh = parse_shell_integration_output(zsh_out).expect("parsed zsh");
        assert!(!zsh.supports_bash_rcfile);
        let zsh_exec = interactive_shell_exec_command(&zsh, &env);
        assert_eq!(
            zsh_exec,
            "exec env http_proxy=http://127.0.0.1:9000 PATH=/tmp/chemssh-launcher.xyz/bin:\"$PATH\" /usr/bin/zsh -l"
        );
        assert!(zsh.interactive_command(&env).contains("shell.pid"));
    }

    #[test]
    fn parse_query_cwd_extracts_nul_delimited_path() {
        let raw = b"banner\n\0/home/user/project dir\0trailing";
        assert_eq!(
            parse_query_cwd_output(raw).as_deref(),
            Some("/home/user/project dir")
        );
        assert_eq!(parse_query_cwd_output(b"/home/user"), None);
    }

    #[test]
    fn cleanup_rejects_unsafe_paths() {
        assert_eq!(cleanup_shell_integration_command("/tmp/../etc"), None);
        assert_eq!(cleanup_shell_integration_command("/tmp/other"), None);
        assert_eq!(
            cleanup_shell_integration_command("/tmp/chemssh-launcher.12345").as_deref(),
            Some("rm -rf -- /tmp/chemssh-launcher.12345")
        );
        assert_eq!(
            cleanup_shell_integration_command("/tmp/chemssh-launcher.dir name").as_deref(),
            Some("rm -rf -- '/tmp/chemssh-launcher.dir name'")
        );
    }
}
