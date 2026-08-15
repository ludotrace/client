package proc

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestWaitForExit_LiveProcessTimesOut(t *testing.T) {
	start := time.Now()
	if WaitForExit(os.Getpid(), 150*time.Millisecond) {
		t.Error("reported this very process as exited")
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Errorf("returned after %v, want it to honour the full timeout", elapsed)
	}
}

func TestWaitForExit_ExitedProcess(t *testing.T) {
	// A reaped child is the case that matters: on Unix the PID is gone from
	// the process table only after Wait, and before that it is a zombie that
	// signal 0 still reports as live.
	cmd := exec.Command(os.Args[0], "-test.run=TestWaitForExit_ExitedProcess", "-test.list=nothing")
	cmd.Env = append(os.Environ(), "GO_PROC_TEST_CHILD=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child exited with error: %v", err)
	}

	if !WaitForExit(pid, 5*time.Second) {
		t.Errorf("pid %d has been reaped but is still reported as running", pid)
	}
}

func TestWaitForExit_InvalidPID(t *testing.T) {
	if WaitForExit(0, time.Second) {
		t.Error("pid 0 should not be reported as an exited process")
	}
	if WaitForExit(-1, time.Second) {
		t.Error("negative pid should not be reported as an exited process")
	}
}
