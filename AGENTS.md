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
}
```

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

Default release builds (two versions):

```bash
go run ./tools/build                              # Standard CLI/console executable
go run ./tools/build --webview2 --windowsgui      # WebView2 GUI executable (no console window)
```

Debug builds:

```bash
go run ./tools/build --webview2                   # WebView2 executable (with console window, for debugging only)
```

The standard release consists of two executables: the CLI version and the WebView2 GUI version (no console).

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

## Testing Expectations

Add focused tests for:

- Profile JSON round trip.
- Secret store interface with fake implementation.
- Password edit behavior: saved secrets are not returned to UI for display.
- Local port availability detection.
- Suggested alternate ports.
- Health check success/failure behavior.
- Command assembly preserves multi-line pre-start commands.

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

