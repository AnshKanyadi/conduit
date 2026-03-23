//go:build linux

package pty

// Spawn for Linux: full namespace isolation + seccomp via the re-exec pattern.
// See sandbox_linux.go for the re-exec design rationale.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// Spawn starts an isolated shell process attached to a PTY.
//
// Isolation layers applied on Linux:
//
//  1. PID namespace (CLONE_NEWPID): the shell's own PID inside the namespace
//     is 1. If the shell spawns child processes and the shell dies, all its
//     children are killed automatically — the kernel treats PID 1 dying as
//     "container exit". No zombie sub-processes leak into the host.
//
//  2. Mount namespace (CLONE_NEWNS): the shell gets a copy of the host's
//     mount table. Future layers can pivot_root or bind-mount to give it an
//     isolated filesystem view without affecting the host.
//
//  3. UTS namespace (CLONE_NEWUTS): the shell can have its own hostname
//     (set to the session ID) without changing the host's hostname.
//
//  4. Network namespace (CLONE_NEWNET): the shell gets a loopback interface
//     only — no access to the host network or other sessions' traffic.
//
//  5. Seccomp BPF denylist (via re-exec): applied in the child before the
//     shell starts. Blocks ~15 high-risk syscalls (mount, reboot, kexec,
//     BPF program loading, etc.) regardless of any other privilege mechanism.
func Spawn(ctx context.Context, cfg Config) (*Process, error) {
	if len(cfg.Command) == 0 {
		return nil, fmt.Errorf("pty: Spawn: command must not be empty")
	}

	// Use /proc/self/exe so the re-exec'd binary is identical to the running
	// binary even if the binary has been updated on disk since startup.
	// os.Executable() follows symlinks, /proc/self/exe is always the real path.
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("pty: resolve self executable: %w", err)
	}

	// Encode the target command as JSON so the sandbox init can reconstruct
	// it exactly. Using JSON avoids shell-quoting ambiguity.
	cmdJSON, err := json.Marshal(cfg.Command)
	if err != nil {
		return nil, fmt.Errorf("pty: marshal sandbox command: %w", err)
	}

	// Build the command. We re-exec our own binary (not the shell directly)
	// so that sandbox_linux.go's init() can apply seccomp before exec'ing the shell.
	cmd := exec.CommandContext(ctx, exe)
	cmd.Env = buildSandboxEnv(cfg.Env, string(cmdJSON))

	// Apply Linux namespaces via Cloneflags. These are passed to clone(2)
	// internally by exec.Cmd.Start() via SysProcAttr. The Setsid and Setctty
	// fields are added by startWithPTY (in process.go) after this returns.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | // isolated PID space
			syscall.CLONE_NEWNS | // isolated mount table
			syscall.CLONE_NEWUTS | // isolated hostname
			syscall.CLONE_NEWNET, // isolated network stack
	}

	ptmx, err := startWithPTY(cmd, cfg.Rows, cfg.Cols)
	if err != nil {
		return nil, fmt.Errorf("pty: start with pty: %w", err)
	}

	p := &Process{
		ptmx:   ptmx,
		cmd:    cmd,
		waitCh: make(chan error, 1),
	}

	// Reap the process in a background goroutine. This goroutine runs for the
	// lifetime of the session and guarantees the process is never a zombie.
	go reapProcess(cmd, p.waitCh, ptmx)

	return p, nil
}

// buildSandboxEnv constructs the environment for the re-exec'd sandbox init.
// We pass a minimal base environment plus the user's requested vars and our
// internal control variables. The internal vars are stripped from the shell's
// environment in sandbox_linux.go before exec.
func buildSandboxEnv(userEnv []string, cmdJSON string) []string {
	base := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"TERM=xterm-256color",
		// Sandbox control variables (stripped before reaching the shell).
		sandboxEnvFlag + "=1",
		sandboxEnvCmd + "=" + cmdJSON,
	}
	return append(base, userEnv...)
}
