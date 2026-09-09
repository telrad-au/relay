//go:build !linux && !windows

package main

import (
	"context"
	"errors"
	"io"
)

func observe(context.Context, io.Writer, int, bool) error {
	return errors.New("run the observer in Docker or on a native Linux/Windows test host")
}

func nativeExecutable(context.Context, int) (string, error) {
	return "", errors.New("native observation requires Linux or Windows")
}
