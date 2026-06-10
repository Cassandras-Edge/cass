package cmd

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
)

// all: so that __init__.py.tmpl (leading underscore) is included.
//
//go:embed all:templates/newmcp
var newMcpTemplates embed.FS

// newMcpData feeds the scaffold templates. Derived entirely from the
// service name, e.g. name "reddit" →
//
//	Dir       cassandra-reddit-mcp   (default output dir + README title)
//	Repo      cassandra-reddit-mcp   (pyproject name + console script)
//	Package   cassandra_reddit_mcp   (python package)
//	ServiceID reddit-mcp             (acl.yaml + woodpecker build args)
//	Title     Reddit                 (human-facing strings)
//	EnvPrefix REDDIT                 (env var hints)
type newMcpData struct {
	Dir       string
	Repo      string
	Package   string
	ServiceID string
	Title     string
	EnvPrefix string
	Port      int
}

func newMcpCmd() *cobra.Command {
	var port int
	var dir string

	cmd := &cobra.Command{
		Use:   "new-mcp <name>",
		Short: "Scaffold a new Cassandra MCP service repo",
		Long: "Generates the standard per-repo files for a Cassandra MCP service\n" +
			"(FastMCP + cassandra-kit + Woodpecker CI), modeled on cassandra-reddit-mcp.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runNewMcp(args[0], port, dir)
		},
	}
	cmd.Flags().IntVar(&port, "port", 3003, "MCP listen port baked into the scaffold")
	cmd.Flags().StringVar(&dir, "dir", "", "output directory (default ./cassandra-<name>-mcp)")
	return cmd
}

// normalizeMcpName strips any cassandra-/-mcp decoration so all derived
// names come from the bare service name ("cassandra-reddit-mcp" → "reddit").
func normalizeMcpName(raw string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(raw))
	name = strings.TrimPrefix(name, "cassandra-")
	name = strings.TrimSuffix(name, "-mcp")
	if name == "" {
		return "", fmt.Errorf("invalid service name %q", raw)
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return "", fmt.Errorf("invalid service name %q: use lowercase letters, digits, and hyphens", raw)
		}
	}
	return name, nil
}

func runNewMcp(rawName string, port int, dir string) error {
	name, err := normalizeMcpName(rawName)
	if err != nil {
		return err
	}

	repo := "cassandra-" + name + "-mcp"
	if dir == "" {
		dir = "./" + repo
	}
	data := newMcpData{
		Dir:       filepath.Base(dir),
		Repo:      repo,
		Package:   strings.ReplaceAll(repo, "-", "_"),
		ServiceID: name + "-mcp",
		Title:     strings.ToUpper(name[:1]) + name[1:],
		EnvPrefix: strings.ToUpper(strings.ReplaceAll(name, "-", "_")),
		Port:      port,
	}

	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("output directory %s already exists", dir)
	}

	pkgDir := filepath.Join("backend", "src", data.Package)
	files := []struct {
		tmpl string // path under templates/newmcp
		out  string // path under dir
	}{
		{"pyproject.toml.tmpl", filepath.Join("backend", "pyproject.toml")},
		{"Dockerfile.tmpl", filepath.Join("backend", "Dockerfile")},
		{"__init__.py.tmpl", filepath.Join(pkgDir, "__init__.py")},
		{"config.py.tmpl", filepath.Join(pkgDir, "config.py")},
		{"mcp_server.py.tmpl", filepath.Join(pkgDir, "mcp_server.py")},
		{"main.py.tmpl", filepath.Join(pkgDir, "main.py")},
		{"woodpecker.yaml.tmpl", ".woodpecker.yaml"},
		{"acl.yaml.example.tmpl", "acl.yaml.example"},
		{"README.md.tmpl", "README.md"},
		{"gitignore.tmpl", ".gitignore"},
	}

	for _, f := range files {
		raw, err := newMcpTemplates.ReadFile("templates/newmcp/" + f.tmpl)
		if err != nil {
			return fmt.Errorf("read template %s: %w", f.tmpl, err)
		}
		t, err := template.New(f.tmpl).Parse(string(raw))
		if err != nil {
			return fmt.Errorf("parse template %s: %w", f.tmpl, err)
		}
		outPath := filepath.Join(dir, f.out)
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return err
		}
		out, err := os.Create(outPath)
		if err != nil {
			return err
		}
		if err := t.Execute(out, data); err != nil {
			out.Close()
			return fmt.Errorf("render %s: %w", f.out, err)
		}
		if err := out.Close(); err != nil {
			return err
		}
		fmt.Printf("  created %s\n", outPath)
	}

	fmt.Printf(`
Scaffolded %s in %s (port %d, service id %s).

Next steps:
  1. Implement real tools in backend/src/%s/mcp_server.py (replace the ping stub).
  2. Create the GitHub repo and push:
       cd %s && git init && git add -A && git commit -m "scaffold"
       gh repo create Cassandras-Edge/%s --private --source . --push
  3. Enable Woodpecker CI and add secrets:
       scripts/wpcli repo add Cassandras-Edge/%s
       scripts/wpcli repo secret add Cassandras-Edge/%s --name github_token --value "$GITHUB_TOKEN" --event push --event pull_request
       scripts/wpcli repo secret add Cassandras-Edge/%s --name auth_yaml --value "$(cat env/acl.yaml)" --event push --event pull_request
       scripts/wpcli repo secret add Cassandras-Edge/%s --name discord_webhook_url --value "$DISCORD_WEBHOOK_URL" --event push --event pull_request
  4. Add the ACL policy to cassandra-stack/env/acl.yaml (see acl.yaml.example).
  5. Create the Helm chart: cassandra-k8s/apps/%s/ (copy apps/reddit-mcp) + ArgoCD app.
  6. Add the CF tunnel route + DNS in cassandra-infra (tunnel.tf), then tofu apply.
  7. Add the repo as a submodule in cassandra-stack.
`, data.Repo, dir, port, data.ServiceID,
		data.Package, dir, data.Repo, data.Repo, data.Repo, data.Repo, data.Repo,
		data.ServiceID)

	return nil
}
