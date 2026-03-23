//go:build darwin

package pty

// Spawn for Darwin (macOS): PTY attachment WITHOUT namespace isolation.
//
// macOS does not support Linux namespaces. This implementation exists solely
// for local development — it lets engineers run and test Conduit on a Mac
// without a Linux VM. It MUST NOT be used in production deployments.
//
// What you get on Darwin:
//   - Full PTY attachment (reads, writes, resize all work)
//   - Clean process reaping via the waitCh goroutine
//   - Context cancellation
//
// What you do NOT get on Darwin:
//   - PID/mount/UTS/network namespace isolation
//   - Seccomp syscall filtering
//   - Any process-level security boundary

import (
	"context"
	"fmt"
	"os/exec"

	creackpty "github.com/creack/pty"
)

// Spawn starts a shell with a PTY on macOS. No isolation is applied.
// WARNING: This is a development stub. Production must use Linux with
// the namespace + seccomp spawn (spawn_linux.go).
func Spawn(ctx context.Context, cfg Config) (*Process, error) {
	if len(cfg.Command) == 0 {
		return nil, fmt.Errorf("pty: Spawn: command must not be empty")
	}

	// On Darwin we exec the shell directly — no re-exec needed because
	// there's no seccomp to install. We still attach a PTY so the Process
	// API is identical to the Linux version.
	cmd := exec.CommandContext(ctx, cfg.Command[0], cfg.Command[1:]...)
	if len(cfg.Env) > 0 {
		cmd.Env = cfg.Env
	}

	// Use creack/pty's StartWithSize rather than our custom startWithPTY.
	//
	// Why? StartWithSize uses an internal findFD() helper to compute the
	// correct Ctty (controlling-terminal fd number in the child). That value
	// depends on how many ExtraFiles the cmd has and the order of
	// Stdin/Stdout/Stderr assignment — hardcoding Ctty:0 can cause the shell
	// to stall on macOS waiting for terminal setup that never completes.
	// Using the library's own launcher is the safe, tested path for Darwin.
	ptmx, err := creackpty.StartWithSize(cmd, &creackpty.Winsize{
		Rows: cfg.Rows,
		Cols: cfg.Cols,
	})
	if err != nil {
		return nil, fmt.Errorf("pty: start with pty: %w", err)
	}

	p := &Process{
		ptmx:   ptmx,
		cmd:    cmd,
		waitCh: make(chan error, 1),
	}

	// Same goroutine-based reaping as the Linux path — same Process API,
	// same zero-zombie guarantee on Darwin.
	go reapProcess(cmd, p.waitCh, ptmx)

	return p, nil
}
