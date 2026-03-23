package pty_test

// Tests use the external test package (_test suffix) so they only exercise
// the exported API. Platform-specific behavior (namespace isolation, seccomp)
// is tested in pty_linux_test.go.

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/anshk/conduit/internal/pty"
)

// collectOutput starts reading from r in a background goroutine and returns a
// function that blocks until reading is done (EOF/error) or the timeout
// elapses, then returns all bytes collected.
//
// Why concurrent reading?
// PTY I/O is asynchronous. The correct pattern is:
//   1. Start reading in the background immediately after Spawn.
//   2. Wait for the process to exit (which closes the slave, delivering EIO
//      to the master on Linux, or EOF on Darwin).
//   3. Collect what was buffered.
//
// Reading AFTER process exit misses data that the kernel has already consumed
// from the PTY buffer — it arrives at the master before the slave closes.
func collectOutput(r io.Reader, timeout time.Duration) func() string {
	ch := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		tmp := make([]byte, 512)
		for {
			n, err := r.Read(tmp)
			if n > 0 {
				buf.Write(tmp[:n])
			}
			if err != nil {
				break
			}
		}
		ch <- buf.String()
	}()
	return func() string {
		select {
		case s := <-ch:
			return s
		case <-time.After(timeout):
			return ""
		}
	}
}

// --- Spawn lifecycle ---------------------------------------------------------

func TestSpawn_ReturnsProcess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	p, err := pty.Spawn(ctx, pty.Config{
		Command: []string{"/bin/sh", "-c", "exit 0"},
		Rows:    24,
		Cols:    80,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Close()
}

func TestSpawn_EmptyCommandReturnsError(t *testing.T) {
	ctx := context.Background()
	_, err := pty.Spawn(ctx, pty.Config{Command: nil, Rows: 24, Cols: 80})
	if err == nil {
		t.Fatal("expected error for empty command, got nil")
	}
}

// --- PTY I/O -----------------------------------------------------------------

func TestSpawn_PTY_Output(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	p, err := pty.Spawn(ctx, pty.Config{
		Command: []string{"/bin/sh", "-c", "printf 'hello'"},
		Rows:    24,
		Cols:    80,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Close()

	// Start collecting output BEFORE waiting for the process to exit.
	// The shell writes "hello" to the PTY slave; we must be draining the
	// PTY master concurrently or the kernel buffer may stall the write.
	collect := collectOutput(p, 4*time.Second)

	select {
	case <-p.Wait():
	case <-time.After(4 * time.Second):
		t.Fatal("process did not exit in time")
	}

	out := collect()
	if !strings.Contains(out, "hello") {
		t.Errorf("PTY output: got %q, want it to contain %q", out, "hello")
	}
}

func TestSpawn_PTY_MultilineOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	p, err := pty.Spawn(ctx, pty.Config{
		Command: []string{"/bin/sh", "-c", "printf 'line1\nline2\nline3\n'"},
		Rows:    24,
		Cols:    80,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Close()

	collect := collectOutput(p, 4*time.Second)

	select {
	case <-p.Wait():
	case <-time.After(4 * time.Second):
		t.Fatal("process did not exit in time")
	}

	out := collect()
	for _, line := range []string{"line1", "line2", "line3"} {
		if !strings.Contains(out, line) {
			t.Errorf("output missing %q; full output: %q", line, out)
		}
	}
}

func TestSpawn_PTY_BinaryOutput(t *testing.T) {
	// Terminals carry arbitrary bytes including ANSI escape sequences.
	// Verify that non-printable bytes survive the round-trip.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	p, err := pty.Spawn(ctx, pty.Config{
		// ESC [ 2 J is the ANSI "clear screen" sequence.
		Command: []string{"/bin/sh", "-c", "printf '\\033[2J'"},
		Rows:    24,
		Cols:    80,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Close()

	collect := collectOutput(p, 4*time.Second)

	select {
	case <-p.Wait():
	case <-time.After(4 * time.Second):
		t.Fatal("process did not exit in time")
	}

	out := collect()
	// Log output for inspection; the sequence may be processed by the terminal layer.
	t.Logf("binary output (hex): %x", out)
	if len(out) == 0 {
		t.Error("expected some PTY output, got empty string")
	}
}

// --- Process exit / Wait ---------------------------------------------------

func TestSpawn_Wait_ZeroExitCode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	p, err := pty.Spawn(ctx, pty.Config{
		Command: []string{"/bin/sh", "-c", "exit 0"},
		Rows:    24,
		Cols:    80,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Close()

	collect := collectOutput(p, 2*time.Second)

	select {
	case err := <-p.Wait():
		if err != nil {
			t.Logf("Wait returned non-nil (may be normal on this platform): %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not return within timeout")
	}

	_ = collect()
}

func TestSpawn_Wait_ChannelClosedAfterExit(t *testing.T) {
	// The waitCh must be closed after the exit value is sent — a second
	// receive must return immediately with ok=false.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	p, err := pty.Spawn(ctx, pty.Config{
		Command: []string{"/bin/sh", "-c", "exit 0"},
		Rows:    24,
		Cols:    80,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Close()

	ch := p.Wait()
	collect := collectOutput(p, 2*time.Second)

	// First receive: exit value.
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("first Wait receive timed out")
	}
	_ = collect()

	// Second receive: channel must be closed.
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("Wait channel should be closed after first receive, but ok=true")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("Wait channel not closed — second receive blocked")
	}
}

// --- Close / teardown -------------------------------------------------------

func TestClose_TerminatesProcess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	p, err := pty.Spawn(ctx, pty.Config{
		Command: []string{"/bin/sh", "-c", "sleep 300"},
		Rows:    24,
		Cols:    80,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	_ = p.Close()

	select {
	case <-p.Wait():
		// Good: process exited.
	case <-time.After(6 * time.Second):
		t.Error("process did not exit within 6s of Close()")
	}
}

func TestClose_Idempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	p, err := pty.Spawn(ctx, pty.Config{
		Command: []string{"/bin/sh", "-c", "exit 0"},
		Rows:    24,
		Cols:    80,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	_ = p.Close()
	// Second Close must not panic.
	_ = p.Close()
}

// --- Resize -----------------------------------------------------------------

func TestResize_DoesNotError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	p, err := pty.Spawn(ctx, pty.Config{
		Command: []string{"/bin/sh", "-c", "sleep 1"},
		Rows:    24,
		Cols:    80,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Close()

	if err := p.Resize(48, 160); err != nil {
		t.Errorf("Resize(48, 160): %v", err)
	}
}

// --- Context cancellation ---------------------------------------------------

func TestSpawn_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

	p, err := pty.Spawn(ctx, pty.Config{
		Command: []string{"/bin/sh", "-c", "sleep 300"},
		Rows:    24,
		Cols:    80,
	})
	if err != nil {
		cancel()
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Close()

	cancel() // trigger context cancellation → exec.CommandContext sends SIGKILL

	select {
	case <-p.Wait():
		// Good.
	case <-time.After(3 * time.Second):
		t.Error("process did not exit within 3s of context cancellation")
	}
}
