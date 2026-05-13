package share

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// JSONLToMarkdown renders a Claude Code session .jsonl as an LLM-pasteable
// transcript. Each record contains a `message` envelope with role + content;
// content can be a string, an array of parts (text / tool_use / tool_result),
// or absent. Tool results are clipped at 8 KB to keep uploads sane.
func JSONLToMarkdown(r io.Reader, title string) string {
	var b strings.Builder
	if title != "" {
		fmt.Fprintf(&b, "# %s\n\n", title)
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1<<20), 1<<24) // session lines can be big
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		for _, c := range extractContent(rec) {
			text := strings.TrimSpace(c.text)
			if text == "" {
				continue
			}
			text = Sanitize(text)
			switch {
			case strings.HasSuffix(c.kind, ":tool_use") || strings.Contains(c.kind, ":tool_use:"):
				fmt.Fprintf(&b, "### %s\n```json\n%s\n```\n\n", c.kind, text)
			case strings.HasSuffix(c.kind, ":tool_result"):
				clip := text
				if len(clip) > 8000 {
					trimmed := len(clip) - 8000
					clip = clip[:8000]
					fmt.Fprintf(&b, "### %s\n```\n%s\n... (%d more chars truncated)\n```\n\n", c.kind, clip, trimmed)
				} else {
					fmt.Fprintf(&b, "### %s\n```\n%s\n```\n\n", c.kind, clip)
				}
			default:
				fmt.Fprintf(&b, "## %s\n%s\n\n", c.kind, text)
			}
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

type contentPart struct {
	kind string
	text string
}

func extractContent(rec map[string]any) []contentPart {
	m, ok := rec["message"].(map[string]any)
	if !ok {
		return nil
	}
	role, _ := m["role"].(string)
	if role == "" {
		role = "unknown"
	}
	switch c := m["content"].(type) {
	case string:
		return []contentPart{{role, c}}
	case []any:
		var out []contentPart
		for _, part := range c {
			p, ok := part.(map[string]any)
			if !ok {
				continue
			}
			t, _ := p["type"].(string)
			switch {
			case t == "text" || (t == "" && p["text"] != nil):
				txt, _ := p["text"].(string)
				out = append(out, contentPart{role, txt})
			case t == "tool_use":
				name, _ := p["name"].(string)
				if name == "" {
					name = "tool"
				}
				serial, _ := json.MarshalIndent(p["input"], "", "  ")
				out = append(out, contentPart{
					kind: role + ":tool_use:" + name,
					text: string(serial),
				})
			case t == "tool_result":
				switch tc := p["content"].(type) {
				case string:
					out = append(out, contentPart{role + ":tool_result", tc})
				case []any:
					for _, y := range tc {
						ym, ok := y.(map[string]any)
						if !ok {
							continue
						}
						if txt, _ := ym["text"].(string); txt != "" {
							out = append(out, contentPart{role + ":tool_result", txt})
						}
					}
				}
			}
		}
		return out
	}
	return nil
}
