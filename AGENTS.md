# AGENTS.md

## Project Purpose

This project is a standalone ChemSSH launcher written in Go. It should produce a small single-file executable that can connect to a remote server over SSH, run ChemSSH there, create a local SSH tunnel, check the local URL, and open the browser.

The launcher exists because ChemSSH is usually run on a remote Linux server or HPC login node while the browser runs locally. Users should not need to manually open two SSH sessions.

## Core Direction

- Language: Go.
- SSH implementation: `golang.org/x/crypto/ssh`.
- Runtime dependencies: no Python, no Node, no system `ssh` requirement.
- Output: one executable per platform via `go build`.
- First UI: CLI or simple terminal UI. Keep code structured so GUI can be added later.


## Encoding

- Decode and write repository text files as UTF-8.
- Documentation files, especially README.md, README.zh-CN.md, and AGENTS.md, must remain valid UTF-8.

## Privacy and Placeholder Conventions

- Never put a real username, hostname, server address, internal path, or account name into documentation, examples, tests, commit messages, or issue text.
- Use placeholders instead: `user`, `example.com`, `example-server`, `/home/user/project`, `C:\Users\user\Desktop`.
- When documenting a path that comes from a real machine, rewrite the user-specific segment as `user` before writing it. For example, a Windows desktop path becomes `C:\Users\user\Desktop`, and a remote home path becomes `/home/user/project`.
- Never quote a real value as an example, even when illustrating this rule.
- Generic credentials and tokens in examples must be obvious dummies such as `password`, `token`, `REPLACE_ME`, or `***`.
- Screenshots and logs pasted into documentation follow the same rule: crop, mask, or replace real identifiers first.

## ChemSSH Context

ChemSSH normally starts like this on the remote machine:

```bash
chemssh --config config.yaml --host 127.0.0.1 --port 8888
```

It is intentionally bound to `127.0.0.1` on the remote server. The launcher should forward a local port to that remote loopback address:

```text
127.0.0.1:8888 -> 127.0.0.1:8888
```

If `127.0.0.1:8888` is occupied locally, ask the user to change the local port. Prefer changing the port over changing `127.0.0.1` to another loopback address.

Do not default to `0.0.0.0`.

Because ChemSSH only listens on the remote loopback, a proxy that exists on the user's own machine is unreachable from the server. The launcher therefore offers an optional reverse channel that publishes the local proxy on a server-assigned loopback port; see [Local Network Proxy](#local-network-proxy).

## Required Features

Implement these before polishing:

1. Multiple server profiles.
2. Encrypted credential storage.
3. Password login.
4. Private key login.
5. Multi-line pre-start commands.
6. Multi-line ChemSSH start command.
7. Local port conflict detection.
8. SSH local port forwarding.
9. Health check for local URL.
10. Optional browser auto-open.
11. Stop session and close tunnel.
12. Optional reverse channel that exposes a local network proxy to the remote loopback.

## Profile Model

Profiles should have stable IDs. Names are user-facing and can change.

Suggested fields:

```go
type Profile struct {
    ID                       string `json:"id"`
    Name                     string `json:"name"`
    SSHHost                  string `json:"ssh_host"`
    SSHPort                  int    `json:"ssh_port"`
    SSHUser                  string `json:"ssh_user"`
    AuthMethod               string `json:"auth_method"`
    HasPassword              bool   `json:"has_password"`
    PrivateKeyPath           string `json:"private_key_path"`
    HasPrivateKeyPassphrase  bool   `json:"has_private_key_passphrase"`
    RemoteHost               string `json:"remote_host"`
    RemotePort               int    `json:"remote_port"`
    LocalHost                string `json:"local_host"`
    LocalPort                int    `json:"local_port"`
    LocalURLPath             string `json:"local_url_path"`
    PreStartCommands         string `json:"pre_start_commands"`
    StartCommand             string `json:"start_command"`
    HealthCheckURL           string `json:"health_check_url"`
    OpenBrowser              bool   `json:"open_browser"`
    HasSecurityToken         bool   `json:"has_security_token"`
    LocalProxy               LocalProxy `json:"local_proxy"`
}
```

The `LocalProxy` value is configuration, not a secret: `Enabled` (`bool`), `Host` (`string`), `Port` (`int`), `NoProxy` (`string`), defaults `127.0.0.1`, `7890`, `localhost,127.0.0.1,::1`. Fill missing fields through a `Normalized()` method when loading old files, and only allow the feature on remote profiles.

Do not store raw passwords or passphrases in this struct.

## Secret Storage Rules

Secrets must be encrypted at rest.

Preferred implementation:

- Use an OS credential store library such as `github.com/zalando/go-keyring`.
- Store profile JSON separately from secrets.
- Use keys like:
  - service: `chemssh-launcher`
  - username: `<profile-id>:password`
  - username: `<profile-id>:key-passphrase`
  - username: `<profile-id>:security-token`

Fallback if keyring is unavailable:

- Implement an encrypted vault file.
- Use Argon2id for key derivation.
- Use AES-GCM or XChaCha20-Poly1305.
- Never store the vault password.

Editing rule:

- A stored password must never be shown again.
- Edit screens/prompts can show `saved` or `not set`.
- Provide actions to replace or clear the secret.

Logging rule:

- Never log decrypted secrets.
- Never include secrets in errors.
- Never pass passwords through process command-line arguments.

## Remote Command Semantics

Users can enter multi-line setup commands, including `cd`, `source`, `conda activate`, `module load`, or env exports.

Run pre-start commands and start command in the same remote shell so environment changes carry forward.

Conceptual command:

```bash
set -e
<pre_start_commands>
<start_command>
```

Keep stdout/stderr visible to the user. These logs are important when remote environment setup fails.

If the configured remote port is `8888` but the custom start command appears to use another port, warn instead of rewriting user commands.

## SSH Tunnel Behavior

Implement local forwarding with a local TCP listener:

1. Listen on `profile.LocalHost:profile.LocalPort`.
2. For each local connection, call `sshClient.Dial("tcp", remoteHostPort)`.
3. Copy bytes in both directions until closed.

Remote target:

```text
profile.RemoteHost:profile.RemotePort
```

Default:

```text
127.0.0.1:8888
```

Local bind default:

```text
127.0.0.1:8888
```

Warn if `LocalHost` is `0.0.0.0` or a non-loopback address.

## Local Network Proxy

A remote profile may optionally publish a proxy that runs on the launcher machine to the remote loopback, so remote programs and ChemSSH's own terminal sessions can reach it:

```text
remote 127.0.0.1:<assigned port> -> ssh -> <LocalProxy.Host>:<LocalProxy.Port>
```

Reverse listener rules:

- Create it with `ssh.Client.ListenTCP(&net.TCPAddr{IP: 127.0.0.1, Port: 0})` and let the SSH server assign the port.
- Only ever bind the server loopback. Never request `0.0.0.0` and never guess a port that is already in use.
- The advertised proxy URL is `http://` plus the address the server returned. Require loopback and a port in `1-65535` on the ChemSSH side too.

Environment contract:

- Emit `http_proxy`, `https_proxy`, `all_proxy`, `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY`, and `no_proxy` / `NO_PROXY` when `NoProxy` is not empty.
- The value is a plain URL with no credentials. Never add proxy authentication, and never store one.
- Existing variables on the remote side are replaced, not appended.
- Inject nothing when the profile does not enable the feature.

Injection points and ordering:

- Remote commands: write the `export` lines right after `set -e` and before the pre-start commands, so `cd`, `source`, `conda activate`, and `module load` keep them. Applies to both the port-check command and the start command.
- Launcher SSH terminal: each shell opens its own reverse listener and touches no bridge file. For bash, append the `export` lines to the end of the `--rcfile` so they run after `/etc/profile` and `~/.bash_profile`. For a non-bash login shell, hand the variables over through an `env` prefix; a login profile may still override them, which is acceptable.
- ChemSSH terminal tabs: through the bridge file below, because they are created in another process and may be created long after startup.

Bridge file contract with ChemSSH:

- Path: `$HOME/.chemssh-launcher/terminal-proxy-<chemssh-port>.json`, mode `0600`, written over SFTP.
- The `<chemssh-port>` segment is the **effective** ChemSSH listening port, which may come from `--port` instead of the config file. ChemSSH must persist the effective host/port into settings before serving so the lookup uses the same value.
- Payload is versioned JSON, currently version `1`: `{"version":1,"proxy_url":"...","no_proxy":"...","env":{"<VAR>":"<value>"}}`.
- The reading side validates version, a bare `http://<loopback>:<port>` URL (no credentials, no path — a trailing `/` fails too — no query, no fragment), a port in `1-65535`, a file size under 64 KB, a reachable TCP endpoint, env keys limited to the variables above, and values without NUL bytes. Any failure means "no proxy": log the reason (never the payload) and keep the terminal working, never raise.
- Take the remote home directory from the SFTP session (`Getwd()`), because the launcher cannot assume a fixed path under a shared home.

Lifecycle:

- Pausing forwarding (the remote service keeps running) keeps the reverse listener and the bridge file, so already-open and new ChemSSH terminals still work.
- Full stop, launcher-resource stop, and abnormal remote exit close the listener, remove the bridge file, and close the SSH connection. Removing a missing file is not an error.
- Resuming forwarding reuses an existing channel and rebuilds a lost one. A rebuild failure is cleaned up before returning.

Degradation is mandatory:

- A failed reverse channel must not fail the session. Start without it and warn in the profile log, the session status payload (`local_proxy.error`), and the terminal page.
- A failed bridge write must not fail the session either. Keep the channel, report the error as a warning, and treat the session as ready but out of sync with ChemSSH terminals.

## Port and Health Checks

Before SSH connection:

- Test whether the local bind address and port can be listened on.
- If not available, show the conflict and suggest nearby free ports.

After starting remote command and tunnel:

- Poll `http://<local_host>:<local_port><local_url_path>`, unless `HealthCheckURL` is set.
- Treat HTTP status 200 through 499 as service reachable.
- Retry until timeout.
- On success, optionally open browser.

## Suggested Packages

Keep dependencies modest.

Likely dependencies:

- `golang.org/x/crypto/ssh`
- `golang.org/x/term` for password input
- `github.com/zalando/go-keyring` for OS credential storage
- `github.com/pkg/sftp` for SFTP file operations and the ChemSSH proxy bridge file

Optional:

- `github.com/spf13/cobra` for CLI commands
- `github.com/google/uuid` for profile IDs

Avoid Electron-style stacks. This is intended to stay small.

## CLI Target

Initial commands:

```bash
chemssh-launcher profile list
chemssh-launcher profile add
chemssh-launcher profile edit <name-or-id>
chemssh-launcher profile delete <name-or-id>
chemssh-launcher profile test <name-or-id>
chemssh-launcher start <name-or-id>
```

Interactive profile creation is acceptable and preferred for v1.

Prompt defaults:

- SSH port: `22`
- Remote host: `127.0.0.1`
- Remote port: `8888`
- Local host: `127.0.0.1`
- Local port: `8888`
- Local URL path: `/`
- Open browser: `true`

## Build Configuration

Default release builds (three recommended variants):

```bash
# Browser mode (opens in system browser)
go run ./tools/build --windowsgui                 # Standard browser mode, no console

# WebView2 mode (embedded browser window)
go run ./tools/build --webview2 --windowsgui      # WebView2 mode, no console (best UX)
go run ./tools/build --webview2                   # WebView2 mode with console (for debugging)
```

The build tool enables size and startup speed optimizations by default. Use `--no-optimize` to disable for debugging.

**Version comparison**:
- **Browser mode**: Uses system browser, lighter weight, easier to debug network requests
- **WebView2 mode**: Embedded window, better integration, smoother user experience
- **`--windowsgui`**: Hides console window, cleaner for end users, harder to debug startup issues

## Code Organization

Start with this structure:

```text
cmd/chemssh-launcher/main.go
internal/app/
internal/config/
internal/secret/
internal/sshclient/
internal/netcheck/
internal/browser/
internal/ui/
```

Keep package responsibilities clear:

- `config`: profile structs, config paths, JSON load/save.
- `secret`: keyring and fallback vault.
- `sshclient`: auth, remote command, tunnel.
- `netcheck`: local port checks and URL health checks.
- `browser`: platform browser opening.
- `app`: orchestration and session lifecycle.
- `ui`: CLI prompts and command wiring.

Local proxy pieces live with their package: `config/local_proxy.go` (model, defaults, env pairs), `sshclient/proxy_channel.go` (reverse channel per session or per terminal), `sshclient/proxy_bridge.go` (bridge file write/remove over SFTP), and the lifecycle glue in `app/session.go`, `gui/gui.go`, and `gui/terminal.go`.

## Testing Expectations

Add focused tests for:

- Profile JSON round trip.
- Secret store interface with fake implementation.
- Password edit behavior: saved secrets are not returned to UI for display.
- Local port availability detection.
- Suggested alternate ports.
- Health check success/failure behavior.
- Command assembly preserves multi-line pre-start commands.
- Proxy defaults and normalization, plus the generated env pairs (both cases, no `no_proxy` when empty).
- Proxy exports appear after `set -e` and before pre-start commands.
- Reverse tunnel accepts and forwards with an in-memory fake SSH server (`tcpip-forward`, `cancel-tcpip-forward`, `forwarded-tcpip`).
- Lifecycle: pausing forwarding keeps the channel, full stop clears it, a degraded channel reports `local_proxy.error`, and launcher terminal channels write no bridge file.
- ChemSSH side of the bridge (sibling repo): valid file injects env after login profiles, and non-loopback, `socks5`, port `0`, stale endpoint, bad JSON, wrong version, and unknown env keys all degrade to no proxy without raising.

Tunnel integration tests can be added later with a local SSH test server or manual test script.

## Implementation Priorities

1. Create profile storage and CLI skeleton.
2. Add secret storage abstraction.
3. Add password and private key auth.
4. Add port conflict check.
5. Add SSH connection and remote command execution.
6. Add tunnel.
7. Add health check and browser open.
8. Improve lifecycle shutdown.
9. Add tests around each completed layer.

## Safety Constraints

- Do not store passwords in plain JSON.
- Do not print passwords.
- Do not show saved passwords while editing.
- Do not silently bind to `0.0.0.0`.
- Do not silently bind the reverse proxy listener to anything but the remote loopback, and do not hardcode its port instead of letting the server assign one.
- Do not inject proxy variables into a session whose profile does not enable them.
- Do not put proxy credentials into the injected environment, the bridge file, or logs.
- Do not fail a session because the reverse channel or the bridge file failed; warn and degrade.
- Do not rewrite custom user commands behind their back.
- Do not require ChemSSH to listen on a public interface.
- Do not assume the remote machine has npm, conda, module, or bash unless the user configured commands accordingly.

## Manual Smoke Test

Expected happy path:

1. Build the launcher.
2. Add a profile with password auth.
3. Use pre-start commands:

```bash
cd /home/user/chemssh
source .venv/bin/activate
```

4. Use start command:

```bash
chemssh --config config.yaml --host 127.0.0.1 --port 8888
```

5. Run:

```bash
chemssh-launcher start <profile>
```

6. Confirm local URL opens:

```text
http://127.0.0.1:8888/
```

7. Stop launcher and confirm the tunnel closes.

Optional check with `Local Network Proxy` enabled:

1. Run a proxy on this computer at `127.0.0.1:7890` and start the session.
2. On the remote machine, dial the assigned port from the profile log:

```bash
curl -x http://127.0.0.1:<assigned port> https://example.com
```

3. Run `env | grep -i proxy` in the launcher SSH terminal and in a newly opened ChemSSH terminal tab; both must show the reverse listener.
4. Stop the local proxy and open another ChemSSH terminal tab: the stale endpoint is ignored and the tab still opens, just without the proxy variables.
5. Break the channel itself (for example by blocking the SSH forward) and start again: the session still starts without it and warns.
6. Stop the session and confirm `$HOME/.chemssh-launcher/terminal-proxy-<chemssh-port>.json` is gone on the remote side.

