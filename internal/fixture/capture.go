package fixture

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

var ErrCaptureCleanup = errors.New("usage capture cleanup failed")

type captureUsageOps struct {
	remove func(string) error
	write  func(string, []byte, os.FileMode) error
}

// CaptureUsage owns the unsanitized bytes at one fixed path. It removes a
// residual before capture and confirms removal before returning any fixture.
func CaptureUsage(ctx context.Context, workspace, provider string, capture func(context.Context) ([]byte, error)) ([]byte, error) {
	rawPath := filepath.Join(workspace, provider+".raw.json")
	if err := os.Remove(rawPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, ErrCaptureCleanup
	}
	if err := os.MkdirAll(workspace, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(workspace, 0700); err != nil {
		return nil, err
	}
	return captureUsage(ctx, rawPath, capture, captureUsageOps{remove: os.Remove, write: os.WriteFile})
}

func captureUsage(ctx context.Context, rawPath string, capture func(context.Context) ([]byte, error), ops captureUsageOps) (sanitized []byte, err error) {
	if err := ops.remove(rawPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, ErrCaptureCleanup
	}
	defer func() {
		if cleanupErr := ops.remove(rawPath); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
			sanitized = nil
			err = ErrCaptureCleanup
		}
	}()
	raw, err := capture(ctx)
	if err != nil {
		return nil, err
	}
	if err := ops.write(rawPath, raw, 0600); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sanitized, err = Sanitize(raw, Usage)
	if err != nil {
		return nil, err
	}
	if err := Scan(sanitized, Usage); err != nil {
		return nil, err
	}
	return sanitized, nil
}
