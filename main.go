package main

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
)

func main() {
	urlFlag := flag.String("url", "", "Runner orchestrator URL")
	keyFlag := flag.String("key", "", "API key")
	modelFlag := flag.String("model", "", "Model (haiku, sonnet, opus, etc.)")
	vaultFlag := flag.String("vault", "", "Vault name")
	resumeFlag := flag.Bool("resume", false, "Resume most recent session")
	cFlag := flag.Bool("c", false, "Resume most recent session (short)")

	flag.Parse()

	cfg := LoadConfig()
	if *urlFlag != "" {
		cfg.RunnerURL = *urlFlag
	}
	if *keyFlag != "" {
		cfg.APIKey = *keyFlag
	}
	if *modelFlag != "" {
		cfg.Model = *modelFlag
	}
	if *vaultFlag != "" {
		cfg.VaultName = *vaultFlag
	}

	// Handle --resume / -c
	if *resumeFlag || *cFlag {
		requireConfig(cfg)
		cmdResume(cfg)
		return
	}

	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(1)
	}

	switch args[0] {
	case "new":
		requireConfig(cfg)
		model := cfg.Model
		if len(args) > 1 {
			model = args[1]
		}
		cmdNew(cfg, model)

	case "ls":
		requireConfig(cfg)
		cmdList(cfg)

	case "attach":
		requireConfig(cfg)
		if len(args) < 2 {
			fatal("usage: cass attach <session-id>")
		}
		cmdAttach(cfg, args[1])

	case "kill":
		requireConfig(cfg)
		if len(args) < 2 {
			fatal("usage: cass kill <session-id>")
		}
		cmdKill(cfg, args[1])

	default:
		fatal("unknown command: %s", args[0])
	}
}

func cmdNew(cfg Config, model string) {
	api := NewAPIClient(cfg.RunnerURL, cfg.APIKey)
	fmt.Fprintf(os.Stderr, "Creating session (model: %s)...\n", model)

	resp, err := api.CreateSession(SessionRequest{
		Model: model,
		Vault: cfg.VaultName,
	})
	if err != nil {
		fatal("create session: %v", err)
	}

	// Pod IP may not be available immediately — retry via list endpoint
	if resp.PodIP == "" {
		sessions, err := api.ListSessions()
		if err == nil {
			for _, s := range sessions {
				if s.SessionID == resp.SessionID && s.PodIP != "" {
					resp.PodIP = s.PodIP
					break
				}
			}
		}
		if resp.PodIP == "" {
			fatal("session created (%s) but no pod IP available", resp.SessionID)
		}
	}

	fmt.Fprintf(os.Stderr, "Session %s created. Connecting to %s...\n", short(resp.SessionID), resp.PodIP)
	connectWithSync(resp.PodIP, resp.SessionID, cfg)
}

func cmdResume(cfg Config) {
	api := NewAPIClient(cfg.RunnerURL, cfg.APIKey)
	sessions, err := api.ListSessions()
	if err != nil {
		fatal("list sessions: %v", err)
	}

	// Filter to running sessions with pod IPs
	var running []SessionInfo
	for _, s := range sessions {
		if (s.Status == "ready" || s.Status == "busy" || s.Status == "idle") && s.PodIP != "" {
			running = append(running, s)
		}
	}

	if len(running) == 0 {
		fatal("no running sessions found — use 'cass new' to create one")
	}

	// Single session — attach directly
	if len(running) == 1 {
		s := running[0]
		fmt.Fprintf(os.Stderr, "Resuming session %s (%s). Connecting to %s...\n", short(s.SessionID), s.Model, s.PodIP)
		connectWithSync(s.PodIP, s.SessionID, cfg)
		return
	}

	// Multiple sessions — show picker
	fmt.Fprintf(os.Stderr, "Multiple running sessions:\n\n")
	for i, s := range running {
		fmt.Fprintf(os.Stderr, "  %d) %s  %s  %s  %s\n", i+1, short(s.SessionID), s.Model, s.PodIP, s.CreatedAt)
	}
	fmt.Fprintf(os.Stderr, "\nSelect session [1-%d]: ", len(running))

	var choice int
	if _, err := fmt.Scanf("%d", &choice); err != nil || choice < 1 || choice > len(running) {
		fatal("invalid selection")
	}

	s := running[choice-1]
	fmt.Fprintf(os.Stderr, "Connecting to %s...\n", short(s.SessionID))
	connectWithSync(s.PodIP, s.SessionID, cfg)
}

func cmdList(cfg Config) {
	api := NewAPIClient(cfg.RunnerURL, cfg.APIKey)
	sessions, err := api.ListSessions()
	if err != nil {
		fatal("list sessions: %v", err)
	}

	if len(sessions) == 0 {
		fmt.Println("No sessions.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SESSION\tMODEL\tSTATUS\tPOD IP\tCREATED")
	for _, s := range sessions {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", short(s.SessionID), s.Model, s.Status, s.PodIP, s.CreatedAt)
	}
	w.Flush()
}

func cmdAttach(cfg Config, sessionID string) {
	api := NewAPIClient(cfg.RunnerURL, cfg.APIKey)
	sessions, err := api.ListSessions()
	if err != nil {
		fatal("list sessions: %v", err)
	}

	// Match by prefix
	for _, s := range sessions {
		if s.SessionID == sessionID || (len(sessionID) >= 4 && len(s.SessionID) >= len(sessionID) && s.SessionID[:len(sessionID)] == sessionID) {
			if s.PodIP == "" {
				fatal("session %s has no pod IP", short(s.SessionID))
			}
			fmt.Fprintf(os.Stderr, "Attaching to %s (%s). Connecting to %s...\n", short(s.SessionID), s.Model, s.PodIP)
			connectWithSync(s.PodIP, s.SessionID, cfg)
			return
		}
	}

	fatal("session %s not found", sessionID)
}

func cmdKill(cfg Config, sessionID string) {
	api := NewAPIClient(cfg.RunnerURL, cfg.APIKey)
	if err := api.DeleteSession(sessionID); err != nil {
		fatal("kill session: %v", err)
	}
	fmt.Fprintf(os.Stderr, "Session %s killed.\n", short(sessionID))
}

func requireConfig(cfg Config) {
	if cfg.RunnerURL == "" {
		fatal("--url or runnerURL in ~/.cassandra/config.json is required")
	}
	if cfg.APIKey == "" {
		fatal("--key or apiKey in ~/.cassandra/config.json is required")
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `Usage: cass <command> [args]

Commands:
  new [model]      Create a new session and connect
  ls               List sessions
  attach <id>      Attach to an existing session
  kill <id>        Kill a session
Flags:
  --resume, -c     Resume most recent session
  --model          Model override
  --url            Runner URL
  --key            API key
  --vault          Vault name`)
}

// connectWithSync starts mutagen sync, connects via SSH, then stops sync on disconnect.
func connectWithSync(podIP, sessionID string, cfg Config) {
	if hasMutagen() {
		fmt.Fprintf(os.Stderr, "Starting file sync...\n")
		localDir, err := startSync(podIP, sessionID, cfg)
		if err != nil {
			fatal("sync failed: %v", err)
		}
		fmt.Fprintf(os.Stderr, "Syncing: %s ↔ pod:/workspace\n", localDir)
		defer func() {
			fmt.Fprintf(os.Stderr, "Flushing sync...\n")
			flushSync(sessionID)
			stopSync(sessionID)
		}()
	}

	_, err := connectSSH(podIP, cfg)
	if err != nil {
		fatal("ssh: %v", err)
	}
	printDetachHint(sessionID)
}

func printDetachHint(sessionID string) {
	fmt.Fprintf(os.Stderr, "\nSession: %s\n", sessionID)
	fmt.Fprintf(os.Stderr, "  Resume:  cass --resume\n")
	fmt.Fprintf(os.Stderr, "  Attach:  cass attach %s\n", short(sessionID))
	fmt.Fprintf(os.Stderr, "  Kill:    cass kill %s\n", short(sessionID))
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "Error: "+format+"\n", args...)
	os.Exit(1)
}

func short(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
