//go:build windows

package main

import "os"

func preserveTargetOwner(_ *os.File, _ string) error { return nil }
