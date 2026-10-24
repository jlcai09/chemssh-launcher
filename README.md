# ChemSSH Launcher

English | [中文](README.zh-CN.md)

ChemSSH Launcher is a desktop-friendly launcher and file-transfer companion for ChemSSH. It helps you start ChemSSH on a remote SSH server, expose it through a local tunnel, open it in a browser or WebView2 window, and manage related files through an SFTP page.

At runtime the launcher is a single Go executable. It does not require Python, Electron, a system `ssh` command, or a local ChemSSH installation unless you are using a local debug profile.

## Install

Use a built executable for your platform, or build it from source.

Typical Windows executables are:

- `chemssh-launcher.exe`: browser-based launcher.
- `chemssh-launcher-webview2.exe`: optional Windows build that opens the launcher and ChemSSH in embedded WebView2 windows.

The WebView2 build requires the Microsoft Edge WebView2 Runtime on the machine that runs it. Windows 10/11 usually already includes it.

<details>
<summary>Building from Source (Developers)</summary>

To build from source, install:

- Go 1.23 or newer.
- Node.js and npm for the Vue frontend build.
- Optional: `go-winres` for offline Windows icon/version resource generation.

```bash
go install github.com/tc-hib/go-winres@v0.3.3
```

</details>

## Start

Double-click the executable, or run:

```bash
chemssh-launcher
```

This starts a local GUI server bound to `127.0.0.1` on a random free port and opens the launcher page. From there you can create profiles, store SSH secrets, test connections, start ChemSSH, stop forwarding or the remote service, open SFTP, inspect logs, export/import profiles, and manage local cache directories.

For the WebView2 build:

```bash
chemssh-launcher-webview2.exe
```

Useful GUI flags:

```bash
chemssh-launcher --browser
chemssh-launcher --webview
chemssh-launcher --devtools
chemssh-launcher --version
```

Environment overrides:

- `CHEMSSH_LAUNCHER_WEBVIEW=1` or `0`: enable or disable embedded WebView mode when the build supports it.
- `CHEMSSH_LAUNCHER_DEVTOOLS=1`: enable WebView2 DevTools for debugging.
- `CHEMSSH_LAUNCHER_VAULT_PASSWORD=...`: forces the encrypted local vault, even when the OS credential store is available (secrets already saved in the keyring are not read in this mode).

## First Run

1. Open the launcher.
2. Create a remote profile with SSH host, port, username, and authentication method.
3. Adjust the ChemSSH remote bind address if needed. The default is `127.0.0.1:8888`.
4. Keep the local tunnel bind on `127.0.0.1` unless you intentionally want to expose it.
5. Test SSH. The first connection to an unknown host asks you to verify and accept the host key fingerprint.
6. Start the profile. The launcher starts ChemSSH remotely when needed, starts the SSH tunnel, waits for the health URL, and opens ChemSSH if the profile enables auto-open.

If the remote ChemSSH port is already serving a reusable ChemSSH instance, the launcher can reuse it and only start forwarding instead of launching another server.

## Core Features

- Remote ChemSSH startup over SSH with password or private-key authentication.
- Multi-line pre-start and start commands executed in one remote shell session.
- SSH local port forwarding with local/remote port checks and suggested local fallback ports.
- Browser mode and optional Windows WebView2 mode.
- Same-origin launcher proxy for embedded ChemSSH (`/<profile-name>/chemssh`; the bare `/chemssh` path works too), including WebView2-friendly file dragging.
- Remote and local profile support.
- Profile-scoped logs plus launcher/backend logs.
- OS credential-store secrets, encrypted vault fallback, and OpenSSH-compatible `known_hosts`.
- Standalone SSH debug terminal at `/terminal` that works before ChemSSH is installed, with an Xshell-style server sidebar.
- Drag files or folders from the OS into the SSH terminal, or use the upload button in the bottom status bar, to upload them over SFTP with a progress ring.
- Standalone SFTP page at `/sftp`.
- Xftp-style two-pane file manager with local and remote tabs.
- File operations: list, open, open in text editor, create file, create directory, rename, delete, copy path.
- File transfers between local and remote panes, remote and remote panes, and local panes.
- Transfer queue with progress, speed/ETA, grouped folder transfers, pause, resume, and cancel.
- Remote file open cache with automatic upload-back sync after local edits.
- System file icons where supported, with fallback icons elsewhere.
- Backend page for config/cache paths, profile export/import, and cache cleanup.

## Profiles

Profiles are stored locally and can be remote or local.

### Remote Profiles

Remote profiles connect through SSH and can start ChemSSH on the server.

Important fields:

- `SSH Host`, `SSH Port`, `SSH User`: SSH target.
- `Auth Method`: `Password` or `Private Key`. When `Password` is selected, the launcher sends both the RFC 4252 `password` method and the RFC 4256 `keyboard-interactive` method, so servers that only allow `keyboard-interactive` (common on HPC login nodes) authenticate without extra configuration.
- `Remote Host`, `Remote Port`: where ChemSSH listens on the remote machine.
- `Local Host`, `Local Port`: where the local tunnel listens.
- `Local URL Path`: path opened in the browser after the local host/port.
- `Pre-start Commands`: optional commands run before the start command.
- `Start Command`: command that starts ChemSSH.
- `Health Check URL`: optional override; when empty, the launcher checks the local browser URL.
- `Open ChemSSH tab after start`: controls browser/WebView auto-open.
- `Security Token`: ChemSSH security token for token-authenticated ChemSSH instances. When set, the launcher injects the token into proxied API requests, terminal WebSocket connections, and health/identity checks so you do not need to enter it manually each time.

Saved secrets are displayed only as `*******`. Each secret has `Keep` / `Replace` / `Clear` actions: leaving the field empty with `Keep` changes nothing, typing a value switches to `Replace`, and only `Clear` removes the stored secret (an empty value with `Replace` stores an empty secret).

### Local Profiles

Local profiles are for local ChemSSH debugging. They do not use SSH, do not create an SFTP session, and do not create an SSH tunnel.

For a local profile, `Local Host`, `Local Port`, `Local URL Path`, `Pre-start Commands`, and `Start Command` describe a local ChemSSH process. If the local health URL is already reachable, the launcher reuses it. Otherwise it runs the configured local command with PowerShell on Windows or `sh -c` on Unix-like systems.

## Default Start Command

The default remote start command assumes the server has `bash`, a project `.venv`, and a `chemssh` command available:

```bash
source .venv/bin/activate
unset PS1
export CONDA_CHANGEPS1=false
export VIRTUAL_ENV_DISABLE_PROMPT=1
chemssh --config config.yaml
```

The launcher checks the **last line** of the start command and appends the flags to that line when it does not already contain `--host` or `--port`:

```bash
--host <Remote Host> --port <Remote Port>
```

## SSH Debug Terminal

The terminal page provides a basic interactive SSH shell for remote profiles without depending on ChemSSH:

```text
/terminal
```

It can be used before ChemSSH is installed to inspect the server, check paths, load modules, or run simple commands. The launcher opens a direct SSH connection and requests a remote PTY (`xterm-256color`); authentication and host-key verification use the same profile and `known_hosts` policy as normal sessions. Terminal size is forwarded on resize, and sessions are closed when you disconnect, delete the profile, or exit the launcher. Local profiles do not provide an SSH terminal.

The page follows an Xshell-style layout: a left sidebar lists the saved remote profiles, and clicking one connects it. If that server already has a session, the matching tab is activated; otherwise the active idle tab is reused or a new tab is opened. On narrow windows the sidebar collapses to a compact icon column. The clear and upload buttons, the `ssh://user@host:port` address, and the connection status live in the status bar at the bottom of the terminal.

Each tab holds its own SSH shell and transfer session. Click `+` in the tab strip to open another tab, right-click a tab to duplicate it, close it, or close all tabs, or double-click a tab to duplicate it. A duplicated tab reconnects to the same profile and runs `cd` into the directory the source tab was in, so parallel shells start in the same place. Drag a tab left or right to reorder the tab strip.

Drag files or folders from the OS file manager onto the terminal to upload them over SFTP, or click the upload button on the left side of the bottom status bar to pick files manually; the button shows a progress ring and percentage while uploading. The launcher tracks the shell's current working directory from a prompt marker (falling back to the remote home directory when unavailable), so uploads follow the directory you are currently in; relative subdirectories are created as needed, and existing items require confirmation. The SFTP session is tied to the active terminal and is released when the terminal disconnects.

The launcher also installs temporary `sz`/`rz` shims into the remote shell's `PATH`, mirroring ChemSSH's own terminal integration: `rz` opens the local file picker and uploads into the current directory, while `sz <files>` downloads those files through SFTP. The shims exit immediately after emitting a transfer marker, so they cannot leave the terminal waiting. If shell integration cannot be installed, native ZMODEM signatures are intercepted with Ctrl-C to keep the terminal usable. Drag-and-drop and the SFTP page remain the recommended transfer paths.

## SFTP And Local Files

The SFTP page is independent from ChemSSH startup. It can be opened from the launcher and served at:

```text
/sftp
```

Each side of the page can hold multiple tabs. A tab can browse the local filesystem or connect to a saved remote profile over SSH/SFTP. Remote SFTP tabs require a remote profile and successful host-key verification, but they do not start ChemSSH, run start commands, create tunnels, or depend on ChemSSH health checks. Right-click a tab to duplicate it, close it, or close all tabs, or double-click a tab to duplicate it; duplicating a remote tab reconnects to the same profile and opens the same directory. Drag tabs left or right to reorder them, drag a tab onto the other pane's tab strip or file area to move it to that side, and use `+` to reopen one after closing all tabs. The `+` menu is a flat list of the local filesystem plus every saved remote profile; clicking an entry opens a tab for that side directly and starts at the local home directory or the remote home directory, with no path prompt or extra confirmation.

Select files or directories and press `Delete` to remove them: local tabs move the entries to the system trash without a confirmation, while remote tabs mirror the context-menu delete and ask for confirmation first.

The path bar always shows an absolute path: relative input such as `.` or `user/data` is resolved through the server before the directory is listed, and copying a file path from the context menu copies the absolute path.

Symbolic links are resolved while a directory is listed: a link to a directory shows a folder icon with a link badge and opens on double-click, and the row tooltip shows the link target. Deleting a link removes the link itself, never the target.

Supported transfer paths include:

- Local to remote.
- Remote to local.
- Remote to remote through the launcher.
- Local to local through the launcher.
- External file/folder drop upload to a remote tab.

Remote files opened through `Open` or `Open in text editor` are downloaded to a local cache directory and opened with the OS file association; `Open in text editor` uses Notepad on Windows, TextEdit on macOS, and the system default app (xdg-open) on Linux. They are watched for changes and uploaded back to the remote path while the SFTP session remains connected. This cache is temporary and is cleaned when the launcher exits when possible.

Directory transfer support is implemented by expanding directories into file tasks. Some backend paths still only copy individual files, so a local-to-local directory transfer may fail with `directory transfer is not supported yet`.

## ChemSSH Bridge

The launcher exposes bridge endpoints for ChemSSH at:

```text
/api/chemssh-bridge/*
```

These endpoints let the proxied ChemSSH page discover launcher capabilities, request system file icons, open workspace files with local applications, open text files, and poll upload-back sync events.

Bridge features are conditional:

- ChemSSH must be opened through the active launcher's proxy (the launcher opens `/<profile-name>/chemssh`; the bare `/chemssh` path works as well).
- The active session must still be forwarding.
- ChemSSH identity must be readable and must include a workspace root.
- Remote bridge open uses a dedicated SFTP session for the active remote profile.
- Local profiles can open files only inside the local ChemSSH workspace and do not provide SFTP bridge open.

The bridge is intentionally workspace-scoped. Paths outside the active ChemSSH workspace are rejected.

## WebView2 And Browser Mode

Browser mode opens the launcher's local URL in the system browser.

WebView2 mode opens `/shell` in an embedded window. Normal WebView2 mode disables DevTools shortcuts and intercepts `Ctrl+Shift+C`; use `--devtools` only while debugging. WebView2 close interception can warn when transfer tasks are still running.

In WebView2 mode, ChemSSH is opened through the launcher's same-origin proxy:

```text
http://127.0.0.1:<launcher-port>/<profile-name>/chemssh
```

not directly through:

```text
http://127.0.0.1:8888
```

This avoids cross-origin iframe drag limitations in WebView2 and lets the launcher proxy participate in ChemSSH file actions.

## Storage

Non-secret profile data is saved as JSON:

- Windows: `%AppData%\ChemSSHLauncher\profiles.json`
- macOS: `~/Library/Application Support/ChemSSHLauncher/profiles.json`
- Linux: `~/.config/chemssh-launcher/profiles.json`

Passwords, private-key passphrases, and security tokens are not written to `profiles.json`. By default they are stored in the OS credential store:

- service: `chemssh-launcher`
- username: `<profile-id>:password`
- username: `<profile-id>:key-passphrase`
- username: `<profile-id>:security-token`

If the OS keyring is unavailable, set `CHEMSSH_LAUNCHER_VAULT_PASSWORD` to use the encrypted local vault instead (setting the variable always selects the vault, there is no automatic fallback, and secrets already saved in the keyring are not migrated):

- Windows: `%AppData%\ChemSSHLauncher\vault.json`
- macOS: `~/Library/Application Support/ChemSSHLauncher/vault.json`
- Linux: `~/.config/chemssh-launcher/vault.json`

The vault password is never stored by the app.

SSH host keys are stored in an OpenSSH-compatible file:

- Windows: `%AppData%\ChemSSHLauncher\known_hosts`
- macOS: `~/Library/Application Support/ChemSSHLauncher/known_hosts`
- Linux: `~/.config/chemssh-launcher/known_hosts`

The config directory also holds `client_identity.json`: the launcher's stable client id, sent as the `X-ChemSSH-Client-Id` header when proxying ChemSSH (the terminal WebSocket uses the `client_id` query parameter) so ChemSSH can isolate per-client session caches.

WebView2 data and temporary SFTP-open cache paths are shown in the Backend page. On Windows, cache cleanup can be delayed by file locks; the launcher records a pending cleanup marker and retries on the next start.

## Security Notes

ChemSSH Launcher relies on SSH for authentication, encryption, host-key verification, and local port forwarding. The launcher manages profile storage, secret lookup, remote/local process startup, SFTP sessions, and local HTTP proxying.

On the first connection to a server, the GUI/CLI shows the host key type and SHA256 fingerprint. Trust it only after comparing it with a reliable source, such as a server administrator or an existing trusted SSH client. If a saved host key changes later, the GUI blocks the connection and shows both the saved and the new fingerprint; the old key is replaced in `known_hosts` only after you verify the new fingerprint and explicitly confirm. The CLI continues to reject changed keys.

Binding `Local Host` to anything other than loopback, such as `0.0.0.0`, can expose the tunnel or local ChemSSH target to other machines. The launcher logs a warning, but the setting is still your responsibility.

The launcher's local GUI API additionally protects write operations (non-GET requests): every launch generates a random token, stores it in the `chemssh_launcher_token` cookie, and requires write requests to carry a matching `X-ChemSSH-Launcher-Token` header plus a valid Origin/Sec-Fetch-Site. The launcher's own frontend sends that header itself; for proxied ChemSSH pages the launcher injects a small script that reads the cookie and fills the header in, so bridge write endpoints (such as opening a file locally) need no extra configuration.

---

<details>
<summary>Architecture (Developers)</summary>

The project combines a Go backend, embedded Vue frontend, SSH/SFTP clients, and optional WebView2 host:

```text
chemssh-launcher executable
  Go GUI server on 127.0.0.1:<random>
    Vue app: /, /shell, /terminal, /sftp, /launcher-logs
    Profile/session APIs: /api/profiles, /api/session/*, /api/session/shell/*
    SFTP/local file APIs: /api/sftp/*, /api/local/*
    ChemSSH proxy: /<profile-name>/chemssh (bare /chemssh also accepted)
    ChemSSH bridge: /api/chemssh-bridge/*
  SSH client
    remote command startup
    interactive PTY shell
    local port forwarding
    host-key verification
  SFTP client manager
    reusable SFTP tabs
    dedicated bridge SFTP session
    open-cache sync watcher
  Browser or WebView2 host
```

The frontend is built with Vue 3, TypeScript, Vite, and Element Plus. The production build is generated into `internal/gui/static/vue/` and embedded into the Go executable.

</details>

<details>
<summary>Directory Structure (Developers)</summary>

```text
cmd/chemssh-launcher/      CLI entrypoint and Windows resource object
frontend/                 Vue 3 frontend, Vite config, package metadata
frontend/src/views/       Launcher, SFTP, shell, terminal, and backend views
internal/app/             Higher-level app/session helpers
internal/browser/         Browser and OS file-opening helpers
internal/chemssh/         ChemSSH identity discovery
internal/config/          Profiles, defaults, paths, known app dirs
internal/fileicon/        System/fallback file icon service
internal/gui/             Local GUI HTTP server, APIs, proxy, bridge, SFTP/local file handlers
internal/netcheck/        Port and health-check helpers
internal/secret/          OS keyring, encrypted vault, memory test store
internal/sftpclient/      SFTP session manager and file operations
internal/sshclient/       SSH auth, host keys, tunnels, remote commands
internal/version/         Version source and tests
internal/webview/         Optional WebView2 integration
tools/build/              Build orchestration for frontend, resources, and Go binary
winres/                   Windows icon and version resource metadata
idea/                     Local development notes (gitignored, not shipped with the repository)
```

</details>

<details>
<summary>Development Guide (Developers)</summary>

Install frontend dependencies once:

```bash
cd frontend
npm install
```

Run frontend type-check/build:

```bash
npm run build
```

Run Go tests:

```bash
go test ./...
```

Build the launcher from the repository root:

```bash
go run ./tools/build
```

The build tool reads `internal/version/VERSION`, syncs `winres/winres.json`, builds the Vue frontend, regenerates Windows resources when needed, and runs `go build ./cmd/chemssh-launcher`.

Build optional Windows WebView2 variants:

```bash
go run ./tools/build --webview2                   # WebView2 with console (for debugging)
go run ./tools/build --webview2 --windowsgui      # WebView2 without console (recommended for release)
```

The build tool optimizes for size and startup speed by default (using `-trimpath -ldflags="-s -w"`). Use `--no-optimize` to disable optimizations for debugging.

**Build variants**:
- **Standard (browser mode)**: Opens in system browser, shows console for debugging
- **Standard + `--windowsgui`**: Opens in system browser, no console window (clean for end users)
- **WebView2**: Embedded browser window with console (for debugging startup issues)
- **WebView2 + `--windowsgui`**: Embedded browser window, no console (best user experience)

For local frontend iteration you can run Vite:

```bash
cd frontend
npm run dev
```

The production executable serves embedded assets, so remember to run `npm run build` or `go run ./tools/build` before testing the packaged Go server.

</details>

<details>
<summary>CLI Commands (Developers)</summary>

The GUI is the primary interface. CLI commands remain useful for debugging and automation:

```bash
chemssh-launcher profile list
chemssh-launcher profile add
chemssh-launcher profile edit <name-or-id>
chemssh-launcher profile delete <name-or-id>
chemssh-launcher profile test <name-or-id>
chemssh-launcher start <name-or-id>
```

</details>

<details>
<summary>Version Management (Developers)</summary>

Only edit this file when bumping the app version:

```text
internal/version/VERSION
```

`go run ./tools/build` syncs that version into Windows resource metadata. The version is shown in the GUI top bar (the brand block in the upper-left corner) and through:

```bash
chemssh-launcher --version
```

Before releasing, make sure `internal/version/VERSION` is updated and matches the git tag and commit message (the build tool only syncs `winres/winres.json`, it never edits VERSION).

</details>

<details>
<summary>Known Limits And Notes</summary>

- Runtime is a single executable, but source builds require Node/npm because the frontend is now a Vue/Vite app.
- SFTP works only for remote profiles. Local profiles are for local ChemSSH debugging and do not provide SSH/SFTP.
- Bridge file opening requires an active proxied ChemSSH session and a readable ChemSSH identity with `workspace_root`.
- The launcher only controls ChemSSH processes it starts or can identify by ChemSSH identity. If PID discovery fails, stopping the service may leave a remote ChemSSH process running.
- Remote and local recursive deletion is supported by the backend, so delete confirmations should be read carefully.
- Temporary SFTP-open cache cleanup can be blocked by Windows file locks; this is an environment cleanup issue, not necessarily a test or code failure.
- The launcher tracks browser page heartbeats: once the launcher page is closed (about 75 seconds without a heartbeat plus a 10 second grace period), it stops session resources, closes tunnels/SFTP/bridge, and exits.
- `go.sum` is intentionally committed. It locks Go module checksums for reproducible and tamper-resistant builds.

</details>
