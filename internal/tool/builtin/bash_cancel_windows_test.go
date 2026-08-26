//go:build windows

package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"reasonix/internal/sandbox"
)

type windowsBashRunResult struct {
	out string
	err error
}

func TestBashCancelKillsWindowsChildProcessTree(t *testing.T) {
	powershell, err := exec.LookPath("powershell")
	if err != nil {
		t.Skip("powershell not found")
	}
	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "child.pid")
	quotedPIDFile := strings.ReplaceAll(pidFile, "'", "''")
	command := fmt.Sprintf(
		"$p = Start-Process -FilePath powershell -ArgumentList '-NoProfile','-NonInteractive','-Command','Start-Sleep -Seconds 120' -PassThru; "+
			"Set-Content -LiteralPath '%s' -Value $p.Id; "+
			"Start-Sleep -Seconds 120",
		quotedPIDFile,
	)
	args, _ := json.Marshal(map[string]any{"command": command})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan windowsBashRunResult, 1)
	go func() {
		out, runErr := (bash{
			shell: sandbox.Shell{Kind: sandbox.ShellPowerShell, Path: powershell},
		}).Execute(ctx, args)
		done <- windowsBashRunResult{out: out, err: runErr}
	}()

	childPID := waitForWindowsPIDFile(t, pidFile, done)
	cancel()
	select {
	case result := <-done:
		err = result.err
	case <-time.After(40 * time.Second):
		killWindowsPID(childPID)
		t.Fatal("cancel did not interrupt bash within 40s")
	}
	if err == nil {
		t.Fatal("expected cancel to return an error")
	}
	for range 50 {
		if !windowsProcessAlive(childPID) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	killWindowsPID(childPID)
	t.Fatalf("child process %d survived bash cancel", childPID)
}

func TestBashWindowsReapsChildAfterForegroundShellExit(t *testing.T) {
	powershell, err := exec.LookPath("powershell")
	if err != nil {
		t.Skip("powershell not found")
	}
	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "child.pid")
	quotedPIDFile := strings.ReplaceAll(pidFile, "'", "''")
	command := fmt.Sprintf(
		"$p = Start-Process -FilePath powershell -ArgumentList '-NoProfile','-NonInteractive','-Command','Start-Sleep -Seconds 120' -PassThru; "+
			"Set-Content -LiteralPath '%s' -Value $p.Id",
		quotedPIDFile,
	)
	args, _ := json.Marshal(map[string]any{"command": command})

	out, err := (bash{
		shell: sandbox.Shell{Kind: sandbox.ShellPowerShell, Path: powershell},
	}).Execute(context.Background(), args)
	childPID := waitForWindowsPIDFile(t, pidFile, nil)
	if err != nil {
		killWindowsPID(childPID)
		t.Fatalf("foreground command failed: %v (out=%q)", err, out)
	}
	for range 50 {
		if !windowsProcessAlive(childPID) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	killWindowsPID(childPID)
	t.Fatalf("child process %d survived foreground bash cleanup", childPID)
}

func TestBashCancelKillsGitBashHereDocPython(t *testing.T) {
	sh := sandbox.ResolveShell("bash", "", nil)
	if sh.Kind != sandbox.ShellBash || sh.Path == "" {
		t.Skip("Git Bash not found")
	}
	python := gitBashPython(t, sh.Path)
	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "python.pid")
	pythonPIDFile := filepath.ToSlash(pidFile)
	command := fmt.Sprintf("%s - <<'PYEOF'\nimport os, time\nwith open(%q, 'w') as f:\n    f.write(str(os.getpid()))\n    f.flush()\ntime.sleep(120)\nPYEOF\n", shellQuote(python), pythonPIDFile)
	args, _ := json.Marshal(map[string]any{"command": command})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan windowsBashRunResult, 1)
	go func() {
		out, runErr := (bash{shell: sh}).Execute(ctx, args)
		done <- windowsBashRunResult{out: out, err: runErr}
	}()

	childPID := waitForWindowsPIDFile(t, pidFile, done)
	time.Sleep(300 * time.Millisecond) // let the tracker observe the Git Bash child tree.
	cancel()
	select {
	case result := <-done:
		if result.err == nil {
			t.Fatal("expected cancel to return an error")
		}
	case <-time.After(20 * time.Second):
		killWindowsPID(childPID)
		t.Fatal("cancel did not interrupt Git Bash here-doc python within 20s")
	}
	for range 50 {
		if !windowsProcessAlive(childPID) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	killWindowsPID(childPID)
	t.Fatalf("Git Bash here-doc python process %d survived bash cancel", childPID)
}

func waitForWindowsPIDFile(t *testing.T, path string, done chan windowsBashRunResult) int {
	t.Helper()
	const startupTimeout = 30 * time.Second
	deadline := time.NewTimer(startupTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr == nil && pid > 0 {
				return pid
			}
		}
		select {
		case result := <-done:
			// The shell can finish just before this select while its final file
			// write becomes visible. Check once more before reporting the early
			// exit, then preserve output so CI distinguishes a launch failure from
			// a slow hosted runner.
			data, readErr := os.ReadFile(path)
			if readErr == nil {
				pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
				if parseErr == nil && pid > 0 {
					done <- result
					return pid
				}
			}
			t.Fatalf("bash exited before writing child pid file %s: err=%v out=%q", path, result.err, result.out)
		case <-deadline.C:
			t.Fatalf("timed out after %s waiting for child pid file %s", startupTimeout, path)
		case <-ticker.C:
		}
	}
}

func gitBashPython(t *testing.T, bashPath string) string {
	t.Helper()
	for _, name := range []string{"python3", "python"} {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		python := gitBashPath(t, bashPath, path)
		out, err := exec.Command(bashPath, "-lc", fmt.Sprintf("%s - <<'PYEOF'\nprint('ok')\nPYEOF\n", shellQuote(python))).CombinedOutput()
		if err == nil {
			return python
		}
		t.Logf("python candidate %s is not usable from Git Bash: %v: %s", python, err, strings.TrimSpace(string(out)))
	}
	t.Skip("python not found or not usable from Git Bash")
	return ""
}

func gitBashPath(t *testing.T, bashPath, path string) string {
	t.Helper()
	out, err := exec.Command(bashPath, "-lc", fmt.Sprintf("cygpath -u %s", shellQuote(path))).Output()
	if err == nil {
		converted := strings.TrimSpace(string(out))
		if converted != "" {
			return strings.Split(converted, "\n")[0]
		}
	}
	return filepath.ToSlash(path)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func windowsProcessAlive(pid int) bool {
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", fmt.Sprintf("if (Get-Process -Id %d -ErrorAction SilentlyContinue) { exit 0 } else { exit 1 }", pid))
	return cmd.Run() == nil
}

func killWindowsPID(pid int) {
	_ = exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid)).Run()
}
