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

[Building from Source (Developers)](#development-guide): see the development guide below for prerequisites, dependency installation, build commands, and troubleshooting.

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
- Optional reverse channel that lets the remote machine use a proxy running on your computer, including ChemSSH's own terminal tabs (see [Local Network Proxy](#local-network-proxy)).
- Browser mode and optional Windows WebView2 mode.
- Tabbed `/shell` workspace whose pinned first tab is the launcher page; `+` opens a blank tab with shortcuts to Launcher, Terminal, SFTP, and Backend.
- Same-origin launcher proxy for embedded ChemSSH (`/<profile-name>/chemssh`; the bare `/chemssh` path works too), including WebView2-friendly file dragging.
- Remote and local profile support.
- Server-profile sidebar with drag-to-reorder and a right-click menu for duplicating, deleting, and opening a remote server in SFTP.
- Profile-scoped logs plus launcher/backend logs, with keyword filtering, copy-all, and export.
- OS credential-store secrets, encrypted vault fallback, and OpenSSH-compatible `known_hosts`.
- Standalone SSH debug terminal at `/terminal` that works before ChemSSH is installed, with an Xshell-style server sidebar.
- Drag files or folders from the OS into the SSH terminal, or use the upload button in the bottom status bar, to upload them over SFTP with a progress ring.
- Standalone SFTP page at `/sftp`.
- Xftp-style two-pane file manager with local and remote tabs, including dragging a multi-selection of files and folders out to the OS.
- File operations: list, open, open in text editor, create file, create directory, rename, delete, copy path.
- File transfers between local and remote panes, remote and remote panes, and local panes.
- Transfer queue with progress, speed/ETA, grouped folder transfers, pause, resume, cancel, byte-offset resume, and automatic retry.
- Remote file open cache with automatic upload-back sync after local edits.
- System file icons where supported, with fallback icons elsewhere.
- Backend page for interface language and version, config/cache paths, profile export/import, and cache cleanup.
- Interface language follows the system language by default and can be fixed to Chinese or English; the preference persists across launches.

## Profiles

Profiles are stored locally and can be remote or local. They appear as cards in the launcher sidebar: drag a card to reorder the list, and the new order is saved with the profile store. Right-click a card to duplicate the profile (including its stored secrets), delete it, or open the server in SFTP. Opening in SFTP reuses the `/shell` tab that already shows the SFTP page and appends that server as a pane tab there; a new SFTP tab is created only when none is open. Local profiles do not support this action.

`Duplicate profile` copies the connection settings and stored password, passphrase, and security token without revealing their values. The copy is appended to the list and selected for editing.

### Remote Profiles

Remote profiles connect through SSH and can start ChemSSH on the server.

Important fields:

- `SSH Host`, `SSH Port`, `SSH User`: SSH target.
- `Auth Method`: `Password` or `Private Key`. When `Password` is selected, the launcher sends both the RFC 4252 `password` method and the RFC 4256 `keyboard-interactive` method, so servers that only allow `keyboard-interactive` (common on HPC login nodes) authenticate without extra configuration.
- `Remote Host`, `Remote Port`: where ChemSSH listens on the remote machine.
- `Local Host`, `Local Port`: where the local tunnel listens.
- `Local URL Path`: path opened in the browser after the local host/port.
- `Local Network Proxy`: reverse-forwards a proxy that runs on this computer so remote processes can use it (see [Local Network Proxy](#local-network-proxy)).
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

The page follows an Xshell-style layout: a left sidebar lists the saved remote profiles; clicking one selects it, and double-clicking one connects it. The refresh button in the sidebar header reloads the profile list, so profiles added, renamed, or deleted on the launcher page show up without reloading the terminal page. Every double-click opens a new tab, even when that server already has a connected session; only the active tab that was never connected is filled in, so the placeholder tab does not linger beside the new session. On narrow windows the sidebar collapses to a compact icon column. The clear and upload buttons, the `ssh://user@host:port` address, and the connection status live in the status bar at the bottom of the terminal.

The left column stacks two collapsible sections, the server list and an SFTP browser for the active terminal tab, and each header toggles its own section. Drag the divider between the sidebar and the terminal to change the sidebar width; the width, the collapsed server list, and the SFTP sync preference are remembered. Every new terminal tab starts with its SFTP browser collapsed, so a tab never opens an extra SFTP channel on its own: expanding it connects (or reuses) an SFTP session for that tab, and collapsing it releases that channel again unless an upload is still running. The browser's sync button links it to the shell in both directions: changing directory in the terminal reloads the browser, and entering a directory in the browser (double-click, the up button, or a typed path) sends the matching `cd` to the shell. Double-clicking a file behaves like the SFTP page's **Open**: the launcher caches the file locally, opens it with the OS file association, and uploads each save back to the remote path — the channel stays open for that even when the browser is collapsed, until the terminal tab closes, and files over 100 MB are refused as on the SFTP page. The arrow next to the path walks back through the directories you visited, and a `..` row at the top of the list opens the parent directory. Dragging a row onto Explorer or the desktop saves that file locally through the OS drag target (Chromium only accepts one `DownloadURL`, so a drag carries a single file), and dropping files or folders from the OS onto the panel uploads them into the directory it currently shows. In the narrow and stacked layouts the sidebar keeps only the server column.

Each tab holds its own SSH shell and transfer session. Click `+` in the tab strip to open another tab, right-click a tab to duplicate it, open SFTP, close it, or close all tabs, or double-click a tab to duplicate it. A duplicated tab reconnects to the same profile and runs `cd` into the directory the source tab was in, so parallel shells start in the same place. **Open SFTP** (or `Ctrl+Alt+F`) opens the SFTP page for the same profile and enters the directory that shell is currently in, instead of the remote home directory the sidebar and launcher entry points use; the shortcut always appends a new tab, even when the same directory is already open. Drag a tab left or right to reorder the tab strip.

Drag files or folders from the OS file manager onto the terminal to upload them over SFTP, or click the upload button on the left side of the bottom status bar to pick files manually; the button shows a progress ring and percentage while uploading. The launcher tracks the shell's current working directory from a prompt marker (falling back to the remote home directory when unavailable), so uploads follow the directory you are currently in; relative subdirectories are created as needed, and existing items require confirmation. The SFTP session is tied to the active terminal and is released when the terminal disconnects.

The launcher also installs temporary `sz`/`rz` shims into the remote shell's `PATH`, mirroring ChemSSH's own terminal integration: `rz` opens the local file picker and uploads into the current directory, while `sz <files>` downloads those files through SFTP. The shims exit immediately after emitting a transfer marker, so they cannot leave the terminal waiting. If shell integration cannot be installed, native ZMODEM signatures are intercepted with Ctrl-C to keep the terminal usable. Temporary `vi`/`vim` wrappers resolve the real executable outside the shim directory and disable Vim's terminal capability queries, matching ChemSSH's workaround for editors that hang in browser terminals. Drag-and-drop and the SFTP page remain the recommended transfer paths.

### X11 forwarding to a local X server

Remote profiles can enable **X11 forwarding** for newly opened launcher SSH terminals. The Go SSH backend requests forwarding before starting the shell, validates incoming X11 authentication, and carries graphical traffic to the local X server. The remote SSH server sets `DISPLAY`; do not hardcode it in pre-start commands. Graphical windows open in VcXsrv or another local X server rather than inside the terminal canvas.

Start VcXsrv or another local X server yourself before opening the launcher SSH terminal. The launcher only forwards to an already running X server; it does not start or manage one. Enable **X11 forwarding** and set the local TCP display to match your server (default `127.0.0.1:0`). The server must accept TCP connections from the launcher on that display.

The **Xauthority file** is optional. Leave it empty to try `XAUTHORITY` / the default home authority file, or select the matching file if your server uses `MIT-MAGIC-COOKIE-1`. If your X server's existing access policy allows local connections without a cookie, forwarding can use that policy too. No cookie needs to be entered; the profile stores only the authority file path. Keep your existing server access controls and firewall restrictions appropriate for local access. This is trusted X11 forwarding: enable it only for remote machines and graphical programs you trust.

The remote machine needs `xauth`, and its SSH server must permit `X11Forwarding`. After saving the profile, disconnect and open a new launcher terminal; old shells do not acquire forwarding retroactively. The terminal shows whether X11 is active or why it could not start. Check `echo "$DISPLAY"` and run `ase gui example.traj` or `xclock`. If the SSH server accepts the request but a login script clears `DISPLAY`, fix that script rather than assigning a display number manually. Programs launched on another compute node or inside a container need their own display and authentication routing.

X11 startup failures leave the text terminal usable. Closing the terminal closes its X11 channels. These settings apply to the launcher's SSH terminals; ChemSSH's own separately created terminal processes do not inherit this forwarding automatically.

## SFTP And Local Files

The SFTP page is independent from ChemSSH startup. It can be opened from the launcher and served at:

```text
/sftp
```

On a remote tab, select files and folders with Ctrl/Shift or range selection, then hold the left mouse button on a selected row for about half a second before dragging. The Windows WebView2 desktop build uses native Windows OLE asynchronous drag-and-drop for single files, multiple files and folders when the system target supports asynchronous extraction. After the target accepts the drop, releasing the mouse restores normal launcher interaction. A background job recursively caches the selection over SFTP and the system copies it into the destination it accepted. The system drag protocol determines the destination.

Drag jobs appear in the existing bottom SFTP transfer task list instead of a top-right popup, with actual cached bytes, progress, speed and elapsed time. Folder totals grow as files are discovered. Right-click selected tasks to pause, resume or cancel background caching, including a mixed selection with ordinary transfers. Pause takes effect at read checkpoints, so an in-flight chunk may still finish. Press Esc in the SFTP page to cancel the latest cache job, interrupt SFTP and remove incomplete copies. After caching, the row shows system delivery; the system owns that copy and the launcher no longer offers pause or cancellation. The row completes only after the system confirms delivery. The destination is labelled as the system drop destination, not the temporary cache path. Use the task list's existing clear action to remove finished tasks. Closing the launcher interrupts unfinished background caching. Targets without asynchronous extraction report failure rather than waiting synchronously for a large cache.

Dropping onto the other pane or a folder transfers the selection inside the launcher. Ordinary browser mode retains browser download drag for single files; dragging multiple files or folders out to the system requires the Windows WebView2 desktop build. Moving immediately after pressing a row selects a range; moving after the hold drags the current selection.

Each side of the page can hold multiple tabs. A tab can browse the local filesystem or connect to a saved remote profile over SSH/SFTP. Remote SFTP tabs require a remote profile and successful host-key verification, but they do not start ChemSSH, run start commands, create tunnels, or depend on ChemSSH health checks. Right-click a tab to duplicate it, close it, or close all tabs, or double-click a tab to duplicate it; duplicating a remote tab reconnects to the same profile and opens the same directory. Drag tabs left or right to reorder them, drag a tab onto the other pane's tab strip or file area to move it to that side, and use `+` to reopen one after closing all tabs. The `+` menu is a flat list of the local filesystem plus every saved remote profile; clicking an entry opens a tab for that side directly and starts at the local home directory or the remote home directory, with no path prompt or extra confirmation. The launcher's **Open in SFTP** command reuses the shell tab that already shows `/sftp` and appends that server as a right-pane tab connected to its remote home directory, focuses the existing tab when that server is already open there, and only falls back to `/sftp?profile=<id>&name=<name>` to create a new shell tab when no SFTP page is open. A terminal tab's **Open SFTP** (also `Ctrl+Alt+F`) adds `&path=<dir>` (or posts the same `path` to the SFTP page) so the tab enters the shell's directory instead; the context-menu command focuses a tab for that profile that already shows the directory and otherwise appends a new right-pane tab at that directory, while `Ctrl+Alt+F` always appends a new right-pane tab even when the same directory is already open, leaving the existing tab where it was. Requests are queued until the SFTP page reports it is ready.

Select files or directories and press `Delete` to remove them: local tabs move the entries to the system trash without a confirmation, while remote tabs mirror the context-menu delete and ask for confirmation first.

The path bar always shows an absolute path: relative input such as `.` or `user/data` is resolved through the server before the directory is listed, and copying a file path from the context menu copies the absolute path.

Symbolic links are resolved while a directory is listed: a link to a directory shows a folder icon with a link badge and opens on double-click, and the row tooltip shows the link target. Deleting a link removes the link itself, never the target.

Supported transfer paths include:

- Local to remote.
- Remote to local.
- Remote to remote through the launcher.
- Local to local through the launcher.
- External file/folder drop upload to a remote tab.

Single-file copies (local to remote, remote to remote, and local to local) resume by byte offset: the launcher only counts bytes whose write has been confirmed, and when a transfer breaks it re-dials SSH and retries with exponential backoff (up to 5 attempts, never waiting more than 15 seconds between them), continuing from the last confirmed offset instead of copying again from the start. While it is waiting, the transfer row shows `Retry n/5` and reports waiting data / estimating instead of a speed and ETA. Errors that cannot be recovered by retrying — permission denied, missing target, a full disk, a source file that changed mid-transfer — fail immediately, and the failed row can be retried from the transfer panel with the context menu (single row or multi-selection), still continuing from its offset. If the source file size or mtime changes during a transfer, the launcher stops and reports it rather than producing a corrupt file; a manual retry against a changed source restarts that file from zero. A cancelled task restarts from zero when it is retried. The browser file-picker upload and the browser download paths do not use this queue — a request body or a response stream cannot be replayed — so they support neither resume nor automatic retry and must be started again after an interruption.

`Open` acts on the whole selection: with several rows selected, every selected file is opened instead of only the row that was right-clicked, and the menu label shows the count.

Remote files opened through `Open` or `Open in text editor` are downloaded to a local cache directory and opened with the OS file association; `Open in text editor` uses Notepad on Windows, TextEdit on macOS, and the system default app (xdg-open) on Linux. They are watched for changes and uploaded back to the remote path while the SFTP session remains connected. This cache is temporary and is cleaned when the launcher exits when possible.

Directory transfer support is implemented by expanding directories into file tasks. Some backend paths still only copy individual files, so such a transfer fails with a directory-transfer error instead of being expanded.

## Local Network Proxy

ChemSSH binds to `127.0.0.1` on the remote machine, so a proxy that only exists on your own computer is out of reach there. Enabling `Local Network Proxy` in a remote profile carries that proxy over the existing SSH connection with reverse port forwarding:

```text
remote 127.0.0.1:<assigned port>  ->  ssh  ->  <Proxy Host>:<Proxy Port> on this computer
```

The launcher asks the SSH server for a listener on its own loopback (port `0`), so the server assigns a free port and nothing is bound to `0.0.0.0` on either side. The resulting `http://127.0.0.1:<assigned port>` is the proxy URL that remote programs dial.

Fields:

- `Local Network Proxy`: enable the reverse channel. Remote profiles only; local profiles have no SSH transport to carry it.
- `Proxy Host`, `Proxy Port`: the proxy on this computer. Defaults: `127.0.0.1` and `7890`.
- `No Proxy`: value of `no_proxy` and `NO_PROXY`. Default: `localhost,127.0.0.1,::1`. Clear it to stop emitting both variables.

While the channel is up, these variables point at the reverse listener: `http_proxy`, `https_proxy`, `all_proxy`, `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`, and `no_proxy` / `NO_PROXY`. An existing variable of the same name on the remote side is replaced, not appended.

Who inherits them:

- The ChemSSH port check and the start command: the `export` lines are written right after `set -e` and before your pre-start commands, so `cd`, `source`, `conda activate`, and `module load` keep them.
- The launcher's SSH debug terminal: every shell gets its own reverse listener. For `bash` the exports are appended to the `--rcfile` after `/etc/profile` and `~/.bash_profile` have run, so they win over a login profile. A non-bash login shell receives the variables through `env` before it starts, so a login profile can still override them there.
- ChemSSH's terminal tabs — including tabs opened later in a reused ChemSSH instance — through the bridge file below.

### ChemSSH terminal bridge

For each session, the launcher writes one file over SFTP:

```text
$HOME/.chemssh-launcher/terminal-proxy-<chemssh-port>.json
```

with mode `0600` and this v1 payload:

```json
{
  "version": 1,
  "proxy_url": "http://127.0.0.1:<assigned port>",
  "no_proxy": "localhost,127.0.0.1,::1",
  "env": {
    "http_proxy": "http://127.0.0.1:<assigned port>",
    "https_proxy": "http://127.0.0.1:<assigned port>",
    "all_proxy": "http://127.0.0.1:<assigned port>",
    "HTTP_PROXY": "http://127.0.0.1:<assigned port>",
    "HTTPS_PROXY": "http://127.0.0.1:<assigned port>",
    "ALL_PROXY": "http://127.0.0.1:<assigned port>",
    "no_proxy": "localhost,127.0.0.1,::1",
    "NO_PROXY": "localhost,127.0.0.1,::1"
  }
}
```

The file is named after the ChemSSH port that is actually listening, which can come from `--port` instead of the config file. ChemSSH reads it whenever it creates a terminal session and validates it: version `1`, an `http` URL with a loopback host and a port in `1-65535`, a reachable endpoint, env keys limited to the variables above, and values without NUL bytes. Anything else is treated as "no proxy" — the terminal still starts, no error is raised.

Lifecycle:

- Pausing forwarding keeps the reverse listener and the bridge file while the remote ChemSSH keeps running.
- Stopping the session, stopping launcher resources, or the remote command exiting with an error closes the listener, removes the bridge file (the `.chemssh-launcher` directory itself stays), and closes the SSH connection.
- If the bridge file cannot be written, the launcher logs a warning and the session still becomes ready: the remote command and launcher terminals get the proxy, but ChemSSH's own tabs are not synchronized.
- If the reverse channel cannot be created at all, the session starts anyway without it. The warning shows up in the profile log, on the session card, and on the terminal page.

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

The `/shell` page is a tabbed workspace, not another application page: its pinned first tab loads the launcher at `/`, and `/shell` itself is never opened inside a tab. Use `+` to open a blank tab with shortcuts to Launcher, Terminal, SFTP, and Backend, or type an app path or external address in the address bar. Interface language is configured under Backend → Interface.

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

The config directory also holds `client_identity.json`: the launcher's stable client id, sent as the `X-ChemSSH-Client-Id` header when proxying ChemSSH (the terminal WebSocket uses the `client_id` query parameter) so ChemSSH can isolate per-client session caches. `settings.json` stores UI preferences such as the interface language (`system`, `zh`, or `en`).

WebView2 data and temporary SFTP-open cache paths are shown in the Backend page. On Windows, cache cleanup can be delayed by file locks; the launcher records a pending cleanup marker and retries on the next start.

## Security Notes

ChemSSH Launcher relies on SSH for authentication, encryption, host-key verification, and local port forwarding. The launcher manages profile storage, secret lookup, remote/local process startup, SFTP sessions, and local HTTP proxying.

On the first connection to a server, the GUI/CLI shows the host key type and SHA256 fingerprint. Trust it only after comparing it with a reliable source, such as a server administrator or an existing trusted SSH client. If a saved host key changes later, the GUI blocks the connection and shows both the saved and the new fingerprint; the old key is replaced in `known_hosts` only after you verify the new fingerprint and explicitly confirm. The CLI continues to reject changed keys.

Binding `Local Host` to anything other than loopback, such as `0.0.0.0`, can expose the tunnel or local ChemSSH target to other machines. The launcher warns about it in the tunnel form, writes a warning to the profile log when the session starts, and prints one from the CLI when the profile is saved; the setting is still your responsibility.

The reverse proxy channel is independent of `Local Host`: its listener is created on the remote loopback with a server-assigned port, so it is never exposed to other machines and never binds `0.0.0.0`. The injected environment carries only a plain `http://127.0.0.1:<port>` URL with no credentials, and the launcher stores no proxy password. Keep `No Proxy` populated so loopback and internal addresses bypass the proxy.

The launcher's local GUI API additionally protects write operations (non-GET requests): every launch generates a random token, stores it in the `chemssh_launcher_token` cookie, and requires write requests to carry a matching `X-ChemSSH-Launcher-Token` header plus a valid Origin/Sec-Fetch-Site. The launcher's own frontend sends that header itself; for proxied ChemSSH pages the launcher injects a small script that reads the cookie and fills the header in, so bridge write endpoints (such as opening a file locally) need no extra configuration.

---

<details>
<summary>Architecture (Developers)</summary>

The project combines a Go backend, embedded Vue frontend, SSH/SFTP clients, and optional WebView2 host:

```text
chemssh-launcher executable
  Go GUI server on 127.0.0.1:<random>
    Vue app: /, /shell, /terminal, /sftp, /launcher-logs
    Profile/session APIs: /api/profiles, /api/profiles/reorder, /api/profiles/duplicate, /api/session/*, /api/session/shell/*
    UI settings API: /api/ui-settings
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

<a id="development-guide"></a>

## Development Guide (Developers)

### Prerequisites

- Install Go 1.23 or newer, plus Node.js (22 or newer recommended) and its bundled npm. Add them to `PATH` and reopen your terminal after installation.
- Obtain the complete repository and run the commands below from the repository root, which contains `go.mod`, `frontend/`, and `tools/`.
- Initial dependency installation requires access to a Go module proxy and an npm registry. Node.js/npm are build-time dependencies; the resulting executable does not need them at runtime.
- WebView2 is a Windows-only variant and requires the Microsoft Edge WebView2 Runtime on the machine running it. Browser mode does not need that runtime.

### Windows / PowerShell: first complete build

Run all commands below from the **repository root**. Using `npm.cmd` avoids PowerShell execution-policy errors involving `npm.ps1`.

```powershell
# 1. Check the tools
go version
node --version
npm.cmd --version

# 2. Install locked frontend dependencies, including development dependencies
npm.cmd --prefix frontend ci --include=dev

# 3. Download Go dependencies
go mod download

# 4. Optional: preinstall the Windows icon/version resource generator
go install github.com/tc-hib/go-winres@v0.3.3

# 5. Build WebView2 mode without a console
go run ./tools/build --webview2 --windowsgui

# 6. Start the generated executable
.\chemssh-launcher-webview2.exe
```

Continue only after each step succeeds, especially dependency installation. `npm ci` installs the versions in `frontend/package-lock.json` and recreates `frontend/node_modules/`. Run it after obtaining the source, when the lockfile changes, or when dependencies are missing. `vue-tsc`, TypeScript, and Vite are development dependencies required for the build: do not use `--omit=dev`. `--include=dev` also prevents settings such as `NODE_ENV=production` from excluding them. No global installation of `vue-tsc` is needed.

The build tool **does not install frontend dependencies automatically**. It reads `internal/version/VERSION`, syncs `winres/winres.json`, runs frontend type checking and compilation, generates Windows resources when needed on Windows, and then compiles the Go application. Frontend assets are generated in `internal/gui/static/vue/` and embedded in the executable.

Preinstalling `go-winres` is optional and does not replace npm dependency installation. If it is not on `PATH`, the build tool falls back to `go run github.com/tc-hib/go-winres@v0.3.3`, downloading it when necessary. To prepare the resource generator in advance, check `go env GOBIN`; if unset, check the `bin` directory under `go env GOPATH`, and add the actual installation directory to `PATH`. This does not make a first build fully offline: the other Go/npm dependencies must also be available beforehand.

### Build variants and output paths

After installing dependencies, choose one command from the repository root:

```powershell
# Browser mode with a console for debugging
go run ./tools/build

# Browser mode without a console
go run ./tools/build --windowsgui

# WebView2 with a console for debugging startup issues
go run ./tools/build --webview2

# WebView2 without a console, recommended for daily use
go run ./tools/build --webview2 --windowsgui
```

On Windows, output defaults to the repository root: `chemssh-launcher.exe` for browser mode and `chemssh-launcher-webview2.exe` for WebView2. Building does not launch the executable. Use `-o` to choose another output path:

```powershell
go run ./tools/build --webview2 --windowsgui -o chemssh-launcher-custom.exe
```

The build tool enables optimizations by default (`-trimpath -ldflags="-s -w"`). For debugging, add `--no-optimize` and omit `--windowsgui` to retain console logs. After frontend source edits, rerun the build command; reinstall dependencies only when they change or are missing.

### macOS / Linux: browser mode

Run from the repository root; omit the Windows-only `--webview2` and `--windowsgui` flags:

```bash
npm --prefix frontend ci --include=dev
go mod download
go run ./tools/build
./chemssh-launcher
```

### Troubleshooting

- **`'vue-tsc' is not recognized as an internal or external command` / `vue-tsc: not found`**: frontend dependencies are usually missing, or development dependencies were omitted. `go-winres` only handles Windows resources and cannot fix this error. From the repository root, run the following commands, continuing to the build only after installation succeeds:

  ```powershell
  npm.cmd --prefix frontend ci --include=dev
  go run ./tools/build --webview2 --windowsgui
  ```

- **`go`, `node`, or `npm.cmd` not found**: check installation and `PATH`, reopen the terminal, and rerun the version checks. If PowerShell blocks `npm.ps1`, use `npm.cmd` as shown above; changing the system execution policy is unnecessary.
- **`npm ci` reports that the lockfile does not match `package.json`**: first ensure both files come from the same source checkout. If you deliberately changed dependencies, run `npm.cmd --prefix frontend install --include=dev` and commit the updated lockfile with the manifest. Normal source builds should retain the lockfile and use `npm ci`.
- **Dependency downloads time out or fail**: fix access to the Go module proxy, npm registry, or local network, then complete installation before building. Do not delete the lockfile or globally install `vue-tsc` to bypass a failed installation.

### Development and checks

Run these commands from the repository root as well:

```powershell
# Frontend type check and build only (use npm on macOS/Linux)
npm.cmd --prefix frontend run build

# Go tests
go test ./...

# Windows WebView2-specific tests
go test -tags webview2 ./internal/webview

# Start the Vite development server
npm.cmd --prefix frontend run dev
```

The Vite development server does not update assets embedded in the Go executable. Before testing the packaged application, rerun `go run ./tools/build` or the corresponding WebView2 build command.

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

`go run ./tools/build` syncs that version into Windows resource metadata. The version is shown in the Backend page and through:

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
