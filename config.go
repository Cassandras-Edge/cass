package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type Config struct {
	RunnerURL  string `json:"runnerURL"`
	APIKey     string `json:"apiKey"`
	Model      string `json:"model"`
	VaultName  string `json:"vaultName"`
	SSHUser    string `json:"sshUser"`
	SSHKeyPath string `json:"sshKeyPath"`
}

func LoadConfig() Config {
	cfg := Config{
		Model: "opus[1m]",
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return cfg
	}

	path := filepath.Join(home, ".cassandra", "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(data, &cfg)

	// Defaults
	if cfg.SSHUser == "" {
		cfg.SSHUser = "runner"
	}
	if cfg.SSHKeyPath == "" {
		cfg.SSHKeyPath = filepath.Join(home, ".ssh", "id_ed25519")
	}

	return cfg
}
