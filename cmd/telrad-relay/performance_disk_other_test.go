//go:build !linux && !darwin && !windows && !relay_container

package main

import "errors"

func performanceDiskAvailable(string) (uint64, error) {
	return 0, errors.New("native performance measurement is unsupported on this platform")
}
