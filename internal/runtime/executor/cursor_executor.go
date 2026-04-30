package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

type CursorExecutor struct {
	cfg *config.Config
}

func NewCursorExecutor(cfg *config.Config) *CursorExecutor {
	return &CursorExecutor{cfg: cfg}
}

func (e *CursorExecutor) Identifier() string { return "cursor" }

func (e *CursorExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	return nil
}

func (e *CursorExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("cursor executor does not support raw HTTP requests")
}

func (e *CursorExecutor) CountTokens(_ context.Context, _ *cliproxyauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, fmt.Errorf("cursor executor does not support token counting")
}

func (e *CursorExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return nil, fmt.Errorf("cursor executor: auth is nil")
	}
	return auth, nil
}

func (e *CursorExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	prompt := extractPromptFromPayload(req.Payload, opts.SourceFormat)
	if prompt == "" {
		return resp, fmt.Errorf("cursor executor: empty prompt")
	}

	cmd, errBuild := e.buildCommand(ctx, auth, baseModel, prompt)
	if errBuild != nil {
		return resp, errBuild
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, errRun := cmd.Output()
	if errRun != nil {
		return resp, fmt.Errorf("cursor executor: command failed: %w, stderr: %s", errRun, stderr.String())
	}

	text, usageInfo := parseCursorAgentOutput(output)

	if usageInfo.InputTokens > 0 || usageInfo.OutputTokens > 0 {
		reporter.Publish(ctx, usage.Detail{
			InputTokens:  int64(usageInfo.InputTokens),
			OutputTokens: int64(usageInfo.OutputTokens),
			CachedTokens: int64(usageInfo.CacheReadTokens),
		})
	}

	openaiResp := buildOpenAIChatResponse(req.Model, text, usageInfo)
	from := sdktranslator.FromString("openai")
	to := opts.SourceFormat
	if to == from {
		resp.Payload = openaiResp
	} else {
		var param any
		resp.Payload = sdktranslator.TranslateNonStream(ctx, from, to, req.Model, opts.OriginalRequest, req.Payload, openaiResp, &param)
	}
	return resp, nil
}

func (e *CursorExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	prompt := extractPromptFromPayload(req.Payload, opts.SourceFormat)
	if prompt == "" {
		return nil, fmt.Errorf("cursor executor: empty prompt")
	}

	cmd, errBuild := e.buildCommand(ctx, auth, baseModel, prompt)
	if errBuild != nil {
		return nil, errBuild
	}

	stdout, errPipe := cmd.StdoutPipe()
	if errPipe != nil {
		return nil, fmt.Errorf("cursor executor: stdout pipe: %w", errPipe)
	}
	if errStart := cmd.Start(); errStart != nil {
		return nil, fmt.Errorf("cursor executor: start: %w", errStart)
	}

	from := sdktranslator.FromString("openai")
	to := opts.SourceFormat

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			_ = cmd.Wait()
		}()

		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(nil, 1_048_576)
		chunkID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
		idx := 0

		for scanner.Scan() {
			line := scanner.Text()
			if strings.TrimSpace(line) == "" {
				continue
			}

			var evt cursorAgentEvent
			if errJSON := json.Unmarshal([]byte(line), &evt); errJSON != nil {
				continue
			}

			switch evt.Type {
			case "assistant":
				text := extractTextFromCursorMessage(evt.Message)
				if text == "" {
					continue
				}
				chunk := buildOpenAIStreamChunk(chunkID, req.Model, text, idx)
				idx++
				if to == from {
					out <- cliproxyexecutor.StreamChunk{Payload: chunk}
				} else {
					var param any
					chunks := sdktranslator.TranslateStream(ctx, from, to, req.Model, opts.OriginalRequest, req.Payload, chunk, &param)
					for i := range chunks {
						out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}
					}
				}
			case "result":
				if evt.Usage != nil {
					reporter.Publish(ctx, usage.Detail{
						InputTokens:  int64(evt.Usage.InputTokens),
						OutputTokens: int64(evt.Usage.OutputTokens),
						CachedTokens: int64(evt.Usage.CacheReadTokens),
					})
				}
			}
		}

		donePayload := []byte("data: [DONE]\n\n")
		if to == from {
			out <- cliproxyexecutor.StreamChunk{Payload: donePayload}
		} else {
			var param any
			chunks := sdktranslator.TranslateStream(ctx, from, to, req.Model, opts.OriginalRequest, req.Payload, []byte("[DONE]"), &param)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}
			}
		}
	}()

	return &cliproxyexecutor.StreamResult{Chunks: out}, nil
}

func (e *CursorExecutor) buildCommand(ctx context.Context, auth *cliproxyauth.Auth, model, prompt string) (*exec.Cmd, error) {
	bin := "cursor-agent"

	ensureCursorAgentAuth(auth)

	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--workspace", "/tmp",
		"--trust",
		"--force",
	}

	if model != "" {
		args = append(args, "--model", model)
	}

	args = append(args, prompt)
	return exec.CommandContext(ctx, bin, args...), nil
}

var cursorAuthOnce sync.Once

// ensureCursorAgentAuth writes the OAuth access/refresh tokens from the proxy's
// auth store into cursor-agent's native auth.json so that cursor-agent can
// authenticate without a separate interactive login.
func ensureCursorAgentAuth(auth *cliproxyauth.Auth) {
	if auth == nil || auth.Metadata == nil {
		return
	}
	accessToken, _ := auth.Metadata["access_token"].(string)
	if strings.TrimSpace(accessToken) == "" {
		return
	}
	refreshToken, _ := auth.Metadata["refresh_token"].(string)

	cursorAuthOnce.Do(func() {
		authPath := cursorAgentAuthFilePath()
		if _, err := os.Stat(authPath); err == nil {
			return
		}

		dir := filepath.Dir(authPath)
		if err := os.MkdirAll(dir, 0700); err != nil {
			log.Errorf("cursor executor: failed to create auth dir %s: %v", dir, err)
			return
		}

		data := map[string]any{
			"accessToken":        accessToken,
			"refreshToken":       refreshToken,
			"apiKey":             nil,
			"bedrockCredentials": nil,
		}
		content, err := json.MarshalIndent(data, "", "  ")
		if err != nil {
			log.Errorf("cursor executor: failed to marshal auth data: %v", err)
			return
		}
		if err := os.WriteFile(authPath, content, 0600); err != nil {
			log.Errorf("cursor executor: failed to write auth file %s: %v", authPath, err)
			return
		}
		log.Infof("cursor executor: wrote auth credentials to %s", authPath)
	})
}

func cursorAgentAuthFilePath() string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, ".cursor", "auth.json")
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			appData = filepath.Join(home, "AppData", "Roaming")
		}
		return filepath.Join(appData, "Cursor", "auth.json")
	default:
		configDir := os.Getenv("XDG_CONFIG_HOME")
		if configDir == "" {
			configDir = filepath.Join(home, ".config")
		}
		return filepath.Join(configDir, "cursor", "auth.json")
	}
}

type cursorAgentEvent struct {
	Type    string            `json:"type"`
	Subtype string            `json:"subtype,omitempty"`
	Message json.RawMessage   `json:"message,omitempty"`
	Result  string            `json:"result,omitempty"`
	Usage   *cursorAgentUsage `json:"usage,omitempty"`
}

type cursorAgentUsage struct {
	InputTokens      int `json:"inputTokens"`
	OutputTokens     int `json:"outputTokens"`
	CacheReadTokens  int `json:"cacheReadTokens"`
	CacheWriteTokens int `json:"cacheWriteTokens"`
}

func extractTextFromCursorMessage(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return ""
	}

	// content can be a string
	var textStr string
	if err := json.Unmarshal(msg.Content, &textStr); err == nil {
		return textStr
	}

	// or an array of {type, text}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(msg.Content, &parts); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

func extractPromptFromPayload(payload []byte, format sdktranslator.Format) string {
	if len(payload) == 0 {
		return ""
	}

	to := sdktranslator.FromString("openai")
	var translated []byte
	if format == to {
		translated = payload
	} else {
		translated = sdktranslator.TranslateRequest(format, to, "", bytes.Clone(payload), false)
	}

	messages := gjson.GetBytes(translated, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return ""
	}

	var parts []string
	for _, msg := range messages.Array() {
		role := msg.Get("role").String()
		content := msg.Get("content").String()
		if content == "" {
			continue
		}
		if role == "system" || role == "user" {
			parts = append(parts, content)
		}
	}

	if len(parts) == 0 {
		last := messages.Array()
		if len(last) > 0 {
			return last[len(last)-1].Get("content").String()
		}
		return ""
	}
	return strings.Join(parts, "\n\n")
}

func parseCursorAgentOutput(output []byte) (string, cursorAgentUsage) {
	var text string
	var usage cursorAgentUsage
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		var evt cursorAgentEvent
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}
		switch evt.Type {
		case "assistant":
			t := extractTextFromCursorMessage(evt.Message)
			if t != "" {
				text = t
			}
		case "result":
			if evt.Result != "" {
				text = evt.Result
			}
			if evt.Usage != nil {
				usage = *evt.Usage
			}
		}
	}
	return text, usage
}

func buildOpenAIChatResponse(model, content string, usage cursorAgentUsage) []byte {
	resp := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": content,
				},
				"finish_reason": "stop",
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     usage.InputTokens,
			"completion_tokens": usage.OutputTokens,
			"total_tokens":      usage.InputTokens + usage.OutputTokens,
		},
	}
	data, err := json.Marshal(resp)
	if err != nil {
		log.Errorf("cursor executor: failed to marshal response: %v", err)
		return nil
	}
	return data
}

func buildOpenAIStreamChunk(id, model, content string, idx int) []byte {
	chunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{
			{
				"index": idx,
				"delta": map[string]any{
					"content": content,
				},
				"finish_reason": nil,
			},
		},
	}
	data, err := json.Marshal(chunk)
	if err != nil {
		return nil
	}
	return append([]byte("data: "), append(data, []byte("\n\n")...)...)
}
