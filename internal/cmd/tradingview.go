package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

const tradingViewProxyService = "tradingview-proxy"
const defaultTradingViewProxyURL = "wss://tradingview.cassandrasedge.com"

type tradingViewSetup struct {
	proxyURL     string
	extensionSrc string
	extensionDir string
	extensionID  string
	noOpen       bool
}

func tradingViewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "tradingview",
		Aliases: []string{"tv"},
		Short:   "Set up TradingView proxy access for Chrome",
	}
	cmd.AddCommand(tradingViewSetupCmd())
	return cmd
}

func tradingViewSetupCmd() *cobra.Command {
	opts := tradingViewSetup{
		proxyURL: defaultTradingViewProxyURL,
	}
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Stage the Chrome extension and configure a TradingView proxy key",
		Long: `Stages the Cassandra TradingView Proxy Chrome extension, mints or reuses
your tradingview-proxy MCP key, and opens Chrome's extension manager.

Chrome does not allow a normal CLI to silently install an extension into an
existing profile. This command performs the reliable parts: it prepares the
unpacked extension, writes local setup details, and opens the right Chrome
screen. After loading the unpacked extension once, use its Options page to save
the generated proxy URL and MCP key.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runTradingViewSetup(opts)
		},
	}
	cmd.Flags().StringVar(&opts.proxyURL, "proxy-url", defaultTradingViewProxyURL, "TradingView proxy base URL")
	cmd.Flags().StringVar(&opts.extensionSrc, "extension-src", "", "Source directory for the unpacked extension (auto-detected in cassandra-stack)")
	cmd.Flags().StringVar(&opts.extensionDir, "extension-dir", "", "Destination directory for the staged extension (default: ~/.cass/tradingview-extension)")
	cmd.Flags().StringVar(&opts.extensionID, "extension-id", "", "Existing Chrome extension ID; opens its options page with generated config")
	cmd.Flags().BoolVar(&opts.noOpen, "no-open", false, "Print setup instructions without opening Chrome")
	return cmd
}

func runTradingViewSetup(opts tradingViewSetup) error {
	proxyURL := strings.TrimSpace(opts.proxyURL)
	if proxyURL == "" {
		return errors.New("--proxy-url is required")
	}

	key, action, err := ensureServiceKey(tradingViewProxyService, false)
	if err != nil {
		return err
	}

	src, err := resolveTradingViewExtensionSource(opts.extensionSrc)
	if err != nil {
		return err
	}
	dst, err := resolveTradingViewExtensionDir(opts.extensionDir)
	if err != nil {
		return err
	}
	if err := copyDir(src, dst); err != nil {
		return fmt.Errorf("stage extension: %w", err)
	}
	if err := writeTradingViewSetupFile(dst, proxyURL, key, opts.extensionID); err != nil {
		return err
	}

	fmt.Println("TradingView proxy setup")
	if action == "created" {
		fmt.Println("  Created a tradingview-proxy MCP key.")
	} else {
		fmt.Println("  Reused the cached tradingview-proxy MCP key.")
	}
	fmt.Printf("  Extension staged at: %s\n", dst)
	fmt.Printf("  Proxy URL: %s\n", proxyURL)
	fmt.Printf("  Setup details: %s\n", filepath.Join(dst, "cass-setup.json"))
	fmt.Println()
	fmt.Println("Chrome setup:")
	fmt.Println("  1. Enable Developer mode in chrome://extensions.")
	fmt.Println("  2. Load unpacked and choose the staged extension directory above.")
	if opts.extensionID != "" {
		fmt.Println("  3. The extension options page was opened with the generated config; click Save there.")
	} else {
		fmt.Println("  3. Open the extension Options page and paste the proxy URL plus MCP key from cass-setup.json.")
	}
	fmt.Println()
	fmt.Println("Chrome cannot be silently modified by cass-cli unless the browser is enterprise-managed, so this keeps the install explicit while automating the key and extension staging.")

	if !opts.noOpen {
		if opts.extensionID != "" {
			openBrowser(tradingViewOptionsURL(opts.extensionID, proxyURL, key))
		} else {
			openBrowser("chrome://extensions")
		}
	}
	return nil
}

func resolveTradingViewExtensionSource(explicit string) (string, error) {
	if explicit != "" {
		return requireExtensionDir(explicit)
	}

	candidates := []string{}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			filepath.Join(wd, "cassandra-tradingview-proxy", "clients", "extension"),
			filepath.Join(wd, "..", "cassandra-tradingview-proxy", "clients", "extension"),
		)
	}
	if exe, err := os.Executable(); err == nil {
		base := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(base, "cassandra-tradingview-proxy", "clients", "extension"),
			filepath.Join(base, "..", "cassandra-tradingview-proxy", "clients", "extension"),
		)
	}

	for _, p := range candidates {
		if dir, err := requireExtensionDir(p); err == nil {
			return dir, nil
		}
	}
	return "", errors.New("could not find TradingView extension source; pass --extension-src /path/to/cassandra-tradingview-proxy/clients/extension")
}

func requireExtensionDir(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", abs)
	}
	if _, err := os.Stat(filepath.Join(abs, "manifest.json")); err != nil {
		return "", fmt.Errorf("%s does not look like a Chrome extension: %w", abs, err)
	}
	return abs, nil
}

func resolveTradingViewExtensionDir(explicit string) (string, error) {
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cass", "tradingview-extension"), nil
}

func writeTradingViewSetupFile(dir, proxyURL, key, extensionID string) error {
	setup := map[string]string{
		"proxyURL":     proxyURL,
		"mcpKey":       key,
		"service":      tradingViewProxyService,
		"optionsHint":  "After loading the unpacked extension, open its Options page and save these values.",
		"chromeURL":    "chrome://extensions",
		"extensionDir": dir,
	}
	if extensionID != "" {
		setup["extensionID"] = extensionID
		setup["optionsURL"] = tradingViewOptionsURL(extensionID, proxyURL, key)
	}
	data, err := json.MarshalIndent(setup, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "cass-setup.json"), append(data, '\n'), 0o600)
}

func tradingViewOptionsURL(extensionID, proxyURL, key string) string {
	// The values ride in the fragment so they are only read by the extension
	// options page, not sent as a network request.
	return "chrome-extension://" + extensionID + "/options.html#proxyURL=" +
		url.QueryEscape(proxyURL) + "&mcpKey=" + url.QueryEscape(key)
}

func copyDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		info, err := d.Info()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	})
}
