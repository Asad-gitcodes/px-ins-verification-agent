package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func main() {
	pid := flag.Int("pid", 0, "PID of the running agent process")
	target := flag.String("target", "", "Path to the installed agent executable")
	source := flag.String("source", "", "Path to the staged replacement agent executable")
	backup := flag.String("backup", "", "Path for the previous agent executable backup")
	restart := flag.String("restart", "", "Path to restart after replacement")
	argsEncoded := flag.String("args", "", "Base64 JSON array of restart args")
	timeout := flag.Duration("timeout", 60*time.Second, "Maximum time to wait for target exe to unlock")
	flag.Parse()

	if *target == "" || *source == "" || *backup == "" || *restart == "" {
		log.Fatalf("target, source, backup, and restart are required")
	}
	configureLogging(filepath.Dir(*target))
	log.Printf("updater started pid=%d target=%s source=%s", *pid, *target, *source)

	args, err := decodeArgs(*argsEncoded)
	if err != nil {
		log.Fatalf("decode restart args: %v", err)
	}
	if err := waitForProcessExit(*pid, *timeout); err != nil {
		log.Fatalf("wait for agent exit: %v", err)
	}
	if err := replaceExecutable(*target, *source, *backup, *timeout); err != nil {
		log.Fatalf("replace executable: %v", err)
	}
	if err := restartAgent(*restart, args); err != nil {
		log.Fatalf("restart agent: %v", err)
	}
	log.Printf("updater finished")
}

func configureLogging(dir string) {
	path := filepath.Join(dir, "agent-updater.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("open updater log failed: %v", err)
		return
	}
	log.SetOutput(f)
	log.SetFlags(log.Ldate | log.Ltime)
}

func decodeArgs(encoded string) ([]string, error) {
	if encoded == "" {
		return nil, nil
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	var args []string
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	return args, nil
}

func replaceExecutable(target, source, backup string, timeout time.Duration) error {
	if _, err := os.Stat(source); err != nil {
		return fmt.Errorf("staged executable unavailable: %w", err)
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		_ = os.Remove(backup)
		err := os.Rename(target, backup)
		if err == nil {
			log.Printf("moved current executable to backup=%s", backup)
			if err := os.Rename(source, target); err != nil {
				_ = os.Rename(backup, target)
				return fmt.Errorf("move staged executable into place: %w", err)
			}
			log.Printf("installed new executable target=%s", target)
			return nil
		}
		lastErr = err
		log.Printf("target still locked, retrying: %v", err)
		time.Sleep(time.Second)
	}
	return fmt.Errorf("timed out waiting for target to unlock: %w", lastErr)
}

func waitForProcessExit(pid int, timeout time.Duration) error {
	if pid <= 0 {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		running, err := processRunning(pid)
		if err != nil {
			log.Printf("process check failed pid=%d: %v", pid, err)
		}
		if !running {
			log.Printf("agent process exited pid=%d", pid)
			return nil
		}
		log.Printf("waiting for agent process to exit pid=%d", pid)
		time.Sleep(time.Second)
	}
	return fmt.Errorf("timed out waiting for pid=%d to exit", pid)
}

func processRunning(pid int) (bool, error) {
	if runtime.GOOS == "windows" {
		cmd := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH")
		out, err := cmd.Output()
		if err != nil {
			return false, err
		}
		text := strings.TrimSpace(string(out))
		if text == "" || strings.Contains(strings.ToLower(text), "no tasks are running") {
			return false, nil
		}
		return strings.Contains(text, fmt.Sprintf("\"%d\"", pid)), nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false, nil
	}
	err = proc.Signal(os.Signal(nil))
	if err == nil {
		return true, nil
	}
	if err == os.ErrProcessDone || err == io.EOF {
		return false, nil
	}
	return false, nil
}

func restartAgent(path string, args []string) error {
	cmd := exec.Command(path, args...)
	cmd.Dir = filepath.Dir(path)
	if err := cmd.Start(); err != nil {
		return err
	}
	log.Printf("restarted agent pid=%d path=%s", cmd.Process.Pid, path)
	return nil
}
