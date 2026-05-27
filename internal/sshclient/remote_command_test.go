package sshclient

import (
	"strings"
	"testing"

	"chemweb-launcher/internal/config"
)

func TestAssembleRemoteCommandPreservesMultilinePreStart(t *testing.T) {
	profile := config.NewProfileDefaults()
	profile.PreStartCommands = "cd /home/user/chemweb\nsource .venv/bin/activate"
	profile.StartCommand = "chemweb --config config.yaml"

	got := AssembleRemoteCommand(profile)
	mustContain := []string{
		"set -e\ncd /home/user/chemweb\nsource .venv/bin/activate\n",
		"trap '__chemweb_launcher_cleanup; exit 143' INT TERM HUP",
		"chemweb --config config.yaml --host 127.0.0.1 --port 8888 &",
		"__chemweb_launcher_pid=$!",
		"printf '%s\\n' \"$__chemweb_launcher_pid\" > \"$__chemweb_launcher_pidfile\"",
		"kill -TERM \"$__chemweb_launcher_pid\"",
	}
	for _, want := range mustContain {
		if !strings.Contains(got, want) {
			t.Fatalf("assembled command missing %q\n%s", want, got)
		}
	}
}

func TestAssembleStopCommandUsesPIDFile(t *testing.T) {
	profile := config.NewProfileDefaults()
	profile.ID = "profile-1"
	got := AssembleStopCommand(profile)
	for _, want := range []string{
		"__chemweb_launcher_pidfile=/tmp/chemweb-launcher-profile-1.pid",
		"kill -TERM \"$__chemweb_launcher_pid\"",
		"kill -KILL \"$__chemweb_launcher_pid\"",
		"rm -f \"$__chemweb_launcher_pidfile\"",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stop command missing %q\n%s", want, got)
		}
	}
}

func TestAssembleCheckPortCommandUsesChemwebCheckPort(t *testing.T) {
	profile := config.NewProfileDefaults()
	profile.PreStartCommands = "cd /home/user/chemweb\nsource .venv/bin/activate"
	profile.StartCommand = "chemweb --config config.yaml"

	got := AssembleCheckPortCommand(profile)
	for _, want := range []string{
		"set -e\ncd /home/user/chemweb\nsource .venv/bin/activate\n",
		"chemweb --config config.yaml --host 127.0.0.1 --port 8888 --check-port",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("check-port command missing %q\n%s", want, got)
		}
	}
}

func TestBackgroundLastCommand(t *testing.T) {
	got := backgroundLastCommand("export FOO=1\nchemweb --config config.yaml")
	want := "export FOO=1\nchemweb --config config.yaml &\n"
	if got != want {
		t.Fatalf("unexpected background command\nwant: %q\n got: %q", want, got)
	}
}

func TestPortWarning(t *testing.T) {
	profile := config.NewProfileDefaults()
	profile.RemotePort = 8888
	profile.StartCommand = "chemweb --port 8899"

	warning := PortWarning(profile)
	if !strings.Contains(warning, "8899") || !strings.Contains(warning, "8888") {
		t.Fatalf("unexpected warning: %q", warning)
	}
}

func TestEffectiveStartCommandDoesNotDuplicateExplicitFlags(t *testing.T) {
	profile := config.NewProfileDefaults()
	profile.RemoteHost = "127.0.0.1"
	profile.RemotePort = 8888
	profile.StartCommand = "chemweb --config config.yaml --host 127.0.0.1 --port 8888"

	got := EffectiveStartCommand(profile)
	if strings.Count(got, "--host") != 1 || strings.Count(got, "--port") != 1 {
		t.Fatalf("duplicated flags: %q", got)
	}
}

func TestEffectiveDefaultStartCommandAppendsFlagsToChemwebLine(t *testing.T) {
	profile := config.NewProfileDefaults()
	got := EffectiveStartCommand(profile)
	if !strings.Contains(got, "source .venv/bin/activate") {
		t.Fatalf("default command missing venv activation\n%s", got)
	}
	if !strings.Contains(got, "chemweb --config config.yaml --host 127.0.0.1 --port 8888") {
		t.Fatalf("default command did not append host/port to chemweb line\n%s", got)
	}
}
