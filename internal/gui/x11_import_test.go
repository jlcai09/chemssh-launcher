package gui

import (
	"testing"

	"chemssh-launcher/internal/config"
)

func TestImportedX11ProfileNormalization(t *testing.T) {
	remote := config.NewProfileDefaults()
	remote.X11 = config.X11Config{Enabled: true, Display: " 127.0.0.1:1 ", XAuthority: " /home/user/.Xauthority "}
	got := normalizeImportedProfile(remote)
	if !got.X11.Enabled || got.X11.Display != "127.0.0.1:1" || got.X11.XAuthority != "/home/user/.Xauthority" {
		t.Fatalf("remote X11 settings not normalized: %+v", got.X11)
	}
	local := config.NewLocalProfileDefaults()
	local.X11 = config.X11Config{Enabled: true}
	got = normalizeImportedProfile(local)
	if got.X11.Enabled || got.X11.Display != config.DefaultX11Display {
		t.Fatalf("local profile retained X11 or lost defaults: %+v", got.X11)
	}
}
