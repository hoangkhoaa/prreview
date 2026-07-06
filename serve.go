package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"prreview/internal/config"
	"prreview/internal/server"
)

// runPurge asks a running serve instance to clear its queue.
func runPurge(args []string) error {
	port := 8787
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port", "-p":
			i++
			if i >= len(args) {
				return fmt.Errorf("--port needs a value")
			}
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 1 || n > 65535 {
				return fmt.Errorf("--port must be 1-65535")
			}
			port = n
		default:
			return fmt.Errorf("unknown purge flag: %s", args[i])
		}
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(fmt.Sprintf("http://127.0.0.1:%d/api/purge", port), "application/json", nil)
	if err != nil {
		return fmt.Errorf("no prreview serve running on port %d (start it with: prreview serve)", port)
	}
	defer resp.Body.Close()
	var out struct {
		Removed int `json:"removed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	fmt.Printf("purged %d job(s) — running & awaiting reviews were kept\n", out.Removed)
	return nil
}

func runServe(args []string) error {
	// daemon management subcommands
	if len(args) > 0 {
		switch args[0] {
		case "stop":
			return stopDaemon("serve")
		case "status":
			return daemonStatus("serve")
		case "logs":
			follow := len(args) > 1 && (args[1] == "-f" || args[1] == "--follow")
			return daemonLogs("serve", follow)
		}
	}
	return runServeMain(args, false)
}

func runServeMain(args []string, isChild bool) error {
	port := 8787
	interval := 15 * time.Second
	concurrency := 2
	autoApprove := false
	noOpen := false
	detach := false

	args, modelName, err := extractModelFlag(args)
	if err != nil {
		return err
	}
	rawArgs := args
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port", "-p":
			i++
			if i >= len(args) {
				return fmt.Errorf("--port needs a value")
			}
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 1 || n > 65535 {
				return fmt.Errorf("--port must be 1-65535")
			}
			port = n
		case "--interval":
			i++
			if i >= len(args) {
				return fmt.Errorf("--interval needs a value (seconds)")
			}
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 5 {
				return fmt.Errorf("--interval must be an integer >= 5 (seconds)")
			}
			interval = time.Duration(n) * time.Second
		case "--concurrency":
			i++
			if i >= len(args) {
				return fmt.Errorf("--concurrency needs a value")
			}
			n, err := strconv.Atoi(args[i])
			if err != nil || n < 1 || n > 8 {
				return fmt.Errorf("--concurrency must be 1-8")
			}
			concurrency = n
		case "--auto-approve":
			autoApprove = true
		case "--no-open":
			noOpen = true
		case "--detach", "-d":
			detach = true
		default:
			return fmt.Errorf("unknown serve flag: %s", args[i])
		}
	}

	if detach && !isChild {
		var childArgs []string
		for _, a := range rawArgs {
			if a != "--detach" && a != "-d" {
				childArgs = append(childArgs, a)
			}
		}
		childArgs = append(childArgs, "--no-open")
		if modelName != "" {
			childArgs = append(childArgs, "--model", modelName)
		}
		pid, err := startDaemon("serve", "__serve-run", childArgs)
		if err != nil {
			return err
		}
		printDaemonHints("serve", pid)
		url := fmt.Sprintf("http://127.0.0.1:%d", port)
		fmt.Printf("  url:    %s\n", url)
		if !noOpen {
			time.Sleep(400 * time.Millisecond)
			_ = exec.Command("open", url).Start()
		}
		return nil
	}

	base, err := config.Load()
	if err != nil {
		return err
	}
	if modelName != "" {
		base.DefaultModelName = modelName // session-only override
	}

	engine, err := server.NewEngine(base, interval, concurrency)
	if err != nil {
		return err
	}

	// Startup safety: the engine must never come up in a state that can post
	// to GitHub on its own. Watch is forced OFF (enable it in the UI when
	// ready), the queue starts empty, and a stray watch daemon from a
	// previous session is terminated.
	engine.SetWatch(false, autoApprove)
	engine.Purge()
	if pid, _, _ := daemonPID("watch"); pid != 0 {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		if pidFile, _, err := daemonPaths("watch"); err == nil {
			_ = os.Remove(pidFile)
		}
		fmt.Printf("note: stopped leftover watch daemon (pid %d) — nothing auto-posts until you enable watch in the UI\n", pid)
	}
	fmt.Println("startup state: watch OFF · queue empty · nothing will be posted without you")

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	url := "http://" + addr
	fmt.Printf("prreview web UI: %s  (Ctrl+C to stop)\n", url)
	if !noOpen {
		go func() {
			time.Sleep(300 * time.Millisecond)
			_ = exec.Command("open", url).Start()
		}()
	}
	return server.Serve(addr, engine, hardShutdown)
}

// hardShutdown is the kill switch: it also terminates the watch daemon (if
// any), cleans up this process's pid file when detached, and exits.
func hardShutdown() {
	if pid, _, _ := daemonPID("watch"); pid != 0 {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		if pidFile, _, err := daemonPaths("watch"); err == nil {
			_ = os.Remove(pidFile)
		}
	}
	if pidFile, _, err := daemonPaths("serve"); err == nil {
		_ = os.Remove(pidFile)
	}
	fmt.Println("kill switch pressed — shutting down")
	os.Exit(0)
}
