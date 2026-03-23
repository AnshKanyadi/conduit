// Package pty manages pseudo-terminal (PTY) attachment for isolated processes.
// See doc.go for the full design rationale.
package pty

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	creackpty "github.com/creack/pty"
)

// Config describes how to spawn an isolated terminal process.
type Config struct {
	// Command is the program and arguments to run inside the PTY.
	// Example: []string{"/bin/sh"} or []string{"/bin/bash", "--login"}
	Command []string

	// Env is the environment for the shell process. If empty, a safe
	// minimal environment is used. These vars are passed THROUGH the
	// sandbox init to the final shell — they are not visible to the
	// server process after fork.
	Env []string

	// Rows and Cols set the initial terminal window size.
	// SIGWINCH is sent to the process when Resize() is called later.
	Rows uint16
	Cols uint16
}

// Process represents a running shell attached to a PTY master.
// Read/Write on Process talk directly to the PTY master — reads return
// shell output (bytes the shell wrote to its stdout), writes send input
// (bytes delivered to the shell as if typed on a keyboard).
//
// The zero value is not valid; always construct via Spawn().
type Process struct {
	ptmx   *os.File   // PTY master — our side of the terminal pair
	cmd    *exec.Cmd  // the sandbox init (or shell on Darwin)
	waitCh chan error  // receives the exit error exactly once, then closes
}

// Read reads raw PTY output from the shell.
// Satisfies io.Reader — safe to pass directly to a WebSocket writer.
func (p *Process) Read(buf []byte) (int, error) {
	return p.ptmx.Read(buf)
}

// Write sends raw bytes to the shell as keyboard input.
// Satisfies io.Writer — safe to pass raw WebSocket payloads here.
func (p *Process) Write(buf []byte) (int, error) {
	return p.ptmx.Write(buf)
}

// Wait returns a channel that receives the process exit error exactly once.
// The channel is closed after the value is sent, so multiple receivers are safe.
// A nil error means the shell exited with status 0.
func (p *Process) Wait() <-chan error {
	return p.waitCh
}

// Resize updates the terminal window size and delivers SIGWINCH to the process.
// Terminal applications (vim, htop, etc.) listen for SIGWINCH to reflow their
// output to the new dimensions.
func (p *Process) Resize(rows, cols uint16) error {
	return creackpty.Setsize(p.ptmx, &creackpty.Winsize{
		Rows: rows,
		Cols: cols,
	})
}

// Close terminates the process gracefully: SIGTERM first, then SIGKILL after
// a 5-second grace period. It always closes the PTY master.
//
// Why this order?
// SIGTERM lets the shell run its EXIT trap (cleanup scripts, history sync).
// SIGKILL is the guarantee — it cannot be caught or ignored, so we never
// leak a zombie. We call cmd.Wait() indirectly via the waitCh goroutine
// that was started in Spawn, so the kernel process table entry is always
// reaped regardless of which signal terminates the shell.
func (p *Process) Close() error {
	if p.cmd.Process != nil {
		// Polite shutdown — give the shell a chance to clean up.
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
	}

	select {
	case <-p.waitCh:
		// Shell exited cleanly within the grace period.
	case <-time.After(5 * time.Second):
		// Grace period expired — force kill.
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		// Still must drain waitCh so the goroutine exits and the process
		// table entry is reaped.
		<-p.waitCh
	}

	return p.ptmx.Close()
}

// startWithPTY opens a PTY pair, wires it to cmd's stdio, augments (not
// replaces) cmd.SysProcAttr, and calls cmd.Start().
//
// Why not use creackpty.StartWithSize?
// That function replaces cmd.SysProcAttr entirely, which discards our
// Cloneflags (the Linux namespace settings). We need to augment the existing
// SysProcAttr rather than replace it, so we open the PTY manually.
func startWithPTY(cmd *exec.Cmd, rows, cols uint16) (*os.File, error) {
	// Open the PTY pair. ptmx is the master (our side), tty is the slave
	// (the child's side — it becomes the shell's controlling terminal).
	ptmx, tty, err := creackpty.Open()
	if err != nil {
		return nil, fmt.Errorf("open pty pair: %w", err)
	}

	// tty is only needed until cmd.Start() — the child inherits it via its
	// stdio file descriptors. We close our copy in the parent after Start().
	defer tty.Close()

	// Set the initial window size before the shell starts so it doesn't
	// receive an initial SIGWINCH event.
	if err = creackpty.Setsize(ptmx, &creackpty.Winsize{Rows: rows, Cols: cols}); err != nil {
		_ = ptmx.Close()
		return nil, fmt.Errorf("set pty size: %w", err)
	}

	// Wire the PTY slave as the command's stdio. In the child process, after
	// exec, these become fd 0 (stdin), fd 1 (stdout), fd 2 (stderr).
	cmd.Stdin = tty
	cmd.Stdout = tty
	cmd.Stderr = tty

	// Augment SysProcAttr — add the PTY-specific fields without touching
	// whatever the platform-specific Spawn already set (e.g. Cloneflags).
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// Setsid: create a new session. Required so the slave PTY can become the
	// child's controlling terminal (a controlling terminal can only be set on
	// a session leader).
	cmd.SysProcAttr.Setsid = true
	// Setctty + Ctty: set the controlling terminal to fd 0 (stdin = tty slave).
	cmd.SysProcAttr.Setctty = true
	cmd.SysProcAttr.Ctty = 0

	if err = cmd.Start(); err != nil {
		_ = ptmx.Close()
		return nil, fmt.Errorf("start process: %w", err)
	}

	return ptmx, nil
}

// reapProcess waits for cmd to exit, sends the result to waitCh, then closes
// it. Started as a goroutine immediately after cmd.Start() so that the process
// is always reaped and never becomes a zombie, regardless of how the caller
// uses the Process handle.
//
// What is a zombie process?
// When a process exits, the kernel keeps its exit status in the process table
// until the parent calls wait(2). If the parent never calls wait, that entry
// stays forever — a zombie. In a long-running server spawning many sessions,
// zombie accumulation can exhaust the PID namespace.
func reapProcess(cmd *exec.Cmd, waitCh chan<- error, ptmx *os.File) {
	err := cmd.Wait()
	waitCh <- err
	close(waitCh)

	// cmd.Wait() guarantees the process has exited, but the PTY master might
	// still buffer unread data. We don't close ptmx here — Close() does it
	// explicitly after the caller has drained the buffer.
	_ = ptmx // suppress unused warning — ptmx lifetime managed by Close()
}

// Stderr satisfies io.Writer so Process can be used where a writer is needed
// (e.g. passing to observability.Logger). Writes go to the PTY master which
// delivers them to the shell's stdin — this is intentional for injection.
var _ io.ReadWriter = (*Process)(nil)
