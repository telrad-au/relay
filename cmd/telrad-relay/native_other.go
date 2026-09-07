//go:build !linux && !windows && !relay_container

package main

import (
	"context"
	"errors"
	"net"
)

func requireServiceIdentity() error                          { return errors.New("native services require Linux or Windows") }
func managementPeerIsAdministrator(net.Conn) bool            { return false }
func listenManagement() ([]managementListener, error)        { return nil, requireServiceIdentity() }
func dialManagement(context.Context, bool) (net.Conn, error) { return nil, requireServiceIdentity() }

func prepareNativeInstallation() error {
	return errors.New("native installation requires Linux or Windows")
}
func nativeServiceRunning() (bool, error)         { return false, prepareNativeInstallation() }
func nativeServiceFiles() []string                { return nil }
func configureNativeService(string) error         { return prepareNativeInstallation() }
func secureNativeState() error                    { return prepareNativeInstallation() }
func reloadNativeService() error                  { return prepareNativeInstallation() }
func snapshotNativeSystem() (func() error, error) { return nil, prepareNativeInstallation() }
