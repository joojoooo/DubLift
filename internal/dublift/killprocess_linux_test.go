//go:build linux

package dublift

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

const seccompChildEnv = "DUBLIFT_TEST_SECCOMP_CHILD"

// seccomp(2) syscall numbers. pidfd_send_signal is 424 on all of them.
var seccompSyscall = map[string]uintptr{"amd64": 317, "arm64": 277, "arm": 383, "386": 354}

// TestRunOutputInputCancelUnderPidfdSeccomp reproduces the Android app
// sandbox seen on Fire OS 7: Go has decided that pidfds work, but seccomp
// traps pidfd_send_signal. Cancelling an FFmpeg job must not use that
// syscall, otherwise SIGSYS terminates the whole server.
func TestRunOutputInputCancelUnderPidfdSeccomp(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not found")
	}
	nr, ok := seccompSyscall[runtime.GOARCH]
	if !ok {
		t.Skipf("no seccomp syscall number for %s", runtime.GOARCH)
	}
	if os.Getenv(seccompChildEnv) != "1" {
		// The filter cannot be removed and a regression kills the process,
		// so run the scenario in a child test binary.
		cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
		cmd.Env = append(os.Environ(), seccompChildEnv+"=1")
		out, err := cmd.CombinedOutput()
		if strings.Contains(string(out), "--- SKIP") {
			t.Skipf("child skipped:\n%s", out)
		}
		if err != nil {
			t.Fatalf("cancelling a job under the seccomp filter killed the process: %v\n%s", err, out)
		}
		return
	}

	// Let Go probe pidfd support before the filter exists. On Android the
	// probe ignores SIGSYS and can report pidfds as usable anyway.
	if err := exec.Command(sleep, "0").Run(); err != nil {
		t.Fatal(err)
	}
	if err := trapPidfdSendSignal(nr); err != nil {
		t.Skipf("cannot install seccomp filter: %v", err)
	}
	e := &Engine{slots: make(chan struct{}, 1)}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = e.runOutput(ctx, sleep, []string{"30"}, io.Discard)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runOutput error = %v, want %v", err, context.DeadlineExceeded)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("job stopped after %v, want prompt termination on cancellation", elapsed)
	}
}

// trapPidfdSendSignal makes pidfd_send_signal raise SIGSYS in every thread,
// like the app seccomp policy of affected Android builds.
func trapPidfdSendSignal(seccompNr uintptr) error {
	const (
		pidfdSendSignal = 424
		retTrap         = 0x00030000 // SECCOMP_RET_TRAP
		retAllow        = 0x7fff0000 // SECCOMP_RET_ALLOW
		prSetNoNewPrivs = 38
		setModeFilter   = 1 // SECCOMP_SET_MODE_FILTER
		filterTsync     = 1 // SECCOMP_FILTER_FLAG_TSYNC
	)
	filter := []syscall.SockFilter{
		{Code: syscall.BPF_LD | syscall.BPF_W | syscall.BPF_ABS, K: 0}, // seccomp_data.nr
		{Code: syscall.BPF_JMP | syscall.BPF_JEQ | syscall.BPF_K, Jt: 0, Jf: 1, K: pidfdSendSignal},
		{Code: syscall.BPF_RET | syscall.BPF_K, K: retTrap},
		{Code: syscall.BPF_RET | syscall.BPF_K, K: retAllow},
	}
	prog := syscall.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	// no_new_privs is per thread; keep both calls on the same one.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0); errno != 0 {
		return errno
	}
	r, _, errno := syscall.RawSyscall(seccompNr, setModeFilter, filterTsync, uintptr(unsafe.Pointer(&prog)))
	if errno != 0 {
		return errno
	}
	if r != 0 {
		return fmt.Errorf("thread %d could not be synchronized", r)
	}
	return nil
}
