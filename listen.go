package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"strconv"
	"strings"
)

// unixPrefix marks an address as a unix socket path, e.g.
// "unix:/run/sseredis/sseredis.sock".
const unixPrefix = "unix:"

// splitNetAddr returns the network ("unix" or "tcp") and the address to
// dial or listen on for addr: "unix:/path" is the unix socket /path,
// anything else a TCP host:port.
func splitNetAddr(addr string) (network, address string) {
	if path, ok := strings.CutPrefix(addr, unixPrefix); ok {
		return "unix", path
	}
	return "tcp", addr
}

// parseFileMode parses an octal permission mode such as "0660".
func parseFileMode(s string) (fs.FileMode, error) {
	m, err := strconv.ParseUint(s, 8, 32)
	if err != nil || m > 0o777 {
		return 0, fmt.Errorf("invalid mode %q: want octal permissions, e.g. 0660", s)
	}
	return fs.FileMode(m), nil
}

// listen listens on addr (see splitNetAddr). For a unix socket, mode is
// the octal permissions of the socket file (ignored for TCP). Closing the
// listener removes the socket file.
func listen(addr, mode string) (net.Listener, error) {
	network, address := splitNetAddr(addr)
	if network == "tcp" {
		return net.Listen("tcp", address)
	}
	perm, err := parseFileMode(mode)
	if err != nil {
		return nil, err
	}
	if err := removeStaleSocket(address); err != nil {
		return nil, err
	}
	l, err := net.Listen("unix", address)
	if err != nil {
		return nil, err
	}
	// Until the chmod the socket has the umask's permissions; with the
	// usual 022 only our own user can connect (connect needs write
	// permission), so the window is harmless.
	if err := os.Chmod(address, perm); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// removeStaleSocket removes the socket file left at path by a previous
// run that did not shut down cleanly (otherwise listen fails with
// "address already in use"). Anything that is not a socket is kept: a
// wrong path must not delete a regular file.
func removeStaleSocket(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode().Type() != fs.ModeSocket {
		return fmt.Errorf("%s exists and is not a socket, not removing it", path)
	}
	return os.Remove(path)
}
