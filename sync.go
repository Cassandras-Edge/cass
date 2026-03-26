package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// syncLabel returns a stable mutagen label for a session.
func syncLabel(sessionID string) string {
	return "cass-" + short(sessionID)
}

// startSync creates a mutagen sync session between ~/workspace/<short-id> and
// the pod's /workspace. Uses SSH via the pod IP with the configured key.
// Returns the local workspace path.
func startSync(podIP string, sessionID string, cfg Config) (string, error) {
	localDir := filepath.Join(os.Getenv("HOME"), "workspace", short(sessionID))
	if err := os.MkdirAll(localDir, 0755); err != nil {
		return "", fmt.Errorf("create local workspace: %w", err)
	}

	label := syncLabel(sessionID)
	remote := fmt.Sprintf("%s@%s:/workspace", cfg.SSHUser, podIP)

	// Terminate any existing sync with this label (idempotent)
	_ = exec.Command("mutagen", "sync", "terminate", "--label-selector", "cass="+label).Run()

	args := []string{
		"sync", "create",
		localDir, remote,
		"--label", "cass=" + label,
		"--ignore-vcs",
		"--ignore", "node_modules",
		"--ignore", ".git",
		"--ignore", "__pycache__",
		"--mode", "two-way-resolved",
	}

	cmd := exec.Command("mutagen", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("mutagen sync create: %w", err)
	}

	return localDir, nil
}

// stopSync terminates the mutagen sync session for the given session ID.
func stopSync(sessionID string) {
	label := syncLabel(sessionID)
	cmd := exec.Command("mutagen", "sync", "terminate", "--label-selector", "cass="+label)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
}

// flushSync waits for any pending sync operations to complete.
func flushSync(sessionID string) {
	label := syncLabel(sessionID)
	cmd := exec.Command("mutagen", "sync", "flush", "--label-selector", "cass="+label)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
}

// hasMutagen checks if the mutagen binary is available.
func hasMutagen() bool {
	_, err := exec.LookPath("mutagen")
	return err == nil
}

// isSyncRunning checks if a sync session exists for the given session ID.
func isSyncRunning(sessionID string) bool {
	label := syncLabel(sessionID)
	out, err := exec.Command("mutagen", "sync", "list", "--label-selector", "cass="+label).Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), label)
}
