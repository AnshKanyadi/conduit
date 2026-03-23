//go:build linux

package pty

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Seccomp BPF constants.
// We define these locally rather than relying on golang.org/x/sys/unix because
// their availability varies across package versions, and having them explicit
// makes the security-critical code self-contained and auditable.
const (
	// seccompRetAllow tells the kernel to allow the syscall and return normally.
	seccompRetAllow = 0x7fff0000

	// seccompRetKillProcess terminates the ENTIRE PROCESS GROUP (not just the
	// thread that made the call). We use KILL_PROCESS over KILL_THREAD because
	// a multi-threaded process could otherwise survive by having other threads
	// continue running after one thread is killed.
	seccompRetKillProcess = 0x80000000

	// auditArchX86_64 is the architecture identifier from <linux/audit.h>.
	// It encodes: EM_X86_64 (0x3E) | AUDIT_ARCH_64BIT (0x80000000) | AUDIT_ARCH_LE (0x40000000)
	// The seccomp BPF preamble validates this before checking any syscall numbers,
	// so a 32-bit process can't bypass 64-bit filters by switching modes.
	auditArchX86_64 = 0xC000003E

	// seccompSetModeFilter is the operation code for SECCOMP_SET_MODE_FILTER,
	// passed as the first argument to the seccomp(2) syscall.
	seccompSetModeFilter = 1

	// seccompFilterFlagTsync syncs the filter to all threads in the process.
	// Without it, only the calling thread gets the filter. Since we apply it
	// in the re-exec'd child before exec'ing the shell, there is only one
	// thread — but we set it for correctness if that ever changes.
	seccompFilterFlagTsync = 1
)

// deniedSyscalls is the list of Linux x86-64 syscall numbers we block.
//
// Design: denylist over allowlist.
// An allowlist is more restrictive but requires listing every syscall a shell
// and its children might ever make (~100+). A denylist blocks the highest-risk
// operations while allowing normal shell execution. This mirrors Docker's
// default seccomp profile approach.
//
// What we're blocking and why:
var deniedSyscalls = []uint32{
	155, // pivot_root      — change the root filesystem; used in container escapes
	159, // adjtimex        — adjust the kernel's hardware clock; host-wide effect
	164, // settimeofday    — set system time; host-wide effect
	165, // mount           — mount filesystems; even in a mount namespace, risky
	166, // umount2         — unmount filesystems
	167, // swapon          — enable a swap device; host-wide resource effect
	168, // swapoff         — disable a swap device
	169, // reboot          — reboot or halt the system
	175, // init_module     — load a kernel module from a byte slice; full kernel access
	176, // delete_module   — remove a kernel module
	246, // kexec_load      — load a new kernel image; complete system takeover
	313, // finit_module    — load a kernel module from a file descriptor
	317, // seccomp         — prevent the shell from installing its own seccomp rules
	//                        (it can't escalate, but defense-in-depth)
	320, // kexec_file_load — fd-based kexec_load variant
	321, // bpf             — load arbitrary eBPF programs; multiple CVEs in this path
}

// applyNoNewPrivs calls prctl(PR_SET_NO_NEW_PRIVS, 1) on the current process.
//
// This is a mandatory prerequisite for non-root seccomp filter installation.
// Without it, an unprivileged process needs CAP_SYS_ADMIN to install a
// seccomp filter. With PR_SET_NO_NEW_PRIVS set, execve() cannot grant new
// privileges (setuid bits are ignored), and seccomp filters can be installed
// without any capability.
//
// It is NOT reversible — once set, it applies to all child processes too.
func applyNoNewPrivs() error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("seccomp: PR_SET_NO_NEW_PRIVS: %w", err)
	}
	return nil
}

// applySeccomp installs the BPF seccomp filter on the current process.
// Must be called after applyNoNewPrivs().
//
// How BPF seccomp filters work:
// The kernel runs a small BPF program for every syscall the process makes.
// The program inspects the syscall number and returns an action code:
//   - SECCOMP_RET_ALLOW  (0x7fff0000): let the syscall proceed normally
//   - SECCOMP_RET_KILL_PROCESS (0x80000000): terminate the process immediately
//
// Our filter structure (one BPF instruction = {Code, Jt, Jf, K}):
//
//  [0] BPF_LD|BPF_W|BPF_ABS  offset=4   — load arch field from seccomp_data
//  [1] BPF_JMP|BPF_JEQ|BPF_K k=ARCH     — if arch==x86_64: skip 1 → [3]
//  [2] BPF_RET|BPF_K          k=KILL     — wrong arch: kill
//  [3] BPF_LD|BPF_W|BPF_ABS  offset=0   — load syscall number
//  [4] BPF_JMP|BPF_JEQ|BPF_K k=denied[0] jt=0 jf=1 — match → [5] kill; no match → [6]
//  [5] BPF_RET|BPF_K          k=KILL
//  [6] BPF_JMP|BPF_JEQ|BPF_K k=denied[1] ... (repeat for each denied syscall)
//  ... BPF_RET|BPF_K          k=ALLOW    — default: allow
func applySeccomp() error {
	filter := buildFilter()

	fprog := unix.SockFprog{
		Len:    uint16(len(filter)),
		Filter: &filter[0],
	}

	// Call seccomp(SECCOMP_SET_MODE_FILTER, SECCOMP_FILTER_FLAG_TSYNC, &fprog).
	// We use RawSyscall rather than unix.Prctl(PR_SET_SECCOMP) because the
	// seccomp(2) syscall provides TSYNC which prctl does not.
	_, _, errno := unix.RawSyscall(
		unix.SYS_SECCOMP,
		seccompSetModeFilter,
		seccompFilterFlagTsync,
		uintptr(unsafe.Pointer(&fprog)),
	)
	if errno != 0 {
		return fmt.Errorf("seccomp: install filter: %w", errno)
	}
	return nil
}

// buildFilter constructs the BPF instruction slice for our seccomp filter.
// Returns a slice of unix.SockFilter (each is a 64-bit BPF instruction).
func buildFilter() []unix.SockFilter {
	// BPF helper constructors. These mirror the macros in <linux/filter.h>.
	stmt := func(code uint16, k uint32) unix.SockFilter {
		return unix.SockFilter{Code: code, K: k}
	}
	jump := func(code uint16, k uint32, jt, jf uint8) unix.SockFilter {
		return unix.SockFilter{Code: code, K: k, Jt: jt, Jf: jf}
	}

	insns := []unix.SockFilter{
		// Validate architecture first. The seccomp_data struct layout is:
		//   offset 0: syscall number (uint32)
		//   offset 4: architecture  (uint32)
		// We check architecture before the syscall number so a 32-bit
		// process using int 0x80 cannot spoof a 64-bit syscall number.
		stmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, 4), // load arch
		jump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K,
			auditArchX86_64,
			1, // jt: arch matches → skip the KILL, fall through to load syscall#
			0, // jf: arch wrong → fall to KILL
		),
		stmt(unix.BPF_RET|unix.BPF_K, seccompRetKillProcess), // wrong arch: kill
		stmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, 0),        // load syscall number
	}

	// For each denied syscall, append two instructions:
	//   JEQ k=N jt=0 jf=1: if nr==N → fall to RET KILL; else → skip RET KILL
	//   RET KILL
	for _, nr := range deniedSyscalls {
		insns = append(insns,
			jump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, nr, 0, 1),
			stmt(unix.BPF_RET|unix.BPF_K, seccompRetKillProcess),
		)
	}

	// Default action: allow. This instruction is reached by any syscall that
	// didn't match any entry in the deny list.
	insns = append(insns, stmt(unix.BPF_RET|unix.BPF_K, seccompRetAllow))

	return insns
}
