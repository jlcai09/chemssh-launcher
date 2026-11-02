package sftpclient

import (
	"testing"

	"chemssh-launcher/internal/config"
)

func TestSameServerProfile(t *testing.T) {
	base := config.Profile{ID: "profile-1", SSHHost: "example.com", SSHPort: 22, SSHUser: "user"}
	cases := []struct {
		name  string
		left  config.Profile
		right config.Profile
		want  bool
	}{
		{"same profile", base, base, true},
		{"same endpoint case-insensitive host", base, config.Profile{ID: "profile-2", SSHHost: "Example.COM", SSHPort: 22, SSHUser: "user"}, true},
		{"same endpoint default port", base, config.Profile{ID: "profile-2", SSHHost: "example.com", SSHPort: 0, SSHUser: "user"}, true},
		{"different host", base, config.Profile{ID: "profile-2", SSHHost: "other.example.com", SSHPort: 22, SSHUser: "user"}, false},
		{"different user", base, config.Profile{ID: "profile-2", SSHHost: "example.com", SSHPort: 22, SSHUser: "other"}, false},
		{"different port", base, config.Profile{ID: "profile-2", SSHHost: "example.com", SSHPort: 2222, SSHUser: "user"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameServerProfile(tc.left, tc.right); got != tc.want {
				t.Fatalf("sameServerProfile() = %v, want %v", got, tc.want)
			}
		})
	}
}

// An account without a host or user cannot be proven identical, so the copy must
// fall back to the streamed path.
func TestSameServerProfileRejectsIncompleteProfiles(t *testing.T) {
	blank := config.Profile{SSHHost: "", SSHUser: ""}
	if sameServerProfile(blank, blank) {
		t.Fatal("two empty profiles were treated as the same server")
	}
	if sameServerProfile(config.Profile{SSHHost: "example.com"}, config.Profile{SSHHost: "example.com"}) {
		t.Fatal("profiles without a login user were treated as the same server")
	}
}

func TestManagerSameServer(t *testing.T) {
	manager, _ := newTestManager(t)

	if !manager.SameServer("session-1", "session-1") {
		t.Fatal("a session is not the same server as itself")
	}
	if manager.SameServer("session-1", "missing") {
		t.Fatal("an unknown session was treated as the same server")
	}
	if manager.SameServer("", "") {
		t.Fatal("empty session ids were treated as the same server")
	}
}

func TestRemoteCopyArgvKeepsPathsAsArguments(t *testing.T) {
	argv := remoteCopyArgv([]RemoteCopyItem{
		{SourcePath: "/data/a b.txt", TargetPath: "/data/out/a b.txt"},
		{SourcePath: "/data/$HOME", TargetPath: "/data/out/$HOME"},
	})
	want := []string{
		"sh", "-c", remoteCopyScript, "sh",
		"/data/a b.txt", "/data/out/a b.txt",
		"/data/$HOME", "/data/out/$HOME",
	}
	if len(argv) != len(want) {
		t.Fatalf("argv length = %d, want %d", len(argv), len(want))
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q", i, argv[i], want[i])
		}
	}
}

func TestParseRemoteCopyOutput(t *testing.T) {
	items := []RemoteCopyItem{
		{SourcePath: "/data/a", TargetPath: "/out/a"},
		{SourcePath: "/data/b", TargetPath: "/out/b"},
		{SourcePath: "/data/c", TargetPath: "/out/c"},
	}
	output := "" +
		"CHEMSSH_COPY\t1\t0\t\n" +
		"some unrelated banner\n" +
		"CHEMSSH_COPY\t2\t1\tcp: cannot create '/out/b': Permission denied\n"

	results := parseRemoteCopyOutput(output, items)
	if len(results) != 3 {
		t.Fatalf("results length = %d, want 3", len(results))
	}
	if !results[0].OK || results[0].Error != "" {
		t.Fatalf("first item = %+v, want success", results[0])
	}
	if results[1].OK || results[1].Error != "cp: cannot create '/out/b': Permission denied" {
		t.Fatalf("second item = %+v, want the reported failure", results[1])
	}
	if results[2].OK || results[2].Error == "" {
		t.Fatalf("third item = %+v, want a failure without a reported line", results[2])
	}
	if results[1].TargetPath != "/out/b" {
		t.Fatalf("target path = %q, want /out/b", results[1].TargetPath)
	}
}
