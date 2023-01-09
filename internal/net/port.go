package net

import (
	"fmt"
	"net"
)

// GetEphemeralTCPPort returns a port in the ephemeral range that is likely free.
// This is obviously racy but is very unlikely to fail as long as you bind the port ASAP.
func GetEphemeralTCPPort() (int, error) {
	addr, err := net.ResolveTCPAddr("tcp", "localhost:0")
	if err != nil {
		return 0, fmt.Errorf("resolving localhost:0: %w", err)
	}
	listener, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return 0, fmt.Errorf("listening to acquire port: %w", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}
