package netcheck

import (
	"fmt"
	"net"
)

func IsPortAvailable(host string, port int) (bool, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		return false, nil
	}
	return true, ln.Close()
}

func CheckPortAvailable(host string, port int) error {
	ok, err := IsPortAvailable(host, port)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("local port occupied: %s:%d is already in use", host, port)
	}
	return nil
}

func SuggestFreePorts(host string, start, count int) ([]int, error) {
	if count <= 0 {
		return nil, nil
	}
	var ports []int
	for port := start + 1; port <= 65535 && len(ports) < count; port++ {
		ok, err := IsPortAvailable(host, port)
		if err != nil {
			return nil, err
		}
		if ok {
			ports = append(ports, port)
		}
	}
	return ports, nil
}
