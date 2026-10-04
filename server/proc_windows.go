package main

import (
	"context"
	"os/exec"
	"syscall"
)

const (
	createNoWindow           = 0x08000000
	belowNormalPriorityClass = 0x00004000
)

// command builds an external tool's process; low = background work at low CPU
// priority. No console window ever flashes up: the server runs hidden under
// Obsidian, and each console tool would otherwise open its own window.
func command(ctx context.Context, low bool, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	flags := uint32(createNoWindow)
	if low {
		flags |= belowNormalPriorityClass
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags}
	return cmd
}
