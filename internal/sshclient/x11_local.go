package sshclient

import (
	"bytes"
	"chemssh-launcher/internal/config"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type x11Display struct {
	host, address  string
	number, screen int
}

func parseX11Display(value string) (x11Display, error) {
	var d x11Display
	colon := strings.LastIndex(value, ":")
	if colon < 0 {
		return d, errors.New("X11 display must be a loopback host:display[.screen]")
	}
	host := strings.Trim(value[:colon], "[]")
	if host == "" || strings.EqualFold(host, "localhost") {
		host = "127.0.0.1"
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return d, errors.New("X11 display must use a loopback IP or localhost")
	}
	parts := strings.Split(value[colon+1:], ".")
	if len(parts) > 2 {
		return d, errors.New("invalid X11 display")
	}
	number, err := strconv.Atoi(parts[0])
	if err != nil || number < 0 || number > 59535 {
		return d, errors.New("X11 display number must be between 0 and 59535")
	}
	screen := 0
	if len(parts) == 2 {
		screen, err = strconv.Atoi(parts[1])
		if err != nil || screen < 0 || screen > 65535 {
			return d, errors.New("invalid X11 screen number")
		}
	}
	return x11Display{host: host, address: net.JoinHostPort(host, strconv.Itoa(6000+number)), number: number, screen: screen}, nil
}

// The launcher connects to an existing server and never owns its process.
type localXServer struct {
	display x11Display
	cookie  []byte
}

func (*localXServer) Close() {}

func prepareLocalXServer(settings config.X11Config) (*localXServer, error) {
	settings = settings.Normalized()
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	display, err := parseX11Display(settings.Display)
	if err != nil {
		return nil, err
	}
	cookie, authErr := readXAuthority(settings.XAuthority, display)
	if authErr != nil {
		if settings.XAuthority != "" {
			return nil, authErr
		}
		// A server can authorize local connections without MIT cookies. Preserve
		// its policy; SSH still requires the synthetic cookie on every channel.
		cookie = nil
	}
	if err := verifyXServer(display, cookie); err != nil {
		return nil, fmt.Errorf("local X server unavailable or access denied at %s; start the server and check its access policy or Xauthority: %w", display.address, err)
	}
	return &localXServer{display: display, cookie: cookie}, nil
}
func verifyXServer(display x11Display, cookie []byte) error {
	conn, err := net.DialTimeout("tcp", display.address, 500*time.Millisecond)
	if err != nil {
		return fmt.Errorf("connect to local X server: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if err := writeX11All(conn, x11Setup(cookie, binary.LittleEndian)); err != nil {
		return errors.New("write local X11 authentication handshake failed")
	}
	header := make([]byte, 8)
	if _, err := io.ReadFull(conn, header); err != nil {
		return errors.New("local X server did not complete authentication")
	}
	if header[0] != 1 || binary.LittleEndian.Uint16(header[2:4]) != 11 || binary.LittleEndian.Uint16(header[4:6]) != 0 {
		return errors.New("local X server rejected X11 authentication")
	}
	return nil
}

func readXAuthority(path string, display x11Display) ([]byte, error) {
	if path == "" {
		path = os.Getenv("XAUTHORITY")
	}
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, errors.New("cannot locate Xauthority; configure its path")
		}
		path = filepath.Join(home, ".Xauthority")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot read Xauthority; configure the existing X server's authority file")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 {
		return nil, errors.New("Xauthority exceeds the size limit or cannot be read")
	}
	return selectXAuthority(data, display)
}
func selectXAuthority(data []byte, display x11Display) ([]byte, error) {
	reader := bytes.NewReader(data)
	bestScore := -1
	var best []byte
	for reader.Len() > 0 {
		var family uint16
		if binary.Read(reader, binary.BigEndian, &family) != nil {
			return nil, errors.New("malformed Xauthority record")
		}
		fields := make([][]byte, 4)
		for i := range fields {
			var size uint16
			if binary.Read(reader, binary.BigEndian, &size) != nil || int(size) > reader.Len() {
				return nil, errors.New("malformed Xauthority field")
			}
			fields[i] = make([]byte, int(size))
			if _, err := io.ReadFull(reader, fields[i]); err != nil {
				return nil, errors.New("malformed Xauthority field")
			}
		}
		if string(fields[1]) != strconv.Itoa(display.number) || string(fields[2]) != x11AuthProtocol || len(fields[3]) != 16 {
			continue
		}
		score := -1
		switch family {
		case 0:
			if len(fields[0]) == 4 && net.IP(fields[0]).Equal(net.ParseIP(display.host)) {
				score = 3
			}
		case 6:
			if len(fields[0]) == 16 && net.IP(fields[0]).Equal(net.ParseIP(display.host)) {
				score = 3
			}
		case 256:
			hostname, _ := os.Hostname()
			if len(fields[0]) == 0 || string(fields[0]) == hostname || strings.EqualFold(string(fields[0]), "localhost") {
				score = 2
			}
		case 65535:
			score = 1
		}
		if score > bestScore {
			bestScore = score
			best = append([]byte(nil), fields[3]...)
		}
	}
	if best == nil {
		return nil, errors.New("no matching MIT-MAGIC-COOKIE-1 credential in Xauthority for this display")
	}
	return best, nil
}
func encodeXAuthority(family uint16, address []byte, display string, cookie []byte) []byte {
	var out bytes.Buffer
	_ = binary.Write(&out, binary.BigEndian, family)
	for _, field := range [][]byte{address, []byte(display), []byte(x11AuthProtocol), cookie} {
		_ = binary.Write(&out, binary.BigEndian, uint16(len(field)))
		_, _ = out.Write(field)
	}
	return out.Bytes()
}
