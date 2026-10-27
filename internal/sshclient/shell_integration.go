package sshclient

import (
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"chemssh-launcher/internal/config"

	"golang.org/x/crypto/ssh"
)

const shellIntegrationSetupTimeout = 5 * time.Second

type shellIntegration struct {
	shimDir   string
	shellPath string
	bash      bool
}

func (s *shellIntegration) binDir() string {
	return s.shimDir + "/bin"
}

func (s *shellIntegration) initPath() string {
	return s.shimDir + "/init.sh"
}

func interactiveShellCommand(integration *shellIntegration, proxyEnv []config.EnvPair) string {
	if integration == nil {
		return ""
	}
	if integration.bash {
		prefix := ""
		if len(proxyEnv) > 0 {
			prefix = "env " + envAssignments(proxyEnv) + " "
		}
		return fmt.Sprintf("exec %s%s --rcfile %s -i", prefix, shellQuote(integration.shellPath), shellQuote(integration.initPath()))
	}
	// `exec` treats a leading assignment as the command name, so the login-shell
	// path always has to go through `env`; only the proxy assignments are optional.
	assignments := envAssignments(proxyEnv)
	if assignments != "" {
		assignments += " "
	}
	return fmt.Sprintf("exec env %sPATH=%s:\"$PATH\" %s -l", assignments, shellQuote(integration.binDir()), shellQuote(integration.shellPath))
}

func proxyLoginShellCommand(proxyEnv []config.EnvPair) string {
	return fmt.Sprintf("exec env %s \"$SHELL\" -l", envAssignments(proxyEnv))
}

func envAssignments(env []config.EnvPair) string {
	parts := make([]string, 0, len(env))
	for _, pair := range env {
		parts = append(parts, pair.Key+"="+shellQuote(pair.Value))
	}
	return strings.Join(parts, " ")
}

func prepareShellIntegration(client *ssh.Client, proxyEnv []config.EnvPair) (*shellIntegration, error) {
	if client == nil {
		return nil, errors.New("ssh client is not available")
	}
	session, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer session.Close()

	command := "echo '" + base64.StdEncoding.EncodeToString([]byte(buildShellIntegrationSetupScript(proxyEnv))) + "' | base64 -d | /bin/sh"
	type result struct {
		output []byte
		err    error
	}
	done := make(chan result, 1)
	go func() {
		output, runErr := session.Output(command)
		done <- result{output: output, err: runErr}
	}()

	var out []byte
	select {
	case res := <-done:
		if res.err != nil {
			return nil, res.err
		}
		out = res.output
	case <-time.After(shellIntegrationSetupTimeout):
		_ = session.Close()
		return nil, errors.New("shell integration setup timed out")
	}

	integration := &shellIntegration{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "CHEMSSH_LAUNCHER_SHELL="):
			integration.shellPath = strings.TrimPrefix(line, "CHEMSSH_LAUNCHER_SHELL=")
		case strings.HasPrefix(line, "CHEMSSH_LAUNCHER_SHIM="):
			integration.shimDir = strings.TrimPrefix(line, "CHEMSSH_LAUNCHER_SHIM=")
		}
	}
	if !validRemoteAbsolutePath(integration.shellPath) {
		return nil, fmt.Errorf("unexpected remote shell path %q", integration.shellPath)
	}
	if !validRemoteAbsolutePath(integration.shimDir) {
		return nil, fmt.Errorf("unexpected remote shim directory %q", integration.shimDir)
	}
	integration.bash = strings.Contains(path.Base(integration.shellPath), "bash")
	return integration, nil
}

func validRemoteAbsolutePath(value string) bool {
	if value == "" || !strings.HasPrefix(value, "/") {
		return false
	}
	return !strings.ContainsAny(value, "\r\n\x00")
}

func cleanupShellIntegration(client *ssh.Client, integration *shellIntegration) {
	if client == nil || integration == nil || integration.shimDir == "" {
		return
	}
	session, err := client.NewSession()
	if err != nil {
		return
	}
	defer session.Close()
	done := make(chan struct{})
	go func() {
		_ = session.Run("rm -rf -- " + shellQuote(integration.shimDir))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = session.Close()
	}
}

func buildShellIntegrationSetupScript(proxyEnv []config.EnvPair) string {
	setup := `set -e
shell=${SHELL:-}
if [ -z "$shell" ]; then
    shell=$(getent passwd "$(id -u)" 2>/dev/null | cut -d: -f7)
fi
[ -n "$shell" ] || shell=/bin/bash
case "$shell" in
    /*) ;;
    *) shell=/bin/bash ;;
esac
[ -x "$shell" ] || shell=/bin/sh
dir=$(mktemp -d "${TMPDIR:-/tmp}/chemssh-launcher.XXXXXX" 2>/dev/null) || dir=$(mktemp -d 2>/dev/null) || exit 1
mkdir -p "$dir/bin" || exit 1
printf '%s' '{{SZ_SHIM}}' | base64 -d > "$dir/bin/sz" || exit 1
printf '%s' '{{RZ_SHIM}}' | base64 -d > "$dir/bin/rz" || exit 1
printf '%s' '{{VI_SHIM}}' | base64 -d > "$dir/bin/vi" || exit 1
printf '%s' '{{VIM_SHIM}}' | base64 -d > "$dir/bin/vim" || exit 1
printf '%s' '{{INIT_SCRIPT}}' | base64 -d > "$dir/init.sh" || exit 1
chmod 700 "$dir" "$dir/bin" "$dir/bin/sz" "$dir/bin/rz" "$dir/bin/vi" "$dir/bin/vim" "$dir/init.sh" || exit 1
if ! "$dir/bin/sz" >/dev/null 2>&1; then
    rm -rf "$dir"
    exit 1
fi
printf 'CHEMSSH_LAUNCHER_SHELL=%s\nCHEMSSH_LAUNCHER_SHIM=%s\n' "$shell" "$dir"
`
	setup = strings.ReplaceAll(setup, "{{SZ_SHIM}}", base64.StdEncoding.EncodeToString([]byte(shellTransferShim("download"))))
	setup = strings.ReplaceAll(setup, "{{RZ_SHIM}}", base64.StdEncoding.EncodeToString([]byte(shellTransferShim("upload"))))
	setup = strings.ReplaceAll(setup, "{{VI_SHIM}}", base64.StdEncoding.EncodeToString([]byte(shellVimShim("vi"))))
	setup = strings.ReplaceAll(setup, "{{VIM_SHIM}}", base64.StdEncoding.EncodeToString([]byte(shellVimShim("vim"))))
	initScript := shellIntegrationInitScript
	if len(proxyEnv) > 0 {
		var extra strings.Builder
		extra.WriteString("\n# Launcher proxy environment, applied after login profiles so it wins.\n")
		for _, pair := range proxyEnv {
			extra.WriteString("export ")
			extra.WriteString(pair.Key)
			extra.WriteByte('=')
			extra.WriteString(shellQuote(pair.Value))
			extra.WriteByte('\n')
		}
		initScript += extra.String()
	}
	setup = strings.ReplaceAll(setup, "{{INIT_SCRIPT}}", base64.StdEncoding.EncodeToString([]byte(initScript)))
	return setup
}

// shellVimShim disables Vim's terminal capability queries, which the browser
// terminal never answers; without this, vi can block at startup on some hosts.
func shellVimShim(command string) string {
	return fmt.Sprintf(`#!/bin/sh
shim_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
clean_path=
old_ifs=$IFS
IFS=:
for item in $PATH; do
    [ "$item" = "$shim_dir" ] && continue
    if [ -z "$clean_path" ]; then
        clean_path=$item
    else
        clean_path=$clean_path:$item
    fi
done
IFS=$old_ifs
PATH=$clean_path
export PATH

real_command=$(command -v %s 2>/dev/null)
if [ -z "$real_command" ]; then
    echo "%s: command not found" >&2
    exit 127
fi

if "$real_command" --version 2>/dev/null | sed -n '1p' | grep -qi 'vim'; then
    exec "$real_command" --cmd 'set t_RV= t_u7= t_RF= t_RB= t_RK=' "$@"
fi

exec "$real_command" "$@"
`, command, command)
}

func shellTransferShim(direction string) string {
	return fmt.Sprintf(`#!/bin/sh
if ! command -v base64 >/dev/null 2>&1; then
    echo "sz: base64 is required for SSH terminal transfers" >&2
    exit 127
fi
encode() {
    printf '%%s' "$1" | base64 | tr -d '\n'
}
cwd=${PWD:-}
[ -n "$cwd" ] || cwd=$(pwd)
payload="v1|%s|$(encode "$cwd")"
for arg in "$@"; do
    payload="$payload|$(encode "$arg")"
done
printf '\033]777;chemssh-transfer;%%s\007' "$payload"
exit 0
`, direction)
}

const shellIntegrationInitScript = `# Sourced by bash --rcfile when the SSH terminal starts.
if [[ -z ${CHEMSSH_LAUNCHER_INIT_DONE:-} ]]; then
    CHEMSSH_LAUNCHER_INIT_DONE=1
    if [[ -r /etc/profile ]]; then
        source /etc/profile
    fi
    if [[ -n ${HOME:-} ]]; then
        for _chemssh_launcher_profile in "${HOME}/.bash_profile" "${HOME}/.bash_login" "${HOME}/.profile"; do
            if [[ -r "${_chemssh_launcher_profile}" ]]; then
                source "${_chemssh_launcher_profile}"
                break
            fi
        done
        unset _chemssh_launcher_profile
    fi
fi
_chemssh_launcher_dir=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
export PATH="$_chemssh_launcher_dir/bin:$PATH"
_chemssh_launcher_marker='printf "\033]633;P;Cwd=%s\007" "$PWD"'
case "${PROMPT_COMMAND:-}" in
    *"]633;P;Cwd="*)
        ;;
    *)
        if [[ -n "${PROMPT_COMMAND:-}" ]]; then
            PROMPT_COMMAND="${_chemssh_launcher_marker}; ${PROMPT_COMMAND}"
        else
            PROMPT_COMMAND="${_chemssh_launcher_marker}"
        fi
        ;;
esac
unset _chemssh_launcher_marker _chemssh_launcher_dir
`
