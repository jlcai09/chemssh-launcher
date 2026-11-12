//go:build windows

package webview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const testProfileDir = `C:\Users\user\AppData\Roaming\ChemSSHLauncher\webview2`

func TestCommandLineUsesDataDir(t *testing.T) {
	quoted := `"C:\Program Files (x86)\Microsoft\EdgeWebView\Application\1\msedgewebview2.exe" --user-data-dir="C:\Users\user\AppData\Roaming\ChemSSHLauncher\webview2\EBWebView" --noerrdialogs`
	cases := []struct {
		name     string
		command  string
		dataPath string
		want     bool
	}{
		{"quoted profile", quoted, testProfileDir, true},
		{"case insensitive", strings.ToLower(quoted), testProfileDir, true},
		{"forward slashes in config", quoted, `C:/Users/user/AppData/Roaming/ChemSSHLauncher/webview2`, true},
		{"trailing separator", quoted, testProfileDir + `\`, true},
		{"prefix only", quoted, `C:\Users\user\AppData\Roaming\ChemSSHLauncher\webview`, false},
		{"sibling directory", `--user-data-dir="C:\Users\user\AppData\Roaming\ChemSSHLauncher\webview2-bak\EBWebView"`, testProfileDir, false},
		{"other profile", `--user-data-dir="C:\Users\user\AppData\Roaming\OtherApp\webview2\EBWebView"`, testProfileDir, false},
		{"empty data path", quoted, "", false},
		{"empty command line", "", testProfileDir, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := commandLineUsesDataDir(tc.command, tc.dataPath); got != tc.want {
				t.Fatalf("commandLineUsesDataDir() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStaleWebViewProcesses(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	profileCommand := `"C:\Edge\msedgewebview2.exe" --user-data-dir="` + testProfileDir + `\EBWebView"`
	otherCommand := `"C:\Edge\msedgewebview2.exe" --user-data-dir="C:\Users\user\AppData\Roaming\OtherApp\webview2\EBWebView"`

	t.Run("orphaned tree is selected root first", func(t *testing.T) {
		records := map[uint32]processRecord{
			100: {pid: 100, parent: 50, exe: "msedgewebview2.exe", created: base},
			101: {pid: 101, parent: 100, exe: "msedgewebview2.exe", created: base.Add(time.Second)},
			102: {pid: 102, parent: 100, exe: "msedgewebview2.exe", created: base.Add(time.Second)},
		}
		commandLines := map[uint32]string{100: profileCommand, 101: profileCommand, 102: profileCommand}
		stale := staleWebViewProcesses(records, commandLines, testProfileDir)
		if len(stale) != 3 {
			t.Fatalf("stale = %v, want 3 PIDs", stale)
		}
		if stale[0] != 100 {
			t.Fatalf("stale = %v, want the browser process 100 first", stale)
		}
	})

	t.Run("live host keeps the tree", func(t *testing.T) {
		records := map[uint32]processRecord{
			200: {pid: 200, parent: 300, exe: "msedgewebview2.exe", created: base},
			201: {pid: 201, parent: 200, exe: "msedgewebview2.exe", created: base.Add(time.Second)},
			300: {pid: 300, parent: 1, exe: "chemssh-launcher-webview2.exe", created: base.Add(-time.Hour)},
		}
		commandLines := map[uint32]string{200: profileCommand, 201: profileCommand}
		if stale := staleWebViewProcesses(records, commandLines, testProfileDir); len(stale) != 0 {
			t.Fatalf("stale = %v, want none", stale)
		}
	})

	t.Run("reused parent PID is treated as orphaned", func(t *testing.T) {
		records := map[uint32]processRecord{
			400: {pid: 400, parent: 500, exe: "msedgewebview2.exe", created: base},
			401: {pid: 401, parent: 400, exe: "msedgewebview2.exe", created: base.Add(time.Second)},
			500: {pid: 500, parent: 1, exe: "unrelated.exe", created: base.Add(time.Minute)},
		}
		commandLines := map[uint32]string{400: profileCommand, 401: profileCommand}
		stale := staleWebViewProcesses(records, commandLines, testProfileDir)
		if len(stale) != 2 || stale[0] != 400 {
			t.Fatalf("stale = %v, want [400 401]", stale)
		}
	})

	t.Run("other profile is ignored", func(t *testing.T) {
		records := map[uint32]processRecord{
			600: {pid: 600, parent: 50, exe: "msedgewebview2.exe", created: base},
		}
		commandLines := map[uint32]string{600: otherCommand}
		if stale := staleWebViewProcesses(records, commandLines, testProfileDir); len(stale) != 0 {
			t.Fatalf("stale = %v, want none", stale)
		}
	})

	t.Run("parent cycle is left alone", func(t *testing.T) {
		records := map[uint32]processRecord{
			700: {pid: 700, parent: 701, exe: "msedgewebview2.exe", created: base},
			701: {pid: 701, parent: 700, exe: "msedgewebview2.exe", created: base},
		}
		commandLines := map[uint32]string{700: profileCommand, 701: profileCommand}
		if stale := staleWebViewProcesses(records, commandLines, testProfileDir); len(stale) != 0 {
			t.Fatalf("stale = %v, want none", stale)
		}
	})
}

func TestProcessCommandLineReadsCurrentProcess(t *testing.T) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(os.Getpid()))
	if err != nil {
		t.Fatalf("OpenProcess: %v", err)
	}
	defer windows.CloseHandle(handle)
	commandLine, err := processCommandLine(handle)
	if err != nil {
		t.Fatalf("processCommandLine: %v", err)
	}
	if !strings.Contains(strings.ToLower(commandLine), strings.ToLower(filepath.Base(os.Args[0]))) {
		t.Fatalf("command line %q does not name the test binary %q", commandLine, filepath.Base(os.Args[0]))
	}
}

func TestCleanupStaleWebViewProcessesFindsNothing(t *testing.T) {
	killed, err := cleanupStaleWebViewProcesses(t.TempDir())
	if err != nil {
		t.Fatalf("cleanupStaleWebViewProcesses: %v", err)
	}
	if len(killed) != 0 {
		t.Fatalf("killed = %v, want none", killed)
	}
}
