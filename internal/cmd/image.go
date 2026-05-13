package cmd

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	codexResponsesURL = "https://chatgpt.com/backend-api/codex/responses"
	codexTokenURL     = "https://auth.openai.com/oauth/token"
	codexClientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexDefaultModel = "gpt-5.4-mini"
	codexExpBuffer    = 300 // refresh 5 min before exp
)

func codexAuthPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "auth.json")
}

func imageCmd() *cobra.Command {
	var out, editPath, aspect, size, model, quality string
	var fast, noOpen bool
	cmd := &cobra.Command{
		Use:   "image <prompt>",
		Short: "Generate or edit an image via your ChatGPT Plus/Pro subscription",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			q := strings.ToLower(quality)
			if q == "" && fast {
				q = "low"
			}
			return runImage(args[0], out, editPath, aspect, strings.ToUpper(size), model, q, !noOpen)
		},
	}
	cmd.Flags().StringVarP(&out, "out", "o", "", "Output path (default: ~/Downloads/cass-img-<ts>.png)")
	cmd.Flags().StringVarP(&editPath, "edit", "e", "", "Input image to edit (runs edit mode)")
	cmd.Flags().StringVarP(&aspect, "aspect", "a", "", "Aspect ratio hint (e.g. 16:9, 1:1, 3:4)")
	cmd.Flags().StringVarP(&size, "size", "s", "", "Target resolution hint (1K | 2K | 4K)")
	cmd.Flags().StringVarP(&model, "model", "m", codexDefaultModel, "Agent model")
	cmd.Flags().BoolVarP(&fast, "fast", "f", false, "Shorthand for --quality low")
	cmd.Flags().StringVarP(&quality, "quality", "q", "", "Render quality (low | medium | high | auto)")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "Don't open the generated image after saving")
	return cmd
}

func runImage(prompt, outPath, editPath, aspect, size, model, quality string, openAfter bool) error {
	auth, err := loadCodexCreds()
	if err != nil {
		return err
	}
	if err := refreshCodexIfNeeded(auth); err != nil {
		return err
	}

	var inputImage []byte
	inputMime := "image/png"
	if editPath != "" {
		data, err := os.ReadFile(editPath)
		if err != nil {
			return err
		}
		inputImage = data
		switch strings.ToLower(filepath.Ext(editPath)) {
		case ".jpg", ".jpeg":
			inputMime = "image/jpeg"
		case ".webp":
			inputMime = "image/webp"
		case ".gif":
			inputMime = "image/gif"
		}
	}

	tokens, _ := auth["tokens"].(map[string]any)
	accessToken, _ := tokens["access_token"].(string)
	accountID, _ := tokens["account_id"].(string)
	if accountID == "" {
		accountID, _ = auth["account_id"].(string)
	}

	content := []map[string]any{
		{"type": "input_text", "text": decoratePrompt(prompt, size, aspect)},
	}
	instructions := "You are an image generation assistant. Generate exactly one image matching the user's prompt by calling the image_generation tool."
	if inputImage != nil {
		dataURL := fmt.Sprintf("data:%s;base64,%s", inputMime, base64.StdEncoding.EncodeToString(inputImage))
		content = append(content, map[string]any{"type": "input_image", "image_url": dataURL})
		instructions = "You are an image editing assistant. The user provides an input image and instructions to modify it. Call the image_generation tool to produce exactly one edited image."
	}

	imageTool := map[string]any{"type": "image_generation", "output_format": "png"}
	if quality != "" {
		imageTool["quality"] = quality
	}
	body := map[string]any{
		"model":        model,
		"instructions": instructions,
		"input": []map[string]any{{
			"type": "message", "role": "user", "content": content,
		}},
		"tools":       []any{imageTool},
		"tool_choice": map[string]string{"type": "image_generation"},
		"store":       false,
		"stream":      true,
	}
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", codexResponsesURL, bytes.NewReader(buf))
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("ChatGPT-Account-ID", accountID)
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("originator", "cass")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	fmt.Fprintln(os.Stderr, "Generating...")
	httpc := &http.Client{Timeout: 180 * time.Second}
	resp, err := httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("responses API HTTP %d: %s", resp.StatusCode, string(errBody))
	}

	pngBytes, revisedPrompt, err := consumeImageSSE(resp.Body)
	if err != nil {
		return err
	}

	finalOut := outPath
	if finalOut == "" {
		finalOut = defaultImageOutPath()
	}
	if err := os.MkdirAll(filepath.Dir(finalOut), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(finalOut, pngBytes, 0o644); err != nil {
		return err
	}
	fmt.Println(finalOut)
	if revisedPrompt != "" {
		fmt.Fprintf(os.Stderr, "(revised prompt: %s)\n", revisedPrompt)
	}
	if openAfter {
		openFile(finalOut)
	}
	return nil
}

// consumeImageSSE reads the SSE stream and returns the first image_generation_call result.
func consumeImageSSE(body io.Reader) (png []byte, revisedPrompt string, err error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1<<20), 1<<24)
	var event string = "message"
	var dataLines []string
	flush := func() (bool, error) {
		if len(dataLines) == 0 {
			event = "message"
			return false, nil
		}
		data := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		thisEvent := event
		event = "message"
		if thisEvent != "response.output_item.done" {
			return false, nil
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(data), &obj); err != nil {
			return false, nil
		}
		item, _ := obj["item"].(map[string]any)
		if item == nil || item["type"] != "image_generation_call" {
			return false, nil
		}
		b64, _ := item["result"].(string)
		if b64 == "" {
			return false, nil
		}
		decoded, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return false, fmt.Errorf("decode image result: %w", err)
		}
		png = decoded
		revisedPrompt, _ = item["revised_prompt"].(string)
		return true, nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			done, err := flush()
			if err != nil {
				return nil, "", err
			}
			if done {
				return png, revisedPrompt, nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimLeft(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	return nil, "", fmt.Errorf("stream ended without an image_generation_call result")
}

func decoratePrompt(prompt, size, aspect string) string {
	var hints []string
	switch size {
	case "1K":
		hints = append(hints, "Target resolution: approximately 1024x1024 pixels (1K).")
	case "2K":
		hints = append(hints, "Target resolution: approximately 2048x2048 pixels (2K).")
	case "4K":
		hints = append(hints, "Target resolution: approximately 4096x4096 pixels (4K).")
	}
	if aspect != "" {
		hints = append(hints, "Aspect ratio: "+aspect+".")
	}
	if len(hints) == 0 {
		return prompt
	}
	return prompt + "\n\n" + strings.Join(hints, " ")
}

func loadCodexCreds() (map[string]any, error) {
	data, err := os.ReadFile(codexAuthPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no Codex login at %s — run `codex login` (ChatGPT Plus/Pro required)", codexAuthPath())
		}
		return nil, err
	}
	var auth map[string]any
	if err := json.Unmarshal(data, &auth); err != nil {
		return nil, fmt.Errorf("malformed %s: %w", codexAuthPath(), err)
	}
	mode, _ := auth["auth_mode"].(string)
	if mode != "chatgpt" {
		return nil, fmt.Errorf("codex auth_mode is %q; need 'chatgpt'. Run `codex logout && codex login`", mode)
	}
	tokens, _ := auth["tokens"].(map[string]any)
	if tokens == nil || tokens["access_token"] == "" || tokens["refresh_token"] == "" {
		return nil, fmt.Errorf("malformed %s: missing access_token or refresh_token", codexAuthPath())
	}
	return auth, nil
}

func saveCodexCreds(auth map[string]any) error {
	auth["last_refresh"] = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	data, err := json.MarshalIndent(auth, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(codexAuthPath(), data, 0o600)
}

func refreshCodexIfNeeded(auth map[string]any) error {
	tokens, _ := auth["tokens"].(map[string]any)
	accessToken, _ := tokens["access_token"].(string)
	exp, err := jwtExp(accessToken)
	if err != nil {
		return err
	}
	if time.Now().Unix() < exp-codexExpBuffer {
		return nil
	}
	refreshToken, _ := tokens["refresh_token"].(string)
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {codexClientID},
		"refresh_token": {refreshToken},
		"scope":         {"openid profile email offline_access"},
	}
	req, _ := http.NewRequest("POST", codexTokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("codex token refresh HTTP %d: %s", resp.StatusCode, string(body))
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return err
	}
	if v, ok := body["access_token"].(string); ok {
		tokens["access_token"] = v
	}
	if v, ok := body["refresh_token"].(string); ok && v != "" {
		tokens["refresh_token"] = v
	}
	if v, ok := body["id_token"].(string); ok && v != "" {
		tokens["id_token"] = v
	}
	return saveCodexCreds(auth)
}

func jwtExp(token string) (int64, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0, fmt.Errorf("not a JWT")
	}
	pad := (4 - len(parts[1])%4) % 4
	decoded, err := base64.URLEncoding.DecodeString(parts[1] + strings.Repeat("=", pad))
	if err != nil {
		// Try without padding using RawURLEncoding
		decoded, err = base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			return 0, err
		}
	}
	var payload struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(decoded, &payload); err != nil {
		return 0, err
	}
	return payload.Exp, nil
}

func defaultImageOutPath() string {
	home, _ := os.UserHomeDir()
	stamp := time.Now().Format("20060102-150405")
	return filepath.Join(home, "Downloads", "cass-img-"+stamp+".png")
}

func openFile(path string) {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", path)
	case "linux":
		c = exec.Command("xdg-open", path)
	case "windows":
		c = exec.Command("cmd", "/c", "start", "", path)
	}
	if c != nil {
		_ = c.Start()
	}
}
