package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalProxyDefaults(t *testing.T) {
	proxy := DefaultLocalProxy()
	if proxy.Enabled {
		t.Fatal("local proxy should be opt-in")
	}
	if proxy.Host != "127.0.0.1" || proxy.Port != 7890 {
		t.Fatalf("unexpected defaults: %+v", proxy)
	}
	if proxy.URL() != "http://127.0.0.1:7890" {
		t.Fatalf("unexpected URL: %s", proxy.URL())
	}
	if !proxy.IsLoopback() {
		t.Fatal("default proxy should be loopback")
	}
}

func TestLocalProxyURLBracketsIPv6(t *testing.T) {
	proxy := LocalProxy{Host: "::1", Port: 7890}
	if got := proxy.URL(); got != "http://[::1]:7890" {
		t.Fatalf("URL = %q", got)
	}
}

func TestLocalProxyNormalizedLegacyZeroValue(t *testing.T) {
	got := LocalProxy{}.Normalized()
	if got.Host != DefaultLocalProxyHost || got.Port != DefaultLocalProxyPort || got.NoProxy != DefaultLocalProxyNoProxy {
		t.Fatalf("legacy zero value normalized to %+v", got)
	}
}

func TestLocalProxyNormalizedKeepsExplicitEmptyNoProxy(t *testing.T) {
	got := LocalProxy{Host: "127.0.0.1", Port: 1080}.Normalized()
	if got.NoProxy != "" {
		t.Fatalf("no_proxy should stay empty, got %q", got.NoProxy)
	}
}

func TestProxyEnvPairsSetsBothSpellings(t *testing.T) {
	pairs := ProxyEnvPairs("http://127.0.0.1:41000", "localhost,127.0.0.1")
	want := map[string]string{
		"http_proxy":  "http://127.0.0.1:41000",
		"https_proxy": "http://127.0.0.1:41000",
		"all_proxy":   "http://127.0.0.1:41000",
		"HTTP_PROXY":  "http://127.0.0.1:41000",
		"HTTPS_PROXY": "http://127.0.0.1:41000",
		"ALL_PROXY":   "http://127.0.0.1:41000",
		"no_proxy":    "localhost,127.0.0.1",
		"NO_PROXY":    "localhost,127.0.0.1",
	}
	if len(pairs) != len(want) {
		t.Fatalf("got %d pairs, want %d: %+v", len(pairs), len(want), pairs)
	}
	for _, pair := range pairs {
		if want[pair.Key] != pair.Value {
			t.Fatalf("unexpected pair %s=%q", pair.Key, pair.Value)
		}
		delete(want, pair.Key)
	}
	if len(want) != 0 {
		t.Fatalf("missing pairs: %+v", want)
	}
}

func TestProxyEnvPairsOmitsBlankNoProxy(t *testing.T) {
	pairs := ProxyEnvPairs("http://127.0.0.1:41000", "  ")
	for _, pair := range pairs {
		if strings.EqualFold(pair.Key, "no_proxy") {
			t.Fatalf("blank no_proxy should be omitted: %+v", pairs)
		}
	}
}

func TestProfileValidateChecksEnabledLocalProxy(t *testing.T) {
	p := NewProfileDefaults()
	p.Name = "Example Server"
	p.SSHHost = "example.com"
	p.SSHUser = "user"
	p.LocalProxy.Enabled = true

	p.LocalProxy.Host = ""
	if err := p.Validate(); !errors.Is(err, ErrProfileInvalid) || !strings.Contains(err.Error(), "local proxy host is required") {
		t.Fatalf("empty host accepted: %v", err)
	}

	p.LocalProxy.Host = "127.0.0.1"
	p.LocalProxy.Port = 0
	if err := p.Validate(); !errors.Is(err, ErrProfileInvalid) || !strings.Contains(err.Error(), "local proxy port") {
		t.Fatalf("zero port accepted: %v", err)
	}

	p.LocalProxy.Port = 7890
	if err := p.Validate(); err != nil {
		t.Fatalf("valid local proxy rejected: %v", err)
	}
}

func TestProfileValidateIgnoresLocalProxyWhenDisabled(t *testing.T) {
	p := NewProfileDefaults()
	p.Name = "Example Server"
	p.SSHHost = "example.com"
	p.SSHUser = "user"
	p.LocalProxy = LocalProxy{Enabled: false, Host: "", Port: 0}
	if err := p.Validate(); err != nil {
		t.Fatalf("disabled local proxy should not be validated: %v", err)
	}
}

func TestProfileValidateIgnoresLocalProxyOnLocalProfile(t *testing.T) {
	p := NewLocalProfileDefaults()
	p.LocalProxy = LocalProxy{Enabled: true, Host: "", Port: 70000}
	if err := p.Validate(); err != nil {
		t.Fatalf("local profile should ignore local proxy: %v", err)
	}
	if p.ProxyActive() {
		t.Fatal("local profile must not report an active reverse proxy channel")
	}
}

func TestProfileWarningsReportsNonLoopbackLocalProxy(t *testing.T) {
	p := NewProfileDefaults()
	p.Name = "Example Server"
	p.SSHHost = "example.com"
	p.SSHUser = "user"
	p.LocalProxy.Enabled = true
	p.LocalProxy.Host = "192.0.2.10"
	warnings := p.Warnings()
	if len(warnings) != 1 || !strings.Contains(warnings[0], "not loopback") {
		t.Fatalf("warnings = %v, want a non-loopback proxy warning", warnings)
	}
}

func TestFileStoreFillsLocalProxyDefaultsForLegacyProfiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.json")
	legacy := `{
  "version": 1,
  "profiles": [
    {
      "id": "legacy",
      "kind": "remote",
      "name": "Legacy",
      "ssh_host": "example.com",
      "ssh_port": 22,
      "ssh_user": "user",
      "auth_method": "password",
      "remote_host": "127.0.0.1",
      "remote_port": 8888,
      "local_host": "127.0.0.1",
      "local_port": 8888,
      "local_url_path": "/"
    }
  ]
}
`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	profile, err := NewFileStore(path).Get("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if profile.LocalProxy.Enabled {
		t.Fatal("legacy profile must keep the local proxy disabled")
	}
	if profile.LocalProxy.Host != DefaultLocalProxyHost || profile.LocalProxy.Port != DefaultLocalProxyPort {
		t.Fatalf("legacy profile got %+v", profile.LocalProxy)
	}
	if profile.LocalProxy.NoProxy != DefaultLocalProxyNoProxy {
		t.Fatalf("legacy no_proxy = %q", profile.LocalProxy.NoProxy)
	}
}
