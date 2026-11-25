use core_config::EnvPair;

use crate::manifest::{Action, ArchiveFile, Manifest};
use crate::options::{InstallDir, InstallerOptions};

/// Exit status of the install script when no download source worked.
/// The caller may then download locally and upload the archive over SFTP.
pub const EXIT_DOWNLOAD_FAILED: u32 = 20;

pub struct ScriptParams<'a> {
    pub dir: &'a InstallDir,
    pub action: Action,
    pub manifest: &'a Manifest,
    pub archive: &'a ArchiveFile,
    pub urls: &'a [String],
    /// Absolute remote path of an already uploaded archive.
    pub uploaded: Option<&'a str>,
    pub options: &'a InstallerOptions,
    pub pre_start: &'a str,
    pub proxy_env: &'a [EnvPair],
}

pub fn shell_quote(value: &str) -> String {
    format!("'{}'", value.replace('\'', "'\\''"))
}

/// `export` lines for the reverse-proxy environment, one per line.
pub fn proxy_exports(proxy_env: &[EnvPair]) -> String {
    let mut out = String::new();
    for pair in proxy_env {
        out.push_str(&format!("export {}={}\n", pair.key, shell_quote(&pair.value)));
    }
    out
}

/// Script that reports the state of the remote install directory.
pub fn build_probe_script(dir: &InstallDir) -> String {
    format!(
        r#"CS_DIR="{dir}"
echo "HOME_DIR=$HOME"
if [ -x "$CS_DIR/.venv/bin/python" ]; then echo "VENV_OK=1"; else echo "VENV_OK=0"; fi
echo "__RECORD__"
cat "$CS_DIR/install.json" 2>/dev/null
echo
echo "__END__"
"#,
        dir = dir.shell_expr()
    )
}

/// The full install / update script. Ordering follows the launcher convention:
/// `set -e`, proxy exports, user pre-start commands, then the installer body.
pub fn build_install_script(p: &ScriptParams<'_>) -> String {
    let mut out = String::new();
    out.push_str("set -e\n");
    out.push_str(&proxy_exports(p.proxy_env));
    if !p.pre_start.trim().is_empty() {
        out.push_str(p.pre_start.trim_end_matches(['\r', '\n']));
        out.push('\n');
    }
    out.push_str(&format!("CS_DIR=\"{}\"\n", p.dir.shell_expr()));
    out.push_str(&format!("CS_ACTION={}\n", shell_quote(p.action.script_name())));
    out.push_str(&format!("CS_VERSION={}\n", shell_quote(&p.manifest.version)));
    out.push_str(&format!("CS_TAG={}\n", shell_quote(&p.manifest.tag)));
    out.push_str(&format!("CS_FILE={}\n", shell_quote(&p.archive.name)));
    out.push_str(&format!("CS_SHA={}\n", shell_quote(&p.archive.sha256.to_ascii_lowercase())));
    out.push_str(&format!(
        "CS_FPRINT={}\n",
        shell_quote(&p.manifest.deps_fingerprint.to_ascii_lowercase())
    ));
    out.push_str(&format!("CS_PYTHON={}\n", shell_quote(p.options.python())));
    out.push_str(&format!("CS_PIP_INDEX={}\n", shell_quote(p.options.pip_index())));
    out.push_str(&format!("CS_UPLOADED={}\n", shell_quote(p.uploaded.unwrap_or(""))));
    out.push_str(&format!("CS_URLS={}\n", shell_quote(&p.urls.join("\n"))));
    out.push_str(&format!("CS_EXIT_DOWNLOAD={EXIT_DOWNLOAD_FAILED}\n"));
    out.push_str(INSTALL_BODY);
    out
}

const INSTALL_BODY: &str = r#"
say() { printf '[installer] %s\n' "$*"; }
die() { printf '[installer] ERROR: %s\n' "$*" >&2; exit 1; }

APP="$CS_DIR/app"
STAGE="$CS_DIR/.staging"
BK="$CS_DIR/.backup"
UPLOAD_DIR="$CS_DIR/.upload"
VPY="$CS_DIR/.venv/bin/python"
SWAPPED=0
VENV_NEW=0
FREEZE=""

restore_app() {
  if [ "$SWAPPED" = 1 ] && [ -d "$BK" ]; then
    say "restoring previous files"
    for f in "$BK"/* "$BK"/.[!.]*; do
      [ -e "$f" ] || continue
      n=$(basename "$f")
      if [ "$n" = "plugins" ] && [ -d "$f" ]; then
        mkdir -p "$APP/plugins"
        for pf in "$f"/* "$f"/.[!.]*; do
          [ -e "$pf" ] || continue
          pn=$(basename "$pf")
          rm -rf "$APP/plugins/$pn"
          mv "$pf" "$APP/plugins/$pn"
        done
      else
        rm -rf "$APP/$n"
        mv "$f" "$APP/$n"
      fi
    done
  fi
}

cleanup() {
  rc=$?
  set +e
  if [ "$rc" -ne 0 ]; then
    restore_app
    if [ "$VENV_NEW" = 1 ]; then
      rm -rf "$CS_DIR/.venv"
    elif [ -n "$FREEZE" ] && [ -s "$FREEZE" ] && [ "$CS_ACTION" = "update_full" ]; then
      say "restoring previous Python packages"
      if [ -n "$CS_PIP_INDEX" ]; then
        "$VPY" -m pip install -q -i "$CS_PIP_INDEX" -r "$FREEZE" >/dev/null 2>&1 || say "could not restore packages; run repair"
      else
        "$VPY" -m pip install -q -r "$FREEZE" >/dev/null 2>&1 || say "could not restore packages; run repair"
      fi
    fi
  fi
  rm -rf "$BK" "$STAGE" "$UPLOAD_DIR"
  [ -z "$FREEZE" ] || rm -f "$FREEZE"
  exit "$rc"
}
trap cleanup EXIT

command -v tar >/dev/null 2>&1 || die "tar is required on the server"
mkdir -p "$CS_DIR"
case "$CS_ACTION" in
  update_fast|update_full)
    [ -x "$VPY" ] || die "virtualenv missing; run repair"
    ;;
esac

PY=""
if [ "$CS_ACTION" = install ] || [ "$CS_ACTION" = repair ]; then NEED_PY=1; else NEED_PY=0; fi
if [ "$NEED_PY" = 1 ]; then
  if [ -n "$CS_PYTHON" ]; then
    cands="$CS_PYTHON"
  else
    cands="python3.12 python3.11 python3.10 python3 python"
  fi
  for c in $cands; do
    p=$(command -v "$c" 2>/dev/null || true)
    [ -n "$p" ] || continue
    if [ -z "$CS_PYTHON" ] && [ -n "${VIRTUAL_ENV:-}" ]; then
      case "$p" in
        "$VIRTUAL_ENV"/*) continue ;;
      esac
    fi
    if "$p" -c 'import sys; sys.exit(0 if sys.version_info >= (3, 10) else 1)' >/dev/null 2>&1; then
      PY="$p"
      break
    fi
  done
  [ -n "$PY" ] || die "Python 3.10+ not found (set a python path in the installer settings)"
  "$PY" -c 'import venv, ensurepip' >/dev/null 2>&1 || die "python venv/ensurepip module is missing for $PY"
  say "using python: $PY"
fi

rm -rf "$STAGE" "$BK"
mkdir -p "$STAGE" "$BK"
ARCHIVE="$STAGE/$CS_FILE"

if [ -n "$CS_UPLOADED" ]; then
  [ -f "$CS_UPLOADED" ] || die "uploaded archive is missing"
  say "using uploaded archive"
  cp "$CS_UPLOADED" "$ARCHIVE"
else
  ok=0
  for url in $CS_URLS; do
    say "downloading $url"
    if command -v curl >/dev/null 2>&1; then
      if curl -fsSL --connect-timeout 10 --max-time 600 -o "$ARCHIVE" "$url"; then ok=1; fi
    elif command -v wget >/dev/null 2>&1; then
      if wget -q --timeout=15 -O "$ARCHIVE" "$url"; then ok=1; fi
    else
      die "curl or wget is required on the server"
    fi
    if [ "$ok" = 1 ]; then break; fi
    say "download failed, trying next source"
  done
  if [ "$ok" != 1 ]; then
    printf '[installer] all download sources failed\n' >&2
    exit "$CS_EXIT_DOWNLOAD"
  fi
fi

if command -v sha256sum >/dev/null 2>&1; then
  GOT=$(sha256sum "$ARCHIVE" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  GOT=$(shasum -a 256 "$ARCHIVE" | awk '{print $1}')
elif command -v openssl >/dev/null 2>&1; then
  GOT=$(openssl dgst -sha256 "$ARCHIVE" | awk '{print $NF}')
else
  die "no sha256 tool (sha256sum, shasum or openssl) available"
fi
GOT=$(printf '%s' "$GOT" | tr 'A-F' 'a-f')
[ "$GOT" = "$CS_SHA" ] || die "sha256 mismatch for $CS_FILE; refusing to install"
say "checksum verified"

tar -xzf "$ARCHIVE" -C "$STAGE"
NEW="$STAGE/chemssh-$CS_VERSION"
[ -f "$NEW/backend/app/__init__.py" ] || die "unexpected archive layout"
grep -Fq "__version__ = \"$CS_VERSION\"" "$NEW/backend/app/__init__.py" || die "archive version does not match $CS_VERSION"

if [ "$CS_ACTION" = update_full ]; then
  FREEZE="$CS_DIR/.freeze.txt"
  "$VPY" -m pip freeze > "$FREEZE" 2>/dev/null || rm -f "$FREEZE"
fi

say "replacing application files"
mkdir -p "$APP"
SWAPPED=1
for item in backend frontend docs pyproject.toml README.md README.zh-CN.md constraints-oldlinux.txt LICENSE; do
  [ -e "$NEW/$item" ] || continue
  if [ -e "$APP/$item" ]; then mv "$APP/$item" "$BK/$item"; fi
  mv "$NEW/$item" "$APP/$item"
done
if [ -d "$NEW/plugins" ]; then
  mkdir -p "$APP/plugins" "$BK/plugins"
  for pentry in "$NEW/plugins"/*; do
    [ -e "$pentry" ] || continue
    pname=$(basename "$pentry")
    if [ -e "$APP/plugins/$pname" ]; then
      mv "$APP/plugins/$pname" "$BK/plugins/$pname"
    fi
    mv "$pentry" "$APP/plugins/$pname"
    if [ -f "$BK/plugins/$pname/.chemssh-plugin-state.json" ] && [ -d "$APP/plugins/$pname" ]; then
      cp "$BK/plugins/$pname/.chemssh-plugin-state.json" "$APP/plugins/$pname/.chemssh-plugin-state.json"
    fi
  done
fi
if [ ! -f "$CS_DIR/config.yaml" ] && [ -f "$NEW/config.yaml" ]; then
  cp "$NEW/config.yaml" "$CS_DIR/config.yaml"
  say "created default config.yaml"
fi

case "$CS_ACTION" in
  install|repair)
    rm -rf "$CS_DIR/.venv"
    say "creating virtualenv"
    "$PY" -m venv "$CS_DIR/.venv"
    VENV_NEW=1
    ;;
esac

case "$CS_ACTION" in
  install|repair|update_full)
    set -- install
    if [ -n "$CS_PIP_INDEX" ]; then set -- "$@" -i "$CS_PIP_INDEX"; fi
    PYOK=$("$VPY" -c 'import sys; print(1 if sys.version_info < (3, 13) else 0)' 2>/dev/null || echo 0)
    GLIBC=$(ldd --version 2>&1 | head -n 1 | sed -n 's/.* \([0-9][0-9]*\.[0-9][0-9]*\)$/\1/p')
    if [ "$PYOK" = 1 ] && [ -n "$GLIBC" ] && [ -f "$APP/constraints-oldlinux.txt" ] \
       && awk -v v="$GLIBC" 'BEGIN{split(v,a,"."); exit !(a[1]+0 < 2 || (a[1]+0 == 2 && a[2]+0 < 28))}'; then
      say "old glibc $GLIBC detected, using constraints-oldlinux.txt"
      set -- "$@" -c "$APP/constraints-oldlinux.txt"
    fi
    say "installing Python dependencies (this can take a few minutes)"
    if [ -n "$CS_PIP_INDEX" ]; then
      "$VPY" -m pip install --upgrade pip -i "$CS_PIP_INDEX" >/dev/null 2>&1 || say "pip upgrade skipped"
    else
      "$VPY" -m pip install --upgrade pip >/dev/null 2>&1 || say "pip upgrade skipped"
    fi
    (cd "$APP" && "$VPY" -m pip "$@" -e .)
    [ -e "$CS_DIR/.venv/bin/chemssh" ] || die "chemssh entry point was not installed"
    ;;
esac

VER=$(cd "$APP" && "$VPY" -c 'import backend.app as a; print(a.__version__)')
[ "$VER" = "$CS_VERSION" ] || die "installed version $VER does not match $CS_VERSION"

printf '{"version":"%s","tag":"%s","deps_fingerprint":"%s","installed_at":"%s"}\n' \
  "$CS_VERSION" "$CS_TAG" "$CS_FPRINT" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$CS_DIR/install.json.tmp"
mv "$CS_DIR/install.json.tmp" "$CS_DIR/install.json"
say "ChemSSH $CS_VERSION is ready"
"#;

#[cfg(test)]
mod tests {
    use super::*;
    use crate::manifest::{ArchiveFile, Manifest};

    fn fixture() -> (Manifest, InstallerOptions, InstallDir) {
        let m = Manifest {
            version: "0.3.11".into(),
            tag: "v0.3.11".into(),
            deps_fingerprint: "a".repeat(64),
            files: vec![ArchiveFile { name: "chemssh-0.3.11.tar.gz".into(), sha256: "b".repeat(64) }],
        };
        (m, InstallerOptions::default(), InstallDir::parse("~/.chemssh").unwrap())
    }

    fn script(action: Action, pre: &str, env: &[EnvPair]) -> String {
        let (m, o, d) = fixture();
        let urls = vec!["https://example.com/a.tar.gz".to_string()];
        build_install_script(&ScriptParams {
            dir: &d,
            action,
            manifest: &m,
            archive: m.archive().unwrap(),
            urls: &urls,
            uploaded: None,
            options: &o,
            pre_start: pre,
            proxy_env: env,
        })
    }

    #[test]
    fn proxy_exports_follow_set_e_and_precede_pre_start() {
        let env = vec![EnvPair { key: "http_proxy", value: "http://127.0.0.1:40000".into() }];
        let s = script(Action::Install, "module load python\nconda activate base", &env);
        let set_e = s.find("set -e\n").unwrap();
        let proxy = s.find("export http_proxy='http://127.0.0.1:40000'").unwrap();
        let pre = s.find("module load python\nconda activate base\n").unwrap();
        let body = s.find("CS_DIR=").unwrap();
        assert!(set_e < proxy && proxy < pre && pre < body);
    }

    #[test]
    fn no_proxy_exports_when_not_enabled() {
        let s = script(Action::UpdateFast, "", &[]);
        assert!(!s.contains("export http_proxy"));
        assert!(s.contains("CS_ACTION='update_fast'"));
    }

    #[test]
    fn values_are_quoted_and_config_is_never_overwritten() {
        let s = script(Action::UpdateFull, "", &[]);
        assert!(s.contains(&format!("CS_SHA='{}'", "b".repeat(64))));
        assert!(s.contains("CS_DIR=\"$HOME/.chemssh\""));
        // config.yaml is only created when missing
        assert!(s.contains("[ ! -f \"$CS_DIR/config.yaml\" ]"));
        // the virtualenv is only recreated for install and repair
        assert!(s.contains("install|repair)\n    rm -rf \"$CS_DIR/.venv\""));
    }

    #[test]
    fn quote_escapes_single_quotes() {
        assert_eq!(shell_quote("a'b"), "'a'\\''b'");
    }

    #[test]
    fn body_is_valid_posix_shell() {
        let s = script(Action::Install, "", &[]);
        let out = std::process::Command::new("sh").arg("-n").arg("-c").arg(&s).output();
        if let Ok(out) = out {
            assert!(out.status.success(), "{}", String::from_utf8_lossy(&out.stderr));
        }
    }

    #[test]
    fn probe_reads_install_json() {
        let d = InstallDir::parse("~/.chemssh").unwrap();
        let s = build_probe_script(&d);
        assert!(s.contains("$CS_DIR/install.json"));
        assert!(s.contains("__RECORD__") && s.contains("__END__"));
    }
}
