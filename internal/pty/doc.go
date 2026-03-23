// Package pty manages pseudo-terminal (PTY) attachment for isolated processes.
//
// A PTY is a kernel-provided pair of file descriptors (master/slave) that
// emulates a hardware terminal. The slave end is given to the child process
// as its stdin/stdout/stderr; the master end is read/written by Conduit to
// pipe bytes to and from connected clients.
//
// Processes are spawned inside Linux namespaces (PID, mount, UTS, network)
// rather than via the Docker API so that Conduit has precise control over
// isolation, resource limits, and teardown — without a daemon dependency.
package pty
