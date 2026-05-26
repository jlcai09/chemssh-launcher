# Chemweb Launcher

English | [中文](README.zh-CN.md)

Chemweb Launcher is a small Go GUI launcher for running Chemweb on a remote server over SSH while browsing it locally.

The executable opens a local browser-based GUI when started without arguments. It manages local profiles, starts remote multi-line setup commands plus the Chemweb command in one shell session, opens an SSH local tunnel, checks the local URL, and can open the browser when the service is reachable.

The GUI is served from the same executable on `127.0.0.1` and uses no Python, Node, Electron, or system `ssh` runtime.

## GUI Usage

```bash
chemweb-launcher
```

This starts the local GUI server and opens the browser automatically. Use the page to create profiles, store secrets, test SSH, start Chemweb, stop the tunnel, and inspect session logs.

## CLI Commands

```bash
chemweb-launcher profile list
chemweb-launcher profile add
chemweb-launcher profile edit <name-or-id>
chemweb-launcher profile delete <name-or-id>
chemweb-launcher profile test <name-or-id>
chemweb-launcher start <name-or-id>
```

## Build

```bash
go build ./cmd/chemweb-launcher
```

The intended output is a single executable per platform. On Windows, use the generated `chemweb-launcher.exe` as the GUI launcher.

## Version

Current version: `0.1.0`

The root shortcut is:

```text
VERSION
```

The version embedded at compile time is mirrored in:

```text
internal/version/VERSION
```

Keep both files in sync before building a new release; tests will fail if they differ. The version is shown in the GUI sidebar and through:

```bash
chemweb-launcher --version
```

## Defaults

- SSH port: `22`
- Remote Chemweb bind: `127.0.0.1:8888`
- Local tunnel bind: `127.0.0.1:8888`
- Local URL path: `/`
- Start command: a multi-line bash script that activates `.venv`, prepares the prompt, and ends with `chemweb --config config.yaml`
- Browser auto-open: enabled

The launcher appends `--host <Remote Host> --port <Remote Port>` automatically when the start command does not already include those flags.

The default start command assumes the remote machine has `bash` and a project `.venv`:

```bash
source .venv/bin/activate
__chemweb_conda_prompt=""
if [ -n "${CONDA_DEFAULT_ENV:-}" ]; then
  __chemweb_conda_prompt="(${CONDA_DEFAULT_ENV}) "
fi

__chemweb_venv_prompt=""
if [ -n "${VIRTUAL_ENV:-}" ]; then
  __chemweb_venv_prompt="($(basename "$VIRTUAL_ENV")) "
fi

__chemweb_prompt_char="$"
if [ "$(id -u)" = "0" ]; then
  __chemweb_prompt_char="#"
fi

export PS1="${__chemweb_conda_prompt}${__chemweb_venv_prompt}[\u@\h \W]${__chemweb_prompt_char} "
chemweb --config config.yaml
```

In the GUI, saved secrets are displayed only as `*******`. Click `Edit` to replace a secret; leave the edited field empty and save to clear it.

## Storage

Non-secret SSH/profile data is saved as JSON:

- Windows: `%AppData%\ChemwebLauncher\profiles.json`
- macOS: `~/Library/Application Support/ChemwebLauncher/profiles.json`
- Linux: `~/.config/chemweb-launcher/profiles.json`

Passwords and private key passphrases are not stored in that JSON file. By default they are stored in the OS credential store with:

- service: `chemweb-launcher`
- username: `<profile-id>:password`
- username: `<profile-id>:key-passphrase`

If an OS keyring is unavailable, set `CHEMWEB_LAUNCHER_VAULT_PASSWORD` to use the encrypted local vault instead:

- Windows: `%AppData%\ChemwebLauncher\vault.json`
- macOS: `~/Library/Application Support/ChemwebLauncher/vault.json`
- Linux: `~/.config/chemweb-launcher/vault.json`

The vault password is never stored by the app.

SSH host keys are verified with an OpenSSH-compatible `known_hosts` file:

- Windows: `%AppData%\ChemwebLauncher\known_hosts`
- macOS: `~/Library/Application Support/ChemwebLauncher/known_hosts`
- Linux: `~/.config/chemweb-launcher/known_hosts`

On the first connection to a server, the GUI/CLI shows the host key type and SHA256 fingerprint. Trust it only after comparing it with a reliable source, such as the server administrator or an existing trusted SSH client. If a saved host key changes later, Chemweb Launcher blocks the connection because that can indicate a server reinstall, DNS/host change, or a man-in-the-middle attack.

## Security Model

Chemweb Launcher relies on SSH for transport security, authentication, encryption, and local port forwarding. The launcher itself manages profiles, stores secrets, starts the remote Chemweb process, and opens the tunnel.

Secrets are stored in the OS credential manager by default, which is safer than inventing app-local encryption. The fallback vault is available only when `CHEMWEB_LAUNCHER_VAULT_PASSWORD` is supplied, and that password is not stored by the app.

## Repository Notes

`go.sum` is intentionally committed. It locks module checksums for reproducible and tamper-resistant Go builds, so it should not be added to `.gitignore`.
