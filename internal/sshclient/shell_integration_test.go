package sshclient

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestShellIntegrationSetupInstallsVimShims(t *testing.T) {
	script := buildShellIntegrationSetupScript()
	for _, command := range []string{"sz", "rz", "vi", "vim"} {
		if !strings.Contains(script, `"$dir/bin/`+command+`"`) {
			t.Errorf("setup script does not install %s", command)
		}
	}
	for _, command := range []string{"vi", "vim"} {
		encoded := base64.StdEncoding.EncodeToString([]byte(shellVimShim(command)))
		if !strings.Contains(script, encoded) {
			t.Errorf("setup script does not embed the %s shim", command)
		}
	}
	if strings.Contains(script, "{{") {
		t.Error("setup script still contains an unresolved placeholder")
	}
}

func TestShellVimShimDisablesTerminalQueries(t *testing.T) {
	for _, command := range []string{"vi", "vim"} {
		shim := shellVimShim(command)
		for _, want := range []string{
			`real_command=$(command -v ` + command + ` 2>/dev/null)`,
			`grep -qi 'vim'`,
			`exec "$real_command" --cmd 'set t_RV= t_u7= t_RF= t_RB= t_RK=' "$@"`,
			`exec "$real_command" "$@"`,
			`[ "$item" = "$shim_dir" ] && continue`,
		} {
			if !strings.Contains(shim, want) {
				t.Errorf("%s shim is missing %q", command, want)
			}
		}
	}
}

func TestShellVimShimRunsExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh wrapper is exercised on Unix hosts")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	shimDir := filepath.Join(dir, "shim")
	realDir := filepath.Join(dir, "real")
	if err := os.MkdirAll(shimDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	shimPath := filepath.Join(shimDir, "vi")
	if err := os.WriteFile(shimPath, []byte(shellVimShim("vi")), 0o700); err != nil {
		t.Fatal(err)
	}
	fakeVi := `#!/bin/sh
if [ "$1" = "--version" ]; then
    echo "$FAKE_VI_VERSION"
    exit 0
fi
for arg in "$@"; do
    if [ "$arg" = "--show-path" ]; then
        echo "$PATH"
        exit 0
    fi
done
for arg in "$@"; do
    printf '%s\n' "$arg"
done
`
	if err := os.WriteFile(filepath.Join(realDir, "vi"), []byte(fakeVi), 0o700); err != nil {
		t.Fatal(err)
	}
	pathValue := shimDir + string(os.PathListSeparator) + realDir + string(os.PathListSeparator) + os.Getenv("PATH")
	run := func(t *testing.T, version string, args ...string) []string {
		t.Helper()
		cmd := exec.Command(shimPath, args...)
		for _, entry := range os.Environ() {
			if strings.HasPrefix(entry, "PATH=") {
				continue
			}
			cmd.Env = append(cmd.Env, entry)
		}
		cmd.Env = append(cmd.Env, "PATH="+pathValue, "FAKE_VI_VERSION="+version)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("run shim: %v", err)
		}
		return strings.Split(strings.TrimSpace(string(out)), "\n")
	}

	t.Run("vim gets terminal query overrides", func(t *testing.T) {
		got := run(t, "VIM - Vi IMproved 9.1", "notes.txt")
		want := []string{"--cmd", "set t_RV= t_u7= t_RF= t_RB= t_RK=", "notes.txt"}
		if !slices.Equal(got, want) {
			t.Fatalf("arguments = %q, want %q", got, want)
		}
	})

	t.Run("vim resolves outside the shim directory", func(t *testing.T) {
		got := run(t, "VIM - Vi IMproved 9.1", "--show-path")
		if len(got) != 1 || strings.Contains(got[0], shimDir) {
			t.Fatalf("PATH = %q, want no %q", got, shimDir)
		}
		if !strings.Contains(got[0], realDir) {
			t.Fatalf("PATH = %q, want %q", got[0], realDir)
		}
	})

	t.Run("non-vim keeps original arguments", func(t *testing.T) {
		got := run(t, "BusyBox v1.36.1", "notes.txt")
		want := []string{"notes.txt"}
		if !slices.Equal(got, want) {
			t.Fatalf("arguments = %q, want %q", got, want)
		}
	})
}
