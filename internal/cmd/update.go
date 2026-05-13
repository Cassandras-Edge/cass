package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const releasesAPI = "https://api.github.com/repos/Cassandras-Edge/cass/releases"

type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func updateCmd() *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update cass to the latest release",
		RunE: func(_ *cobra.Command, _ []string) error {
			rel, err := fetchLatest()
			if err != nil {
				return err
			}
			latest := strings.TrimPrefix(rel.TagName, "v")
			fmt.Printf("Current version: %s\n", Version)
			fmt.Printf("Latest version:  %s\n", latest)
			if check {
				if latest == Version {
					fmt.Println("cass is up to date.")
				} else {
					fmt.Printf("Update available: %s → %s\n", Version, latest)
				}
				return nil
			}
			if latest == Version {
				fmt.Println("Already up to date.")
				return nil
			}
			return installRelease(rel)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "Only report what would be updated, don't install")
	return cmd
}

func installCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "install [target]",
		Short: "Install cass — latest, or a specific version (e.g. 0.6.8 / v0.6.8)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			target := "latest"
			if len(args) == 1 {
				target = args[0]
			}
			fmt.Printf("Current version: %s\n", Version)
			rel, err := resolveRelease(target)
			if err != nil {
				return err
			}
			desired := strings.TrimPrefix(rel.TagName, "v")
			fmt.Printf("Target version:  %s\n", desired)
			if desired == Version && !force {
				fmt.Println("Already at target version. Use --force to reinstall.")
				return nil
			}
			return installRelease(rel)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Reinstall even if already at target version")
	return cmd
}

func resolveRelease(target string) (*ghRelease, error) {
	if target == "latest" || target == "stable" {
		return fetchLatest()
	}
	tag := target
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	return fetchTag(tag)
}

func fetchLatest() (*ghRelease, error) {
	return doReleaseRequest(releasesAPI + "/latest")
}

func fetchTag(tag string) (*ghRelease, error) {
	return doReleaseRequest(releasesAPI + "/tags/" + tag)
}

func doReleaseRequest(url string) (*ghRelease, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("github %s: %s", resp.Status, string(body))
	}
	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

func detectTarget() (string, error) {
	var arch string
	switch runtime.GOARCH {
	case "amd64", "x86_64":
		arch = "amd64"
	case "arm64", "aarch64":
		arch = "arm64"
	default:
		return "", fmt.Errorf("unsupported architecture: %s", runtime.GOARCH)
	}
	switch runtime.GOOS {
	case "darwin", "linux":
		return runtime.GOOS + "-" + arch, nil
	case "windows":
		return "windows-" + arch, nil
	}
	return "", fmt.Errorf("unsupported OS: %s", runtime.GOOS)
}

func installRelease(rel *ghRelease) error {
	target, err := detectTarget()
	if err != nil {
		return err
	}
	assetName := "cass-" + target
	if strings.HasPrefix(target, "windows") {
		assetName += ".exe"
	}
	var url string
	for _, a := range rel.Assets {
		if a.Name == assetName {
			url = a.BrowserDownloadURL
			break
		}
	}
	if url == "" {
		return fmt.Errorf("no binary found for %s in release %s", target, rel.TagName)
	}

	fmt.Printf("Downloading %s...\n", assetName)
	dest, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate current binary: %w", err)
	}
	dest, _ = filepath.EvalSymlinks(dest)

	// Stream to a sibling temp file so the final os.Rename is atomic and
	// stays on the same filesystem.
	dir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(dir, ".cass-update-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			os.Remove(tmpPath)
		}
	}()

	httpc := &http.Client{Timeout: 90 * time.Second}
	resp, err := httpc.Get(url)
	if err != nil {
		tmp.Close()
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		tmp.Close()
		return fmt.Errorf("download: %s", resp.Status)
	}
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return err
	}
	// On Unix, replacing the currently-running binary is fine: open file
	// descriptors keep pointing at the old inode until the process exits.
	if err := os.Rename(tmpPath, dest); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	cleanup = false
	fmt.Printf("Installed %s → %s\n", rel.TagName, dest)
	return nil
}
