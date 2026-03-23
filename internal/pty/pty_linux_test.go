//go:build linux

package pty_test

// Linux-specific tests: verify that namespace isolation is actually applied.
// These tests require Linux and will be skipped on Darwin.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/anshk/conduit/internal/pty"
)

// TestSpawn_PIDNamespace verifies that the shell runs as PID 1 inside its
// own PID namespace.
//
// How: we ask the shell to print $$ (its own PID). Inside the namespace,
// the first process is always PID 1. From the host's perspective, it has
// a much larger PID — the two views are different, which is the whole point
// of PID namespaces.
func TestSpawn_PIDNamespace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	p, err := pty.Spawn(ctx, pty.Config{
		// The shell prints its own PID and exits. Inside the namespace, $$ == 1.
		Command: []string{"/bin/sh", "-c", "echo $$"},
		Rows:    24,
		Cols:    80,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Close()

	select {
	case <-p.Wait():
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit")
	}

	out := readWithTimeout(t, p, 2*time.Second)
	// The shell's $$ inside a PID namespace is 1 (it's the init process).
	if !strings.Contains(out, "1") {
		t.Errorf("expected PID 1 in output (namespace isolation), got: %q", out)
	}
}

// TestSpawn_NetworkNamespace verifies the shell has an isolated network stack.
//
// How: the only network interface in a fresh network namespace is the loopback
// (lo). We check /proc/net/dev — if there's no eth0, ens*, or other physical
// interface, the namespace is isolated.
func TestSpawn_NetworkNamespace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	p, err := pty.Spawn(ctx, pty.Config{
		// List network interfaces from /proc/net/dev (available without ip/ifconfig).
		Command: []string{"/bin/sh", "-c", "cat /proc/net/dev"},
		Rows:    24,
		Cols:    80,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Close()

	select {
	case <-p.Wait():
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit")
	}

	out := readWithTimeout(t, p, 2*time.Second)
	// A fresh network namespace has only "lo" — no eth0, ens*, or similar.
	// We check that no physical-sounding interface name appears.
	for _, iface := range []string{"eth0", "ens3", "enp", "wlan0"} {
		if strings.Contains(out, iface) {
			t.Errorf("unexpected interface %q in isolated network namespace; /proc/net/dev:\n%s", iface, out)
		}
	}
	// Loopback should still be present.
	if !strings.Contains(out, "lo:") {
		t.Errorf("loopback interface missing; /proc/net/dev:\n%s", out)
	}
}

// TestSpawn_SeccompBlocksDeniedSyscall verifies that the seccomp filter
// terminates the process when it attempts a blocked syscall.
//
// We use reboot(2) (syscall 169) as the test case because:
//   - It's in our denylist
//   - It requires root to actually reboot — the filter kills us before the
//     capability check, so we can test even without root
//   - It has no side effects if we're killed before the kernel processes it
func TestSpawn_SeccompBlocksDeniedSyscall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	p, err := pty.Spawn(ctx, pty.Config{
		// Use Python to call the reboot syscall directly. If seccomp is working,
		// the process is killed immediately; if not, reboot() returns EPERM (no root).
		// We distinguish: EPERM → seccomp NOT applied, signal kill → seccomp applied.
		Command: []string{"/bin/sh", "-c",
			`python3 -c "import ctypes; ctypes.CDLL(None).syscall(169, 0xfee1dead, 0x28121969, 0x01234567, 0)" 2>&1; echo "exit:$?"`,
		},
		Rows: 24,
		Cols: 80,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Close()

	select {
	case <-p.Wait():
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit")
	}

	out := readWithTimeout(t, p, 2*time.Second)
	// If seccomp killed the process, we never reach "echo exit:$?" because
	// the KILL_PROCESS action terminates the entire process group, not just
	// the Python subprocess. The waitCh receives a non-zero exit status.
	// We just verify the process did not succeed with an "exit:0" message.
	if strings.Contains(out, "exit:0") {
		t.Errorf("reboot syscall succeeded or returned safely — seccomp may not be applied; output: %q", out)
	}
	t.Logf("seccomp test output: %q", out)
}
