//go:build linux && !relay_container

package main

import (
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"net"
	"os"
	"os/user"
	"strconv"
)

const managementSocket = "/run/telrad-relay/management.sock"

func serviceUID() (uint32, error) {
	u, err := user.Lookup(linuxServiceUser)
	if err != nil {
		return 0, err
	}
	id, err := strconv.ParseUint(u.Uid, 10, 32)
	return uint32(id), err
}
func requireServiceIdentity() error {
	uid, err := serviceUID()
	if err != nil {
		return err
	}
	if uint32(os.Geteuid()) != uid || uid == 0 {
		return errors.New("managed Relay must run as telrad-relay")
	}
	return nil
}
func socketPeer(conn net.Conn) (*unix.Ucred, error) {
	c, ok := conn.(*net.UnixConn)
	if !ok {
		return nil, errors.New("management requires a Unix socket")
	}
	raw, err := c.SyscallConn()
	if err != nil {
		return nil, err
	}
	var peer *unix.Ucred
	var peerErr error
	err = raw.Control(func(fd uintptr) { peer, peerErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
	if err != nil {
		return nil, err
	}
	return peer, peerErr
}
func managementPeerIsAdministrator(conn net.Conn) bool {
	peer, err := socketPeer(conn)
	return err == nil && peer.Uid == 0
}
func listenManagement() ([]managementListener, error) {
	if info, err := os.Lstat(managementSocket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("management endpoint is not a socket")
		}
		if err := os.Remove(managementSocket); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.Listen("unix", managementSocket)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(managementSocket, 0666); err != nil {
		listener.Close()
		return nil, err
	}
	return []managementListener{{listener, true}}, nil
}
func dialManagement(ctx context.Context, _ bool) (net.Conn, error) {
	c, err := (&net.Dialer{}).DialContext(ctx, "unix", managementSocket)
	if err != nil {
		return nil, err
	}
	peer, err := socketPeer(c)
	uid, uidErr := serviceUID()
	if err != nil || uidErr != nil || peer.Uid != uid {
		c.Close()
		return nil, errors.New("Relay management peer identity is invalid")
	}
	return c, nil
}
