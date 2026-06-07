package sftpclient

import "testing"

func TestCleanPath(t *testing.T) {
	tests := map[string]string{
		"":                 ".",
		"   ":              ".",
		".":                ".",
		"/":                "/",
		"/home/user/../x":  "/home/x",
		`home\user\file`:   "home/user/file",
		"~/project":        "~/project",
		"relative//folder": "relative/folder",
	}
	for input, want := range tests {
		if got := CleanPath(input); got != want {
			t.Fatalf("CleanPath(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestParentPath(t *testing.T) {
	tests := map[string]string{
		".":              ".",
		"/":              "/",
		"file":           ".",
		"dir/file":       "dir",
		"/home/user":     "/home",
		"/home/user/app": "/home/user",
	}
	for input, want := range tests {
		if got := ParentPath(input); got != want {
			t.Fatalf("ParentPath(%q) = %q, want %q", input, got, want)
		}
	}
}
