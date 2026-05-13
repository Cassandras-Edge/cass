package tui

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"

	"github.com/Cassandras-Edge/cass/internal/auth"
)

// menuEntry is one row in the dashboard picker. Cmd is the subcommand to exec
// when chosen; Ready=false renders the entry greyed-out and disables it.
type menuEntry struct {
	Cmd   string
	Label string
	Hint  string
	Ready bool
}

var menu = []menuEntry{
	{Cmd: "setup", Label: "setup", Hint: "First-time platform setup (Claude + Codex)", Ready: true},
	{Cmd: "login", Label: "login", Hint: "Authenticate via browser (per-device CF token)", Ready: true},
	{Cmd: "whoami", Label: "whoami", Hint: "Show current identity", Ready: true},
	{Cmd: "ensure-key", Label: "ensure-key", Hint: "Get or mint a per-service MCP key", Ready: true},
	{Cmd: "refresh-keys", Label: "refresh-keys", Hint: "Provision MCP keys for every plugin", Ready: true},
	{Cmd: "devices", Label: "devices", Hint: "List / revoke authorized devices", Ready: true},
	{Cmd: "keys", Label: "keys", Hint: "List cached service keys / clear cache", Ready: true},
	{Cmd: "cookies", Label: "cookies", Hint: "Sync browser cookies into services", Ready: true},
	{Cmd: "share", Label: "share", Hint: "Share Claude Code conversations via ephemeral URLs", Ready: true},
	{Cmd: "image", Label: "image", Hint: "Generate / edit an image via ChatGPT subscription", Ready: true},
	{Cmd: "update", Label: "update", Hint: "Update cass to the latest release", Ready: true},
	{Cmd: "logout", Label: "logout", Hint: "Remove cached credentials on this device", Ready: true},
}

var (
	headerStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("63")).
			Padding(0, 2).
			MarginBottom(1)
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("63"))
	dimStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	hintStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
)

func Run() error {
	fmt.Println(renderHeader())

	options := make([]huh.Option[string], 0, len(menu))
	for _, e := range menu {
		label := fmt.Sprintf("%-12s  %s", e.Label, hintStyle.Render(e.Hint))
		if !e.Ready {
			label = dimStyle.Render(fmt.Sprintf("%-12s  %s  (not yet ported)", e.Label, e.Hint))
		}
		options = append(options, huh.NewOption(label, e.Cmd))
	}

	var choice string
	form := huh.NewSelect[string]().
		Title("What would you like to do?").
		Options(options...).
		Value(&choice).
		Height(len(menu) + 4)

	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return nil
		}
		return err
	}

	for _, e := range menu {
		if e.Cmd == choice && !e.Ready {
			return fmt.Errorf("%s is not yet ported to Go", e.Cmd)
		}
	}
	return execSelf(choice)
}

func renderHeader() string {
	creds, err := auth.Read()
	title := titleStyle.Render("cass")
	if err != nil || creds.MCPKey == "" {
		return headerStyle.Render(fmt.Sprintf("%s\n\n%s",
			title,
			dimStyle.Render("Not logged in — pick 'login' below to authenticate."),
		))
	}
	key := creds.MCPKey
	if len(key) > 20 {
		key = key[:20] + "…"
	}
	body := fmt.Sprintf("%s\n\n%s  %s\n%s  %s",
		title,
		dimStyle.Render("email"), creds.Email,
		dimStyle.Render("key  "), key,
	)
	return headerStyle.Render(body)
}

func execSelf(sub string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	c := exec.Command(exe, sub)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}
