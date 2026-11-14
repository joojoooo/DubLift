package dublift

import (
	"errors"
	"os"
	"testing"
)

func TestKillProcessInvalid(t *testing.T) {
	if err := killProcess(nil); err == nil {
		t.Fatal("killProcess(nil) expected error, got nil")
	}
	pZero := &os.Process{Pid: 0}
	if err := killProcess(pZero); err == nil {
		t.Fatal("killProcess(Pid: 0) expected error, got nil")
	}
	pNeg := &os.Process{Pid: -1}
	if err := killProcess(pNeg); err == nil {
		t.Fatal("killProcess(Pid: -1) expected error, got nil")
	}
}

func TestKillProcessAlreadyExited(t *testing.T) {
	p := &os.Process{Pid: 9999999}
	err := killProcess(p)
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("unexpected error for non-existent process: %v", err)
	}
}
