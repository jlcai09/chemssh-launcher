package sshclient

import (
	"strings"
	"testing"

	"chemssh-launcher/internal/config"
)

func TestAssembleRemoteCommandPreservesMultilinePreStart(t *testing.T) {
	profile := config.NewProfileDefaults()
	profile.PreStartCommands = "cd /home/user/chemssh\nsource .venv/bin/activate"
	profile.StartCommand = "chemssh --config config.yaml"

	got := AssembleRemoteCommand(profile)
	mustContain := []string{
		"set -e\ncd /home/user/chemssh\nsource .venv/bin/activate\n",
		"trap '__chemssh_launcher_cleanup; exit 143' INT TERM HUP",
		"chemssh --config config.yaml --host 127.0.0.1 --port 8888 &",
		"__chemssh_launcher_pid=$!",
		"kill -TERM \"$__chemssh_launcher_pid\"",
	}
	for _, want := range mustContain {
		if !strings.Contains(got, want) {
			t.Fatalf("assembled command missing %q\n%s", want, got)
		}
	}
	if strings.Contains(got, "pidfile") {
		t.Fatalf("assembled command should not write launcher pidfiles anymore\n%s", got)
	}
}

func TestAssembleKillPIDCommandUsesExplicitPID(t *testing.T) {
	got := AssembleKillPIDCommand(12345)
	for _, want := range []string{
		"__chemssh_launcher_pid=12345",
		"kill -TERM \"$__chemssh_launcher_pid\"",
		"kill -KILL \"$__chemssh_launcher_pid\"",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stop command missing %q\n%s", want, got)
		}
	}
	if strings.Contains(got, "pidfile") {
		t.Fatalf("kill command should not use launcher pidfiles\n%s", got)
	}
}

func TestAssembleCheckPortCommandUsesChemSSHCheckPort(t *testing.T) {
	profile := config.NewProfileDefaults()
	profile.PreStartCommands = "cd /home/user/chemssh\nsource .venv/bin/activate"
	profile.StartCommand = "chemssh --config config.yaml"

	got := AssembleCheckPortCommand(profile)
	for _, want := range []string{
		"set -e\ncd /home/user/chemssh\nsource .venv/bin/activate\n",
		"chemssh --config config.yaml --host 127.0.0.1 --port 8888 --check-port",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("check-port command missing %q\n%s", want, got)
		}
	}
}

func TestBackgroundLastCommand(t *testing.T) {
	got := backgroundLastCommand("export FOO=1\nchemssh --config config.yaml")
	want := "export FOO=1\nchemssh --config config.yaml &\n"
	if got != want {
		t.Fatalf("unexpected background command\nwant: %q\n got: %q", want, got)
	}
}

func TestPortWarning(t *testing.T) {
	profile := config.NewProfileDefaults()
	profile.RemotePort = 8888
	profile.StartCommand = "chemssh --port 8899"

	warning := PortWarning(profile)
	if !strings.Contains(warning, "8899") || !strings.Contains(warning, "8888") {
		t.Fatalf("unexpected warning: %q", warning)
	}
}

func TestEffectiveStartCommandDoesNotDuplicateExplicitFlags(t *testing.T) {
	profile := config.NewProfileDefaults()
	profile.RemoteHost = "127.0.0.1"
	profile.RemotePort = 8888
	profile.StartCommand = "chemssh --config config.yaml --host 127.0.0.1 --port 8888"

	got := EffectiveStartCommand(profile)
	if strings.Count(got, "--host") != 1 || strings.Count(got, "--port") != 1 {
		t.Fatalf("duplicated flags: %q", got)
	}
}

func TestEffectiveDefaultStartCommandAppendsFlagsToChemSSHLine(t *testing.T) {
	profile := config.NewProfileDefaults()
	got := EffectiveStartCommand(profile)
	if !strings.Contains(got, "source .venv/bin/activate") {
		t.Fatalf("default command missing venv activation\n%s", got)
	}
	if !strings.Contains(got, "chemssh --config config.yaml --host 127.0.0.1 --port 8888") {
		t.Fatalf("default command did not append host/port to chemssh line\n%s", got)
	}
}

func TestEffectiveLocalStartCommandAppendsLocalAddress(t *testing.T) {
	profile := config.NewLocalProfileDefaults()
	profile.LocalHost = "127.0.0.1"
	profile.LocalPort = 8890
	profile.RemoteHost = "10.0.0.9"
	profile.RemotePort = 9999
	profile.StartCommand = "chemssh --config local.yaml"

	got := EffectiveLocalStartCommand(profile)
	want := "chemssh --config local.yaml --host 127.0.0.1 --port 8890"
	if got != want {
		t.Fatalf("local command = %q, want %q", got, want)
	}
	if strings.Contains(got, "10.0.0.9") || strings.Contains(got, "9999") {
		t.Fatalf("local command used remote address: %q", got)
	}
}

func TestAssembleLocalCommandPreservesPreStartAndDoesNotDuplicateFlags(t *testing.T) {
	profile := config.NewLocalProfileDefaults()
	profile.LocalHost = "localhost"
	profile.LocalPort = 8890
	profile.PreStartCommands = "cd C:/chemssh\n$env:FOO='1'"
	profile.StartCommand = "chemssh --config local.yaml --host localhost --port 8890"

	got := AssembleLocalCommand(profile)
	if !strings.Contains(got, "cd C:/chemssh\n$env:FOO='1'\nchemssh --config local.yaml") {
		t.Fatalf("local command did not preserve pre-start commands\n%s", got)
	}
	if strings.Count(got, "--host") != 1 || strings.Count(got, "--port") != 1 {
		t.Fatalf("duplicated local flags: %q", got)
	}
}
