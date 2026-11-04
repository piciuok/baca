//go:build !darwin && !linux

package activity

import (
	"context"
	"errors"
	"runtime"
)

var osName = runtime.GOOS

func probe(ctx context.Context, wantTitle bool) (sample, error) {
	return sample{}, errors.New("activity collection works on macOS and Linux only")
}
