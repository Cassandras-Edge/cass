package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

const ccPatchesRepo = "Cassandras-Edge/cassandra-cc-patches"

func patchedBinPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "bin", "claude-patched")
}

func patchedCLICmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "patched-cli",
		Short: "Manage the patched Claude Code CLI at ~/.local/bin/claude-patched",
		Long: `Used by marketplace plugins (e.g. stopgate) that need a patched
claude binary. Installs the prebuilt artifact from cassandra-cc-patches
GitHub releases. (The legacy --local NPM repack path from the Python
build isn't carried over — use the Python cass if you need to repack
from source locally.)`,
	}
	cmd.AddCommand(patchedInstallCmd())
	cmd.AddCommand(patchedStatusCmd())
	cmd.AddCommand(patchedRestoreCmd())
	return cmd
}

func patchedInstallCmd() *cobra.Command {
	var releaseTag string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the prebuilt patched Claude CLI",
		RunE: func(_ *cobra.Command, _ []string) error {
			target, err := patchedHostTarget()
			if err != nil {
				return err
			}
			if _, err := exec.LookPath("gh"); err != nil {
				return fmt.Errorf("gh CLI not found. Install: brew install gh && gh auth login")
			}
			binPath := patchedBinPath()
			if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
				return err
			}
			if err := os.Remove(binPath); err != nil && !os.IsNotExist(err) {
				return err
			}

			asset := "claude-patched-" + target
			args := []string{"release", "download"}
			if releaseTag != "" {
				args = append(args, "--tag", releaseTag)
			}
			args = append(args, "--repo", ccPatchesRepo, "--pattern", asset, "--output", binPath)
			tagDesc := "latest"
			if releaseTag != "" {
				tagDesc = releaseTag
			}
			fmt.Printf("Downloading %s from %s (%s)...\n", asset, ccPatchesRepo, tagDesc)

			out, err := exec.Command("gh", args...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("gh release download: %w\n%s", err, string(out))
			}
			if err := os.Chmod(binPath, 0o755); err != nil {
				return err
			}
			if err := smokeTestPatched(); err != nil {
				return err
			}
			fmt.Printf("\nInstalled: %s\n", binPath)
			printPatchedVersion()
			return nil
		},
	}
	cmd.Flags().StringVar(&releaseTag, "release", "", "Specific cc-patches release tag (default: latest)")
	return cmd
}

func patchedStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show patched CLI installation status",
		RunE: func(_ *cobra.Command, _ []string) error {
			binPath := patchedBinPath()
			info, err := os.Lstat(binPath)
			if err != nil {
				return fmt.Errorf("not installed. Run: cass patched-cli install")
			}
			kind := "binary"
			if info.Mode()&os.ModeSymlink != 0 {
				kind = "symlink"
				if target, err := os.Readlink(binPath); err == nil {
					fmt.Printf("Path:    %s (%s → %s)\n", binPath, kind, target)
				} else {
					fmt.Printf("Path:    %s (%s)\n", binPath, kind)
				}
			} else {
				fmt.Printf("Path:    %s (%s)\n", binPath, kind)
			}
			printPatchedVersion()
			return nil
		},
	}
}

func patchedRestoreCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restore",
		Short: "Remove the patched CLI install",
		RunE: func(_ *cobra.Command, _ []string) error {
			removed := false
			if _, err := os.Lstat(patchedBinPath()); err == nil {
				if err := os.Remove(patchedBinPath()); err == nil {
					fmt.Printf("Removed %s\n", patchedBinPath())
					removed = true
				}
			}
			home, _ := os.UserHomeDir()
			prefix := filepath.Join(home, ".local", "share", "claude-patched")
			if _, err := os.Stat(prefix); err == nil {
				if err := os.RemoveAll(prefix); err == nil {
					fmt.Printf("Removed %s\n", prefix)
					removed = true
				}
			}
			if !removed {
				fmt.Println("Nothing to remove.")
			}
			return nil
		},
	}
}

// patchedHostTarget maps the local OS/arch to the cc-patches asset suffix
// (darwin-arm64, linux-x64, ...). Native Windows isn't supported by the
// patched build; WSL reports as Linux so it just works.
func patchedHostTarget() (string, error) {
	if runtime.GOOS == "windows" {
		return "", fmt.Errorf("native Windows not supported — run cass inside WSL")
	}
	var arch string
	switch runtime.GOARCH {
	case "amd64":
		arch = "x64"
	case "arm64":
		arch = "arm64"
	default:
		return "", fmt.Errorf("unsupported arch: %s", runtime.GOARCH)
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return "", fmt.Errorf("no patched build for %s", runtime.GOOS)
	}
	return runtime.GOOS + "-" + arch, nil
}

func smokeTestPatched() error {
	out, err := exec.Command(patchedBinPath(), "--version").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return fmt.Errorf("smoke test failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func printPatchedVersion() {
	out, err := exec.Command(patchedBinPath(), "--version").Output()
	if err == nil {
		fmt.Printf("Version: %s\n", strings.TrimSpace(string(out)))
	}
}
