package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// daemonPaths returns the pid/log file paths for a named daemon
// ("watch" or "serve").
func daemonPaths(name string) (pidFile, logFile string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	dir := filepath.Join(home, ".config", "prreview")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	return filepath.Join(dir, name+".pid"), filepath.Join(dir, name+".log"), nil
}

// daemonPID returns the running daemon's PID, or 0 if not running.
func daemonPID(name string) (int, string, error) {
	pidFile, logFile, err := daemonPaths(name)
	if err != nil {
		return 0, "", err
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, logFile, nil
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0, logFile, nil
	}
	// liveness probe
	if err := syscall.Kill(pid, 0); err != nil {
		_ = os.Remove(pidFile)
		return 0, logFile, nil
	}
	return pid, logFile, nil
}

// startDaemon re-execs this binary detached, running the hidden childCmd with
// args, output appended to the daemon's log file. Returns the child PID.
func startDaemon(name, childCmd string, args []string) (int, error) {
	pid, logFile, err := daemonPID(name)
	if err != nil {
		return 0, err
	}
	if pid != 0 {
		return 0, fmt.Errorf("%s daemon already running (pid %d) — prreview %s stop", name, pid, name)
	}
	pidFile, _, err := daemonPaths(name)
	if err != nil {
		return 0, err
	}

	self, err := os.Executable()
	if err != nil {
		return 0, err
	}
	logw, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer logw.Close()

	cmd := exec.Command(self, append([]string{childCmd}, args...)...)
	cmd.Stdout = logw
	cmd.Stderr = logw
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach from terminal
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid = cmd.Process.Pid
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		return 0, err
	}
	_ = cmd.Process.Release()
	return pid, nil
}

func printDaemonHints(name string, pid int) {
	_, logFile, _ := daemonPaths(name)
	fmt.Printf("%s daemon started (pid %d)\n", name, pid)
	fmt.Printf("  logs:   %s   (prreview %s logs -f)\n", logFile, name)
	fmt.Printf("  stop:   prreview %s stop\n", name)
	fmt.Printf("  status: prreview %s status\n", name)
}

func stopDaemon(name string) error {
	pid, _, err := daemonPID(name)
	if err != nil {
		return err
	}
	if pid == 0 {
		fmt.Printf("%s daemon is not running\n", name)
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}
	pidFile, _, _ := daemonPaths(name)
	_ = os.Remove(pidFile)
	fmt.Printf("sent SIGTERM to %s daemon (pid %d)\n", name, pid)
	return nil
}

func daemonStatus(name string) error {
	pid, logFile, err := daemonPID(name)
	if err != nil {
		return err
	}
	if pid == 0 {
		fmt.Printf("%s daemon: not running\n", name)
		return nil
	}
	fmt.Printf("%s daemon: running (pid %d)\n", name, pid)
	fmt.Printf("logs: %s\n", logFile)
	return nil
}

func daemonLogs(name string, follow bool) error {
	_, logFile, err := daemonPID(name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(logFile); err != nil {
		fmt.Println("no logs yet")
		return nil
	}
	args := []string{"-n", "50"}
	if follow {
		args = []string{"-n", "50", "-f"}
	}
	cmd := exec.Command("tail", append(args, logFile)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}
