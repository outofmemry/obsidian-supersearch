//go:build !windows

package main

import (
	"context"
	"os/exec"
)

// command builds an external tool's process; low = background work at low CPU
// priority, so OCR never competes with the editor.
func command(ctx context.Context, low bool, name string, args ...string) *exec.Cmd {
	if low {
		return exec.CommandContext(ctx, "nice", append([]string{"-n", "15", name}, args...)...)
	}
	return exec.CommandContext(ctx, name, args...)
}
