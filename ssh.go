package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

// connectSSH connects to a pod via SSH, attaches to the tmux claude session,
// and returns the exit reason ("detached" or error).
func connectSSH(podIP string, cfg Config) (detached bool, err error) {
	keyData, err := os.ReadFile(cfg.SSHKeyPath)
	if err != nil {
		return false, fmt.Errorf("read SSH key %s: %w", cfg.SSHKeyPath, err)
	}

	signer, err := ssh.ParsePrivateKey(keyData)
	if err != nil {
		return false, fmt.Errorf("parse SSH key: %w", err)
	}

	client, err := ssh.Dial("tcp", net.JoinHostPort(podIP, "22"), &ssh.ClientConfig{
		User:            cfg.SSHUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		return false, fmt.Errorf("SSH dial %s: %w", podIP, err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return false, fmt.Errorf("SSH session: %w", err)
	}
	defer session.Close()

	// Get terminal size
	fd := int(os.Stdin.Fd())
	width, height, err := term.GetSize(fd)
	if err != nil {
		width, height = 80, 24
	}

	// Pass through local TERM so tmux can match terminal capabilities.
	// Ghostty uses xterm-ghostty; falling back to xterm-256color if unset.
	termEnv := os.Getenv("TERM")
	if termEnv == "" {
		termEnv = "xterm-256color"
	}

	// Request PTY
	if err := session.RequestPty(termEnv, height, width, ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}); err != nil {
		return false, fmt.Errorf("request PTY: %w", err)
	}

	// Set terminal to raw mode
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return false, fmt.Errorf("raw mode: %w", err)
	}
	defer term.Restore(fd, oldState)

	// Handle window resize
	sigWinch := make(chan os.Signal, 1)
	signal.Notify(sigWinch, syscall.SIGWINCH)
	go func() {
		for range sigWinch {
			if w, h, err := term.GetSize(fd); err == nil {
				_ = session.WindowChange(h, w)
			}
		}
	}()
	defer signal.Stop(sigWinch)

	// Wire up I/O with clipboard bridge
	stdinPipe, err := session.StdinPipe()
	if err != nil {
		return false, fmt.Errorf("stdin pipe: %w", err)
	}

	session.Stdout = os.Stdout
	session.Stderr = os.Stderr

	// Start clipboard bridge — wraps stdin to intercept paste events
	bridge := NewClipboardBridge(client)
	bridgedStdin := bridge.WrapStdin(os.Stdin)

	// Start the remote command — wait for tmux session to exist, then attach
	cmd := `for i in $(seq 1 30); do tmux has-session -t claude 2>/dev/null && break; sleep 1; done; exec tmux -u attach-session -t claude`
	if err := session.Start(cmd); err != nil {
		return false, fmt.Errorf("start tmux: %w", err)
	}

	// Intercept Ctrl+C — detach from tmux instead of killing it
	sigInt := make(chan os.Signal, 1)
	signal.Notify(sigInt, syscall.SIGINT, syscall.SIGTERM)
	detachRequested := false
	go func() {
		<-sigInt
		detachRequested = true
		// Send tmux detach key sequence (Ctrl+B d)
		_, _ = stdinPipe.Write([]byte{0x02}) // Ctrl+B
		_, _ = stdinPipe.Write([]byte("d"))  // d
	}()
	defer signal.Stop(sigInt)

	// Copy stdin → remote in background
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(stdinPipe, bridgedStdin)
		stdinPipe.Close()
	}()

	// Wait for session to end
	err = session.Wait()
	wg.Wait()

	if detachRequested {
		return true, nil
	}

	if err != nil {
		if exitErr, ok := err.(*ssh.ExitError); ok && exitErr.ExitStatus() == 0 {
			return true, nil // tmux detach
		}
		return false, err
	}
	return false, nil
}
