//go:build !darwin && !linux

package activity

import (
	"context"
	"errors"
	"runtime"
	"time"
)

var osName = runtime.GOOS

func probe(ctx context.Context, wantTitle bool, idleAfter time.Duration) (sample, error) {
	return sample{}, errors.New("activity collection works on macOS and Linux only")
}

func windows(ctx context.Context, apps []string) ([][2]string, error) { return nil, nil }
