package netcheck

import (
	"net"
	"testing"
)

func TestIsPortAvailable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	ok, err := IsPortAvailable("127.0.0.1", port)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected listener port to be unavailable")
	}
}

func TestSuggestFreePorts(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	ports, err := SuggestFreePorts("127.0.0.1", port, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ports) != 2 {
		t.Fatalf("expected 2 suggestions, got %v", ports)
	}
	for _, suggested := range ports {
		if suggested <= port {
			t.Fatalf("expected suggestions above %d, got %v", port, ports)
		}
	}
}
