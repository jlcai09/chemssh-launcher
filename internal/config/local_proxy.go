package config

import (
	"net"
	"strconv"
	"strings"
)

const (
	DefaultLocalProxyHost    = "127.0.0.1"
	DefaultLocalProxyPort    = 7890
	DefaultLocalProxyNoProxy = "localhost,127.0.0.1,::1"
)

// LocalProxy points the remote side of a session at a proxy that only exists on
// the machine running the launcher.
type LocalProxy struct {
	Enabled bool   `json:"enabled"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	NoProxy string `json:"no_proxy"`
}

// EnvPair is one proxy environment variable shared by the remote command, the
// launcher terminal, and the ChemSSH terminal bridge file.
type EnvPair struct {
	Key   string
	Value string
}

func DefaultLocalProxy() LocalProxy {
	return LocalProxy{
		Host:    DefaultLocalProxyHost,
		Port:    DefaultLocalProxyPort,
		NoProxy: DefaultLocalProxyNoProxy,
	}
}

// Normalized fills fields that older profile files never stored. A fully empty
// value is a legacy profile, so no_proxy gets its default too; a partially
// filled value keeps no_proxy as the user left it (including empty).
func (lp LocalProxy) Normalized() LocalProxy {
	if strings.TrimSpace(lp.Host) == "" && lp.Port == 0 && strings.TrimSpace(lp.NoProxy) == "" {
		lp.Host = DefaultLocalProxyHost
		lp.Port = DefaultLocalProxyPort
		lp.NoProxy = DefaultLocalProxyNoProxy
		return lp
	}
	if strings.TrimSpace(lp.Host) == "" {
		lp.Host = DefaultLocalProxyHost
	}
	if lp.Port == 0 {
		lp.Port = DefaultLocalProxyPort
	}
	return lp
}

func (lp LocalProxy) Address() string {
	return net.JoinHostPort(strings.TrimSpace(lp.Host), strconv.Itoa(lp.Port))
}

func (lp LocalProxy) URL() string {
	return "http://" + lp.Address()
}

func (lp LocalProxy) IsLoopback() bool {
	host := strings.TrimSpace(lp.Host)
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ProxyEnvPairs returns the variables that point proxy-aware programs at
// proxyURL. Both spellings are emitted because tools disagree on case.
func ProxyEnvPairs(proxyURL, noProxy string) []EnvPair {
	pairs := []EnvPair{
		{Key: "http_proxy", Value: proxyURL},
		{Key: "https_proxy", Value: proxyURL},
		{Key: "all_proxy", Value: proxyURL},
		{Key: "HTTP_PROXY", Value: proxyURL},
		{Key: "HTTPS_PROXY", Value: proxyURL},
		{Key: "ALL_PROXY", Value: proxyURL},
	}
	if strings.TrimSpace(noProxy) != "" {
		pairs = append(pairs,
			EnvPair{Key: "no_proxy", Value: noProxy},
			EnvPair{Key: "NO_PROXY", Value: noProxy},
		)
	}
	return pairs
}
