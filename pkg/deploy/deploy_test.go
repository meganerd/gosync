package deploy

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// runCleanupScript executes remoteCleanupScript under sh with an isolated HOME.
// Every branch of the script exits 0, so any non-zero exit fails the test.
func runCleanupScript(t *testing.T, home string) {
	t.Helper()
	cmd := exec.Command("sh", "-c", remoteCleanupScript)
	env := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "HOME=") {
			env = append(env, e)
		}
	}
	cmd.Env = append(env, "HOME="+home)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cleanup script exited non-zero: %v\n%s", err, out)
	}
}

// The child is owned by the test: t.Cleanup kills and reaps it.
func startSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

// procStartTime returns field 22 of /proc/<pid>/stat (starttime in clock ticks).
func procStartTime(t *testing.T, pid int) string {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatalf("read /proc/%d/stat: %v", pid, err)
	}
	fields := strings.Fields(string(data))
	if len(fields) <= 21 {
		t.Fatalf("/proc/%d/stat has %d fields, want at least 22", pid, len(fields))
	}
	return fields[21]
}

// writePidMarker writes "<pid> <start>\n" (or just "<pid>\n" when start is
// empty) into $HOME/.gosync.pid.
func writePidMarker(t *testing.T, home, pid, start string) {
	t.Helper()
	content := pid
	if start != "" {
		content += " " + start
	}
	content += "\n"
	if err := os.WriteFile(filepath.Join(home, ".gosync.pid"), []byte(content), 0o600); err != nil {
		t.Fatalf("write pid marker: %v", err)
	}
}

func assertMarker(t *testing.T, home, want string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".gosync.pid"))
	if err != nil {
		t.Fatalf("marker: %v", err)
	}
	if string(data) != want {
		t.Fatalf("marker content = %q, want %q", data, want)
	}
}

func assertNoMarker(t *testing.T, home string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(home, ".gosync.pid")); !os.IsNotExist(err) {
		t.Fatalf("marker still present (stat err: %v)", err)
	}
}

func assertNoBinary(t *testing.T, binary string) {
	t.Helper()
	if _, err := os.Stat(binary); !os.IsNotExist(err) {
		t.Fatalf("%s still present (stat err: %v)", binary, err)
	}
}

// kill(pid, 0) succeeds on zombies, so the process must be reaped first.
func assertGone(t *testing.T, pid int) {
	t.Helper()
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("process %d still alive", pid)
	}
}

func TestRemoteCleanupScript(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("remote cleanup script uses /proc; linux only")
	}
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("/proc not available")
	}

	t.Run("no-broad-kill", func(t *testing.T) {
		for _, banned := range []string{"pkill", "killall", "pgrep"} {
			if strings.Contains(remoteCleanupScript, banned) {
				t.Fatalf("cleanup script must not use %q", banned)
			}
		}
	})

	t.Run("no-marker-is-noop", func(t *testing.T) {
		home := t.TempDir()
		runCleanupScript(t, home)
		assertNoMarker(t, home)
	})

	t.Run("non-numeric-pid-is-noop", func(t *testing.T) {
		home := t.TempDir()
		writePidMarker(t, home, "abc", "123")
		runCleanupScript(t, home)
		assertMarker(t, home, "abc 123\n")
	})

	t.Run("pid-one-is-noop", func(t *testing.T) {
		home := t.TempDir()
		writePidMarker(t, home, "1", "0")
		runCleanupScript(t, home)
		assertMarker(t, home, "1 0\n")
	})

	t.Run("empty-start-removes-marker-only", func(t *testing.T) {
		home := t.TempDir()
		writePidMarker(t, home, "123", "")
		runCleanupScript(t, home)
		assertNoMarker(t, home)
	})

	t.Run("dead-pid-removes-marker-only", func(t *testing.T) {
		home := t.TempDir()
		c := exec.Command("sh", "-c", "exit 0")
		if err := c.Start(); err != nil {
			t.Fatalf("start child: %v", err)
		}
		deadPID := c.Process.Pid
		if err := c.Wait(); err != nil {
			t.Fatalf("reap child: %v", err)
		}
		writePidMarker(t, home, strconv.Itoa(deadPID), "0")
		runCleanupScript(t, home)
		assertNoMarker(t, home)
	})

	t.Run("start-mismatch-leaves-process-and-marker", func(t *testing.T) {
		home := t.TempDir()
		cmd := startSleeper(t)
		pid := cmd.Process.Pid
		writePidMarker(t, home, strconv.Itoa(pid), "0") // starttime is never 0, so this is a mismatch
		runCleanupScript(t, home)
		assertMarker(t, home, strconv.Itoa(pid)+" 0\n")
		if err := syscall.Kill(pid, 0); err != nil {
			t.Fatalf("process %d should still be alive: %v", pid, err)
		}
	})

	t.Run("owned-pid-kills-and-cleans", func(t *testing.T) {
		home := t.TempDir()
		cmd := startSleeper(t)
		pid := cmd.Process.Pid
		start := procStartTime(t, pid)

		binary := filepath.Join(home, "gosync")
		if err := os.WriteFile(binary, []byte("placeholder"), 0o755); err != nil {
			t.Fatalf("write placeholder binary: %v", err)
		}
		writePidMarker(t, home, strconv.Itoa(pid), start)

		runCleanupScript(t, home)

		// The script SIGTERMs the process; the zombie survives until reaped,
		// so the script's /proc poll runs its full ~5s budget here.
		if err := cmd.Wait(); err == nil {
			t.Fatal("sleeper should have been terminated, not exited cleanly")
		}
		assertGone(t, pid)
		assertNoMarker(t, home)
		assertNoBinary(t, binary)
	})
}
