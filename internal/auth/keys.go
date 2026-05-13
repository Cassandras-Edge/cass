package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
)

func KeysDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cass", "keys")
}

func KeyPath(service string) string {
	return filepath.Join(KeysDir(), service+".json")
}

type cachedKey struct {
	Key     string `json:"key"`
	Service string `json:"service"`
	Email   string `json:"email"`
}

func GetServiceKey(service string) string {
	data, err := os.ReadFile(KeyPath(service))
	if err != nil {
		return ""
	}
	var c cachedKey
	if err := json.Unmarshal(data, &c); err != nil {
		return ""
	}
	return c.Key
}

func SaveServiceKey(service, key, email string) error {
	if err := os.MkdirAll(KeysDir(), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cachedKey{Key: key, Service: service, Email: email}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(KeyPath(service), data, 0o600)
}

func ClearServiceKey(service string) error {
	if err := os.Remove(KeyPath(service)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
