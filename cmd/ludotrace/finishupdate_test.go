package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// launchRecord captures what runFinishUpdate asked to be started, so a test
// can assert *which* binary is running when it returns.
type launchRecord struct {
	path string
	args []string
}

// newSwapFixture writes a stand-in "new binary" and "installed binary" into a
// temp dir and returns deps wired to record launches instead of performing
// them. The caller overrides individual seams as the case needs.
func newSwapFixture(t *testing.T) (swapDeps, *[]launchRecord) {
	t.Helper()

	dir := t.TempDir()
	self := filepath.Join(dir, "pending_update.exe")
	original := filepath.Join(dir, "ludotrace.exe")
	if err := os.WriteFile(self, []byte("new binary"), 0o755); err != nil {
		t.Fatalf("write self: %v", err)
	}
	if err := os.WriteFile(original, []byte("old binary"), 0o755); err != nil {
		t.Fatalf("write original: %v", err)
	}

	var launches []launchRecord
	return swapDeps{
		self:          self,
		originalPath:  original,
		remainingArgs: []string{"--autostart"},
		launch: func(path string, args []string) error {
			launches = append(launches, launchRecord{path: path, args: args})
			return nil
		},
		sleep: func(time.Duration) {},
	}, &launches
}

// The regression this issue is about: when the swap cannot be completed, the
// old binary must be running when the process exits. Before the fix this path
// removed the temp file and exited 1 with nothing left running at all.
func TestRunFinishUpdate_RenameAlwaysFails_RelaunchesOriginal(t *testing.T) {
	d, launches := newSwapFixture(t)
	attempts := 0
	d.rename = func(string, string) error {
		attempts++
		return errors.New("sharing violation")
	}

	if code := runFinishUpdate(d); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if attempts != renameAttempts {
		t.Errorf("rename attempts = %d, want %d", attempts, renameAttempts)
	}
	if len(*launches) != 1 {
		t.Fatalf("launches = %v, want exactly one (the original)", *launches)
	}
	if got := (*launches)[0].path; got != d.originalPath {
		t.Errorf("relaunched %q, want the original %q", got, d.originalPath)
	}
	if got := (*launches)[0].args; len(got) != 1 || got[0] != "--autostart" {
		t.Errorf("relaunch args = %v, want the original launch args", got)
	}

	// The old binary must be untouched, and no temp left behind.
	body, err := os.ReadFile(d.originalPath)
	if err != nil {
		t.Fatalf("read original: %v", err)
	}
	if string(body) != "old binary" {
		t.Errorf("original content = %q, want it unmodified", body)
	}
	entries, err := filepath.Glob(filepath.Join(filepath.Dir(d.originalPath), "ludotrace-install-*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("temp files left behind: %v", entries)
	}
}

// The Windows lock clears within moments of the parent exiting, so a rename
// that fails once must not be treated as a failed update.
func TestRunFinishUpdate_RenameSucceedsOnRetry(t *testing.T) {
	d, launches := newSwapFixture(t)
	attempts := 0
	slept := 0
	d.sleep = func(time.Duration) { slept++ }
	d.rename = func(oldpath, newpath string) error {
		attempts++
		if attempts < 3 {
			return errors.New("sharing violation")
		}
		return os.Rename(oldpath, newpath)
	}

	if code := runFinishUpdate(d); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if attempts != 3 {
		t.Errorf("rename attempts = %d, want 3", attempts)
	}
	if slept != 2 {
		t.Errorf("backoff sleeps = %d, want 2", slept)
	}
	body, err := os.ReadFile(d.originalPath)
	if err != nil {
		t.Fatalf("read original: %v", err)
	}
	if string(body) != "new binary" {
		t.Errorf("install path content = %q, want the new binary", body)
	}
	if len(*launches) != 1 || (*launches)[0].path != d.originalPath {
		t.Errorf("launches = %v, want the install path started once", *launches)
	}
}

// Staging failures land in the same place as rename failures: something must
// still be running afterwards.
func TestRunFinishUpdate_StagingFails_RelaunchesOriginal(t *testing.T) {
	d, launches := newSwapFixture(t)
	d.self = filepath.Join(filepath.Dir(d.self), "does-not-exist.exe")
	d.rename = func(string, string) error {
		t.Error("rename attempted after staging failed")
		return nil
	}

	if code := runFinishUpdate(d); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if len(*launches) != 1 || (*launches)[0].path != d.originalPath {
		t.Errorf("launches = %v, want the original relaunched", *launches)
	}
}

// Nothing left to try — but it must still be reported as a failure rather
// than exiting 0 as though the update had worked.
func TestRunFinishUpdate_RelaunchAlsoFails(t *testing.T) {
	d, _ := newSwapFixture(t)
	d.rename = func(string, string) error { return errors.New("sharing violation") }
	d.launch = func(string, []string) error { return errors.New("access denied") }

	if code := runFinishUpdate(d); code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

// A parent that never goes away must not strand the update: the swap is still
// attempted once the wait times out.
func TestRunFinishUpdate_ParentWaitTimesOut_StillSwaps(t *testing.T) {
	d, _ := newSwapFixture(t)
	d.parentPID = 4242
	waited := false
	d.waitForExit = func(pid int, timeout time.Duration) bool {
		waited = true
		if pid != 4242 {
			t.Errorf("waited on pid %d, want 4242", pid)
		}
		return false
	}
	d.rename = os.Rename

	if code := runFinishUpdate(d); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !waited {
		t.Error("parent PID was passed but never waited on")
	}
}

func TestParseParentPID(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantPID int
		wantRem []string
	}{
		{
			name:    "flag present",
			args:    []string{parentPIDFlag, "1234", "--autostart"},
			wantPID: 1234,
			wantRem: []string{"--autostart"},
		},
		{
			// A pending binary is launched by the *previous* version, so the
			// old positional-only form has to keep working.
			name:    "legacy parent passes no flag",
			args:    []string{"--autostart"},
			wantPID: 0,
			wantRem: []string{"--autostart"},
		},
		{
			name:    "no args at all",
			args:    nil,
			wantPID: 0,
			wantRem: nil,
		},
		{
			name:    "unparseable pid is dropped, not relaunched as an arg",
			args:    []string{parentPIDFlag, "not-a-number", "--autostart"},
			wantPID: 0,
			wantRem: []string{"--autostart"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pid, rem := parseParentPID(tt.args)
			if pid != tt.wantPID {
				t.Errorf("pid = %d, want %d", pid, tt.wantPID)
			}
			if len(rem) != len(tt.wantRem) {
				t.Fatalf("remaining = %v, want %v", rem, tt.wantRem)
			}
			for i := range rem {
				if rem[i] != tt.wantRem[i] {
					t.Fatalf("remaining = %v, want %v", rem, tt.wantRem)
				}
			}
		})
	}
}
