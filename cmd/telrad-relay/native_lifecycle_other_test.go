//go:build !windows && !linux && !relay_container

package main

import "testing"

func configureNativeTestCA(t *testing.T, _ string, _ []byte) {
	t.Fatal("native lifecycle requires Linux or Windows")
}
func checkSpoofedNativeEndpoint(t *testing.T) { t.Fatal("requires Linux or Windows") }
