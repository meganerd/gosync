package deploy

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// The fleet hit this on mi-hci01-1010: the receiver log lived at the shared
// path /tmp/gosync-server.log, the remote shell could not create it, and the
// deployment reported the unrelated error
// "fetch remote certificate failed: ssh: exit status 1" because the failing
// redirect was a background job whose status the remote shell discarded.

func newTestDeployer(t *testing.T, transport string) *Deployer {
	t.Helper()
	d := NewDeployer("source", "host:/data/shared/Backup", "")
	if err := d.parseDestination(); err != nil {
		t.Fatal(err)
	}
	d.SetServerPort(51785)
	if err := d.SetTransport(transport); err != nil {
		t.Fatal(err)
	}
	return d
}

// writeReceiverStub installs an executable ~/gosync stub so remoteServerCommand
// can be run locally under sh. body is the stub's shell script.
func writeReceiverStub(t *testing.T, home, body string) {
	t.Helper()
	stub := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(home, "gosync"), []byte(stub), 0o755); err != nil {
		t.Fatalf("write receiver stub: %v", err)
	}
}

// deploymentShells is every shell the remote command must work under: POSIX sh
// is the floor, and bash is the login shell on the fleet.
func deploymentShells(t *testing.T) []string {
	t.Helper()
	shells := []string{"sh"}
	if _, err := exec.LookPath("bash"); err == nil {
		shells = append(shells, "bash")
	}
	return shells
}

// runRemoteServerCommand executes the generated remote command under the given
// shell with an isolated HOME, returning merged stdout and stderr.
func runRemoteServerCommand(t *testing.T, d *Deployer, home, shell string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("nohup"); err != nil {
		t.Skip("nohup not available")
	}
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("remote start command reads /proc; linux only")
	}
	cmd := exec.Command(shell, "-c", d.remoteServerCommand())
	env := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "HOME=") {
			env = append(env, e)
		}
	}
	cmd.Env = append(env, "HOME="+home)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// homeLogFile is where the receiver log must live: beside the pid marker, in
// the remote user's own home, never in a path shared with other users.
func homeLogFile(home string) string {
	return filepath.Join(home, ".gosync-server.log")
}

func TestRemoteServerCommandLogsUnderRemoteHome(t *testing.T) {
	for _, transport := range []string{"quic", "tcp", "server"} {
		t.Run(transport, func(t *testing.T) {
			d := newTestDeployer(t, transport)
			command := d.remoteServerCommand()
			t.Logf("remote command: %s", command)
			if strings.Contains(command, "/tmp/") {
				t.Fatalf("remote command writes to a shared path: %q", command)
			}
			if !strings.Contains(command, `.gosync-server.log`) {
				t.Fatalf("remote command has no receiver log: %q", command)
			}
			if !strings.Contains(command, "--base '/data/shared/Backup'") {
				t.Fatalf("remote command lost its base directory: %q", command)
			}
		})
	}
}

func TestRemoteServerCommandFailsWhenLogUnwritable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block func(t *testing.T, home string)
	}{
		{"log-path-is-a-directory", func(t *testing.T, home string) {
			if err := os.Mkdir(homeLogFile(home), 0o755); err != nil {
				t.Fatalf("block log path: %v", err)
			}
		}},
		{"log-path-has-no-permission", func(t *testing.T, home string) {
			if err := os.Mkdir(homeLogFile(home), 0o000); err != nil {
				t.Fatalf("block log path: %v", err)
			}
		}},
		{"home-has-no-write-permission", func(t *testing.T, home string) {
			if err := os.Chmod(home, 0o555); err != nil {
				t.Fatalf("make home read-only: %v", err)
			}
			t.Cleanup(func() { _ = os.Chmod(home, 0o755) })
		}},
	} {
		for _, transport := range []string{"quic", "tcp"} {
			for _, shell := range deploymentShells(t) {
				t.Run(tc.name+"/"+transport+"/"+shell, func(t *testing.T) {
					if os.Geteuid() == 0 {
						t.Skip("permission bits are not enforced for root")
					}
					home := t.TempDir()
					writeReceiverStub(t, home, "echo serving; exec sleep 60")
					tc.block(t, home)

					output, err := runRemoteServerCommand(t, newTestDeployer(t, transport), home, shell)
					if err == nil {
						t.Fatalf("deployment accepted an unwritable receiver log: output=%q", output)
					}
					if !strings.Contains(output, "gosync: remote log") {
						t.Fatalf("no deployment error naming the receiver log: output=%q", output)
					}
					if !strings.Contains(output, ".gosync-server.log") {
						t.Fatalf("deployment error omitted the receiver log path: output=%q", output)
					}
				})
			}
		}
	}
}

func TestRemoteServerCommandReportsReceiverStartupFailure(t *testing.T) {
	for _, shell := range deploymentShells(t) {
		t.Run(shell, func(t *testing.T) {
			home := t.TempDir()
			writeReceiverStub(t, home, "echo 'bind: address already in use' >&2; exit 1")

			output, err := runRemoteServerCommand(t, newTestDeployer(t, "quic"), home, shell)
			if err == nil {
				t.Fatalf("deployment accepted a receiver that exited immediately: output=%q", output)
			}
			if !strings.Contains(output, "failed to start") {
				t.Fatalf("no startup failure reported: output=%q", output)
			}
			if !strings.Contains(output, "address already in use") {
				t.Fatalf("receiver log not surfaced in the deployment error: output=%q", output)
			}
			if _, err := os.Stat(filepath.Join(home, ".gosync.pid")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("startup failure wrote a pid marker (stat err: %v)", err)
			}
		})
	}
}

func TestRemoteServerCommandStartsReceiverAndWritesMarker(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("cleanup test relies on /proc starttime matching")
	}
	for _, shell := range deploymentShells(t) {
		t.Run(shell, func(t *testing.T) {
			home := t.TempDir()
			writeReceiverStub(t, home, "echo serving; exec sleep 60")
			d := newTestDeployer(t, "quic")

			output, err := runRemoteServerCommand(t, d, home, shell)
			if err != nil {
				t.Fatalf("receiver start failed: %v\ncommand=%q\noutput=%s", err, d.remoteServerCommand(), output)
			}

			logData, err := os.ReadFile(homeLogFile(home))
			if err != nil {
				t.Fatalf("receiver log: %v", err)
			}
			if !strings.Contains(string(logData), "serving") {
				t.Fatalf("receiver log = %q, want the receiver startup line", logData)
			}

			markerData, err := os.ReadFile(filepath.Join(home, ".gosync.pid"))
			if err != nil {
				t.Fatalf("pid marker: %v", err)
			}
			fields := strings.Fields(string(markerData))
			if len(fields) != 2 {
				t.Fatalf("pid marker = %q, want \"<pid> <starttime>\"", markerData)
			}
			pid, err := strconv.Atoi(fields[0])
			if err != nil {
				t.Fatalf("pid marker = %q: %v", markerData, err)
			}
			if fields[1] != procStartTime(t, pid) {
				t.Fatalf("pid marker starttime = %s, want %s", fields[1], procStartTime(t, pid))
			}

			// The deployed receiver must be removable by the existing cleanup script.
			runCleanupScript(t, home)
			if err := syscall.Kill(pid, 0); err == nil {
				t.Fatalf("cleanup left receiver %d running", pid)
			}
			if _, err := os.Stat(homeLogFile(home)); err != nil {
				t.Fatalf("receiver log removed by cleanup: %v", err)
			}
		})
	}
}
