package lock

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func TestAcquireRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")

	l, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("lock file not created: %v", err)
	}

	l.Release()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("lock file not removed after Release, err=%v", err)
	}
}

func TestAcquireHeldByLiveProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")

	// A real long-lived process (ourselves via a subprocess is overkill;
	// os.Getpid() of the test binary itself is alive and not us).
	cmd := exec.Command(os.Args[0], "-test.run=NONE")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	defer cmd.Process.Kill()
	defer cmd.Wait()

	if err := os.WriteFile(path, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	if _, err := Acquire(path); err != ErrHeld {
		t.Fatalf("Acquire = %v, want ErrHeld", err)
	}
}

func TestAcquireReclaimsDeadPID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")

	cmd := exec.Command(os.Args[0], "-test.run=NONE")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	pid := cmd.Process.Pid
	cmd.Wait() // let it exit so the PID is dead

	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	l, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire should reclaim stale lock, got: %v", err)
	}
	l.Release()
}

func TestAcquireReclaimsEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")

	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	l, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire should reclaim empty lock file, got: %v", err)
	}
	l.Release()
}
