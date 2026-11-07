package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestX11DefaultsAndNormalization(t *testing.T) {
	want := DefaultX11Config()
	if want.Enabled || want.Display != "127.0.0.1:0" || want.XAuthority != "" {
		t.Fatalf("unexpected defaults: %+v", want)
	}
	if got := (X11Config{}).Normalized(); got != want {
		t.Fatalf("zero normalized = %+v", got)
	}
	got := (X11Config{Enabled: true, Display: " 127.0.0.1:1.2 ", XAuthority: " /tmp/example.Xauthority "}).Normalized()
	if !got.Enabled || got.Display != "127.0.0.1:1.2" || got.XAuthority != "/tmp/example.Xauthority" {
		t.Fatalf("normalized = %+v", got)
	}
	if NewProfileDefaults().X11 != want || NewLocalProfileDefaults().X11Active() {
		t.Fatal("profile X11 defaults incorrect")
	}
}

func TestX11ValidateLoopbackTCPDisplays(t *testing.T) {
	for _, display := range []string{"", "127.0.0.1:0", "127.0.0.1:0.0", "localhost:1.255", "127.0.0.2:59535.0", "[::1]:0.1"} {
		t.Run("valid_"+display, func(t *testing.T) {
			if err := (X11Config{Display: display}).Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, display := range []string{":0", "unix:0", "example.com:0", "192.0.2.1:0", "0.0.0.0:0", "[::]:0", "127.0.0.1:-1", "127.0.0.1:+1", "127.0.0.1:59536", "127.0.0.1:99999999999999999999999", "127.0.0.1:0.-1", "127.0.0.1:0.256", "127.0.0.1:0.", "127.0.0.1:0.0.0", "127.0.0.1:0/path", "127.0.0.1: 0", "localhost:", "localhost:0\x00"} {
		t.Run("invalid_"+display, func(t *testing.T) {
			if err := (X11Config{Display: display}).Validate(); !errors.Is(err, ErrProfileInvalid) {
				t.Fatalf("Validate = %v", err)
			}
		})
	}
	if err := (X11Config{XAuthority: "bad\x00path"}).Validate(); err == nil {
		t.Fatal("NUL path accepted")
	}
}

func TestProfileX11ValidationAndLocalExclusion(t *testing.T) {
	p := NewProfileDefaults()
	p.Name = "Example"
	p.SSHHost = "example.com"
	p.SSHUser = "user"
	p.X11 = X11Config{Enabled: true, Display: "example.com:0"}
	if !p.X11Active() || !errors.Is(p.Validate(), ErrProfileInvalid) {
		t.Fatal("remote enabled X11 not validated")
	}
	p.X11.Enabled = false
	if p.X11Active() || p.Validate() != nil {
		t.Fatal("disabled X11 blocked profile")
	}
	p.Kind = ProfileKindLocal
	p.X11.Enabled = true
	if p.X11Active() || p.Validate() != nil {
		t.Fatal("local profile activated X11")
	}
}

func TestX11ProfileJSONAndStoreNormalization(t *testing.T) {
	p := NewProfileDefaults()
	p.ID = "example-profile"
	p.Name = "Example"
	p.X11 = X11Config{Enabled: true, Display: "127.0.0.1:1.2", XAuthority: "/tmp/example.Xauthority"}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	var x11 map[string]any
	if err = json.Unmarshal(fields["x11"], &x11); err != nil {
		t.Fatal(err)
	}
	if len(x11) != 3 || x11["enabled"] != true || x11["display"] != p.X11.Display || x11["xauthority"] != p.X11.XAuthority {
		t.Fatalf("JSON fields: %s", data)
	}
	var roundTrip Profile
	if err = json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.X11 != p.X11 {
		t.Fatalf("round trip = %+v", roundTrip.X11)
	}
	path := filepath.Join(t.TempDir(), "profiles.json")
	if err = os.WriteFile(path, []byte(`{"version":1,"profiles":[{"id":"legacy","name":"Example"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	store := NewFileStore(path)
	legacy, err := store.Get("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if legacy.X11 != DefaultX11Config() {
		t.Fatalf("legacy defaults = %+v", legacy.X11)
	}
	if err = store.Save(p); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.X11 != p.X11 {
		t.Fatalf("stored = %+v", stored.X11)
	}
	p.X11 = X11Config{Enabled: true}
	if err = store.Save(p); err != nil {
		t.Fatal(err)
	}
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bytes), `"display": "127.0.0.1:0"`) || strings.Contains(string(bytes), "cookie") {
		t.Fatalf("saved X11 normalization incorrect: %s", bytes)
	}
}
