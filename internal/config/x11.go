package config

import (
	"net"
	"strconv"
	"strings"
)

const DefaultX11Display = "127.0.0.1:0"

// X11Config stores local X server settings, never authentication cookies.
type X11Config struct {
	Enabled    bool   `json:"enabled"`
	Display    string `json:"display"`
	XAuthority string `json:"xauthority"`
}

func DefaultX11Config() X11Config { return X11Config{Display: DefaultX11Display} }

func (x X11Config) Normalized() X11Config {
	x.Display = strings.TrimSpace(x.Display)
	if x.Display == "" {
		x.Display = DefaultX11Display
	}
	x.XAuthority = strings.TrimSpace(x.XAuthority)
	return x
}

// Validate accepts explicit loopback TCP displays, with port 6000+n and screen 0-255.
func (x X11Config) Validate() error {
	x = x.Normalized()
	separator := strings.LastIndexByte(x.Display, ':')
	if separator < 1 {
		return invalidProfile("X11 display must be a loopback TCP display such as 127.0.0.1:0.0")
	}
	host := x.Display[:separator]
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return invalidProfile("X11 display host must be localhost or a loopback IP address")
	}
	parts := strings.Split(x.Display[separator+1:], ".")
	if len(parts) > 2 || !x11Digits(parts[0]) {
		return invalidProfile("X11 display number must be between 0 and 59535")
	}
	display, err := strconv.Atoi(parts[0])
	if err != nil || display > 65535-6000 {
		return invalidProfile("X11 display number must be between 0 and 59535")
	}
	if len(parts) == 2 {
		screen, err := strconv.Atoi(parts[1])
		if !x11Digits(parts[1]) || err != nil || screen > 255 {
			return invalidProfile("X11 screen number must be between 0 and 255")
		}
	}
	if strings.ContainsRune(x.XAuthority, '\x00') {
		return invalidProfile("X11 paths must not contain NUL bytes")
	}
	return nil
}

func x11Digits(value string) bool {
	if value == "" {
		return false
	}
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}
