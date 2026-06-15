package tui

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Cassandras-Edge/cass/internal/auth"
	"github.com/Cassandras-Edge/cass/internal/config"
)

// dashboard is a grouped task launcher: pick a row, it execs the namespaced
// command (e.g. {"link","gmail"}, {"auth","whoami"}). Section headers are
// non-selectable; the cursor skips over them.

// item is a launchable row. args is the namespaced command path to exec.
// svc, when non-empty, names the link-account service whose linked? badge
// is shown after a best-effort status probe.
type item struct {
	label string
	args  []string
	svc   string // link-account service id for the badge probe (optional)
}

// section is a header plus its launchable rows.
type section struct {
	title string
	items []item
}

// LinkAccount is one row of the LINK ACCOUNTS menu. cmd.New() injects these via
// SetLinkAccounts from the single link-account table in root.go, so adding an
// account there populates both the CLI and this interactive menu — no second edit.
type LinkAccount struct {
	Label string
	Args  []string // command path the row execs
	Svc   string   // badge probe service id ("" = no badge)
}

var linkAccounts []LinkAccount

// SetLinkAccounts is called once at startup (before Run) by the cmd package.
func SetLinkAccounts(a []LinkAccount) { linkAccounts = a }

var (
	sectionAuth = section{
		title: "AUTH",
		items: []item{
			{label: "whoami", args: []string{"auth", "whoami"}},
			{label: "login", args: []string{"auth", "login"}},
			{label: "logout", args: []string{"auth", "logout"}},
			{label: "devices", args: []string{"auth", "devices", "list"}},
		},
	}
	sectionClients = section{
		title: "CLIENTS",
		items: []item{
			{label: "setup", args: []string{"client", "setup"}},
			{label: "claude", args: []string{"client", "claude"}},
			{label: "codex", args: []string{"client", "codex"}},
			{label: "flock", args: []string{"client", "flock"}},
			{label: "config", args: []string{"client", "config", "show"}},
		},
	}
	sectionTools = section{
		title: "TOOLS",
		items: []item{
			{label: "share", args: []string{"tools", "share", "list"}},
			{label: "update", args: []string{"tools", "update"}},
		},
	}
)

// sections assembles the menu, building LINK ACCOUNTS from the injected table.
func sections() []section {
	link := section{title: "LINK ACCOUNTS"}
	for _, la := range linkAccounts {
		link.items = append(link.items, item{label: la.Label, args: la.Args, svc: la.Svc})
	}
	return []section{sectionAuth, link, sectionClients, sectionTools}
}

var (
	headerStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("210")). // coral
			Padding(0, 2).
			MarginBottom(1)
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("210"))
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	sectionStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("245"))
	selectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("210"))
	okStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))  // green ●
	offStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("240")) // grey ○
	footerStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("240")).MarginTop(1)
)

// row is one rendered line in the flattened view. header rows are not
// selectable; the cursor only stops on rows where selectable is true.
type row struct {
	selectable bool
	itm        item
	rendered   func(selected bool) string
}

type model struct {
	rows    []row
	cursor  int // index into rows; always on a selectable row
	header  string
	badges  map[string]bool // svc -> linked? (best-effort, filled async)
	creds   auth.DeviceCreds
	authErr error
	chosen  *item
	quit    bool
	filter  string
	filtMod bool // typing into filter
}

// badgesMsg carries the result of the async link-status probe back into Update.
type badgesMsg map[string]bool

// Run shows the grouped launcher and execs the chosen command.
func Run() error {
	creds, authErr := auth.Read()

	m := newModel(creds, authErr)
	prog := tea.NewProgram(m)
	res, err := prog.Run()
	if err != nil {
		return err
	}
	fm := res.(model)
	if fm.quit || fm.chosen == nil {
		return nil
	}
	return execSelf(fm.chosen.args...)
}

func newModel(creds auth.DeviceCreds, authErr error) model {
	m := model{
		header:  renderHeader(creds, authErr),
		badges:  map[string]bool{},
		creds:   creds,
		authErr: authErr,
	}
	m.rebuild()
	// land the cursor on the first selectable row.
	m.cursor = m.firstSelectable()
	return m
}

// Init kicks off the best-effort link-status probe off the render path so the
// launcher paints instantly; badgesMsg repaints it when results land (or the
// deadline fires). No probe when logged out.
func (m model) Init() tea.Cmd {
	if m.authErr != nil || m.creds.MCPKey == "" {
		return nil
	}
	creds := m.creds
	return func() tea.Msg { return badgesMsg(probeBadges(creds, nil)) }
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case badgesMsg:
		m.badges = map[string]bool(msg)
		m.rebuild() // recompute rows so badges render
		return m, nil
	case tea.KeyMsg:
		if m.filtMod {
			return m.updateFilter(msg)
		}
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			m.quit = true
			return m, tea.Quit
		case "up", "k":
			m.cursor = m.prevSelectable(m.cursor)
		case "down", "j":
			m.cursor = m.nextSelectable(m.cursor)
		case "/":
			m.filtMod = true
		case "enter":
			if m.cursor >= 0 && m.cursor < len(m.rows) && m.rows[m.cursor].selectable {
				it := m.rows[m.cursor].itm
				m.chosen = &it
				return m, tea.Quit
			}
		}
	}
	return m, nil
}

func (m model) updateFilter(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.filtMod = false
		m.filter = ""
		m.rebuild()
		m.cursor = m.firstSelectable()
	case "enter":
		m.filtMod = false
		m.cursor = m.firstSelectable()
	case "backspace":
		if m.filter != "" {
			m.filter = m.filter[:len(m.filter)-1]
		}
		m.rebuild()
		m.cursor = m.firstSelectable()
	case "ctrl+c":
		m.quit = true
		return m, tea.Quit
	default:
		if len(msg.String()) == 1 {
			m.filter += msg.String()
			m.rebuild()
			m.cursor = m.firstSelectable()
		}
	}
	return m, nil
}

func (m model) View() string {
	var b strings.Builder
	b.WriteString(m.header)
	b.WriteString("\n")
	for i, r := range m.rows {
		b.WriteString(r.rendered(i == m.cursor))
		b.WriteString("\n")
	}
	if m.filtMod {
		b.WriteString(dimStyle.Render("  / " + m.filter + "▌"))
		b.WriteString("\n")
	}
	b.WriteString(footerStyle.Render("  ↑↓ move   ⏎ run   / filter   q quit"))
	return b.String()
}

// rebuild flattens sections into rows, honouring the active filter. Section
// headers are dropped when none of their items match the filter.
func (m *model) rebuild() {
	f := strings.ToLower(strings.TrimSpace(m.filter))
	var rows []row
	for _, sec := range sections() {
		var matched []item
		for _, it := range sec.items {
			if f == "" || strings.Contains(strings.ToLower(it.label), f) {
				matched = append(matched, it)
			}
		}
		if len(matched) == 0 {
			continue
		}
		sec := sec
		rows = append(rows, row{
			selectable: false,
			rendered: func(bool) string {
				return "  " + sectionStyle.Render(sec.title)
			},
		})
		for _, it := range matched {
			it := it
			badge := m.badgeFor(it)
			rows = append(rows, row{
				selectable: true,
				itm:        it,
				rendered: func(selected bool) string {
					marker := "  "
					label := it.label
					if selected {
						marker = " →"
						label = selectedStyle.Render(label)
					}
					line := fmt.Sprintf("%s  %s", marker, label)
					if badge != "" {
						line += " " + badge
					}
					return line
				},
			})
		}
	}
	m.rows = rows
}

// badgeFor returns a coloured ●/○ badge for link-account rows whose status we
// probed, or "" when there's no badge to show (non-link rows, or rows we
// didn't manage to probe in the deadline).
func (m *model) badgeFor(it item) string {
	if it.svc == "" {
		return ""
	}
	linked, ok := m.badges[it.svc]
	if !ok {
		return ""
	}
	if linked {
		return okStyle.Render("●")
	}
	return offStyle.Render("○")
}

func (m model) firstSelectable() int {
	for i, r := range m.rows {
		if r.selectable {
			return i
		}
	}
	return 0
}

func (m model) nextSelectable(from int) int {
	for i := from + 1; i < len(m.rows); i++ {
		if m.rows[i].selectable {
			return i
		}
	}
	return from
}

func (m model) prevSelectable(from int) int {
	for i := from - 1; i >= 0; i-- {
		if m.rows[i].selectable {
			return i
		}
	}
	return from
}

func renderHeader(creds auth.DeviceCreds, err error) string {
	title := titleStyle.Render("cass · this-mac")
	if err != nil || creds.MCPKey == "" {
		return headerStyle.Render(fmt.Sprintf("%s\n%s",
			title,
			dimStyle.Render("Not logged in — choose login"),
		))
	}
	body := fmt.Sprintf("%s\n%s   %s",
		title,
		creds.Email,
		okStyle.Render("●")+" "+dimStyle.Render("logged in"),
	)
	return headerStyle.Render(body)
}

// probeBadges runs a best-effort, single-deadline (≤1.5s total) check of which
// link-account services are linked. All probes run concurrently against the
// portal credentials endpoint; whatever returns before the deadline is shown,
// the rest render without a badge. Never blocks the launcher meaningfully and
// returns an empty map when not logged in.
func probeBadges(creds auth.DeviceCreds, authErr error) map[string]bool {
	out := map[string]bool{}
	if authErr != nil || creds.MCPKey == "" {
		return out
	}

	// Only link-account services with a cheap per-service credentials lookup —
	// derived from the same injected table that builds the menu.
	var svcs []string
	for _, la := range linkAccounts {
		if la.Svc != "" {
			svcs = append(svcs, la.Svc)
		}
	}

	deadline := 1500 * time.Millisecond
	client := &http.Client{Timeout: deadline}

	type result struct {
		svc    string
		linked bool
	}
	resultCh := make(chan result, len(svcs))
	var wg sync.WaitGroup
	for _, svc := range svcs {
		wg.Add(1)
		go func(svc string) {
			defer wg.Done()
			linked, ok := probeLinked(client, creds, svc)
			if ok {
				resultCh <- result{svc: svc, linked: linked}
			}
		}(svc)
	}
	go func() { wg.Wait(); close(resultCh) }()

	timer := time.NewTimer(deadline)
	defer timer.Stop()
	for {
		select {
		case r, more := <-resultCh:
			if !more {
				return out
			}
			out[r.svc] = r.linked
		case <-timer.C:
			return out
		}
	}
}

// probeLinked GETs the portal credentials endpoint for one service. Request
// shape matches internal/cmd/plaud.go runPlaudStatus: bearer MCP key + the two
// CF-Access headers. Returns (linked, ok); ok=false means we couldn't tell.
func probeLinked(client *http.Client, creds auth.DeviceCreds, svc string) (linked, ok bool) {
	u := fmt.Sprintf("%s/api/extension/credentials/%s", config.PortalURL(), svc)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return false, false
	}
	req.Header.Set("CF-Access-Client-Id", creds.CFAccessClientID)
	req.Header.Set("CF-Access-Client-Secret", creds.CFAccessClientSecret)
	req.Header.Set("Authorization", "Bearer "+creds.MCPKey)
	resp, err := client.Do(req)
	if err != nil {
		return false, false
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent {
		return false, true // definitively not linked
	}
	if resp.StatusCode != http.StatusOK {
		return false, false
	}
	var raw map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&raw); err != nil {
		return false, false
	}
	if inner, isMap := raw["credentials"].(map[string]any); isMap {
		raw = inner
	}
	return len(raw) > 0, true
}

func execSelf(args ...string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	c := exec.Command(exe, args...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}
