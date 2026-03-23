//go:build linux

package pty

// The re-exec sandbox pattern.
//
// Problem: In Go, you cannot run arbitrary code between fork() and exec()
// because the Go runtime doesn't support it. The runtime may have multiple
// threads, and after fork() only the forking thread survives in the child —
// the Go scheduler, GC, and other threads are gone. This makes it impossible
// to do complex setup (like installing a seccomp BPF filter) between fork
// and exec using normal Go code.
//
// Solution (the industry standard, used by Docker/runc/Chrome/Firefox):
// Re-execute our own binary with a sentinel environment variable. The child
// binary's init() detects the variable, performs the setup, then calls
// syscall.Exec() to replace itself with the real shell. This way all setup
// (seccomp, namespace configuration, etc.) runs after exec — in a fresh,
// single-threaded process — before the shell starts.
//
// Flow:
//   Parent:              Spawn() → exec("/proc/self/exe") with _CONDUIT_SANDBOX=1
//   Child init():        sees _CONDUIT_SANDBOX=1 → sandboxInit()
//   sandboxInit():       applyNoNewPrivs() → applySeccomp() → syscall.Exec(shell)
//   Shell:               runs inside the PTY with namespaces + seccomp

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"syscall"
)

const (
	// sandboxEnvFlag is the sentinel that tells a re-exec'd binary it should
	// run as the sandbox init, not as the normal server. We use a leading
	// underscore to signal that this is an internal, not a user-facing, var.
	sandboxEnvFlag = "_CONDUIT_SANDBOX"

	// sandboxEnvCmd carries the JSON-encoded command ([]string) for the shell
	// that the sandbox init will exec into.
	sandboxEnvCmd = "_CONDUIT_SANDBOX_CMD"
)

// init runs before main() in every binary that imports this package.
// On normal server startup, _CONDUIT_SANDBOX is not set, so we return
// immediately and have zero overhead.
//
// When Spawn() re-execs our binary with _CONDUIT_SANDBOX=1, this init
// fires in the child, applies security setup, and execs the shell.
// The shell then replaces this init process — the Go runtime never
// reaches main() in this child.
func init() {
	if os.Getenv(sandboxEnvFlag) != "1" {
		return // normal server startup, do nothing
	}
	sandboxInit()
	// sandboxInit calls syscall.Exec which replaces this process image.
	// If we reach here, exec failed — exit rather than run as a server.
	fmt.Fprintln(os.Stderr, "conduit: sandbox init: exec failed, aborting")
	os.Exit(1)
}

// sandboxInit is called by init() in the re-exec'd child. It:
//  1. Reads the target command from the environment.
//  2. Applies PR_SET_NO_NEW_PRIVS (prerequisite for unprivileged seccomp).
//  3. Installs the BPF seccomp denylist.
//  4. Calls syscall.Exec to replace this process with the shell.
//
// If any step fails we call os.Exit(1) — we must not let a misconfigured
// sandbox proceed, and we cannot propagate the error back to the parent
// since we're in a separate process.
func sandboxInit() {
	// Read and decode the target command.
	cmdJSON := os.Getenv(sandboxEnvCmd)
	if cmdJSON == "" {
		fmt.Fprintln(os.Stderr, "conduit: sandbox: missing _CONDUIT_SANDBOX_CMD")
		os.Exit(1)
	}
	var command []string
	if err := json.Unmarshal([]byte(cmdJSON), &command); err != nil || len(command) == 0 {
		fmt.Fprintln(os.Stderr, "conduit: sandbox: invalid _CONDUIT_SANDBOX_CMD:", err)
		os.Exit(1)
	}

	// Apply PR_SET_NO_NEW_PRIVS — prevents privilege escalation via setuid
	// binaries and is required to install a seccomp filter without CAP_SYS_ADMIN.
	if err := applyNoNewPrivs(); err != nil {
		fmt.Fprintln(os.Stderr, "conduit: sandbox: no_new_privs:", err)
		os.Exit(1)
	}

	// Install the BPF seccomp denylist. From this point on, any call to a
	// denied syscall will kill the process immediately.
	if err := applySeccomp(); err != nil {
		fmt.Fprintln(os.Stderr, "conduit: sandbox: seccomp:", err)
		os.Exit(1)
	}

	// Build the shell's environment: inherit everything EXCEPT our internal
	// _CONDUIT_* control variables. The shell doesn't need to know how it
	// was launched.
	shellEnv := filterSandboxEnv(os.Environ())

	// syscall.Exec replaces the current process image with the shell.
	// The PTY slave file descriptors (stdin/stdout/stderr) are inherited
	// across exec, so the shell's I/O is already wired to the PTY.
	// Namespace memberships (PID, mount, UTS, network) are also preserved.
	if err := syscall.Exec(command[0], command, shellEnv); err != nil {
		fmt.Fprintln(os.Stderr, "conduit: sandbox: exec:", err)
		os.Exit(1)
	}
}

// filterSandboxEnv removes _CONDUIT_* variables from the environment before
// passing it to the shell. The shell doesn't need to know about our internal
// re-exec mechanism.
func filterSandboxEnv(env []string) []string {
	filtered := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, "_CONDUIT_") {
			filtered = append(filtered, e)
		}
	}
	return filtered
}
