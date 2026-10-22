package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"worldsim/internal/config"
)

// CallIsolatedSync is the independent writer/reviewer boundary: a fresh pair,
// one HTTP attempt, no tools/history/redirects or reasoning-as-content fallback.
func CallIsolatedSync(ctx context.Context, cfg *config.APIConfig, system, user string) (result CompletionResult, err error) {
	if cfg == nil || strings.TrimSpace(cfg.Model) == "" || cfg.MaxTokens <= 0 {
		return result, errors.New("isolated generation configuration is incomplete")
	}
	if err := config.ValidateIsolatedExtraBody(cfg.ExtraBody); err != nil {
		return result, err
	}
	endpoint, parseErr := url.Parse(normalizeURL(cfg))
	if parseErr != nil || endpoint == nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return result, errors.New("invalid isolated generation endpoint")
	}
	seconds := cfg.HTTPTimeoutSeconds
	if seconds == 0 {
		seconds = 120
	}
	if seconds < 1 || seconds > 3600 {
		return result, errors.New("isolated generation timeout must be within 1..3600 seconds")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	select {
	case llmSem <- struct{}{}:
		defer func() { <-llmSem }()
	case <-ctx.Done():
		return result, ctx.Err()
	}
	messages := []Message{{Role: "system", Content: system}, {Role: "user", Content: user}}
	var usage *tokenUsage
	ctx = WithStart(ctx)
	defer func() {
		RecordSpan(ctx, cfg.Model, usage, countMessageRunes(messages), utf8.RuneCountInString(result.Content), err)
	}()
	body := map[string]any{"model": cfg.Model, "messages": messages, "stream": false, "max_tokens": cfg.MaxTokens}
	for key, value := range cfg.ExtraBody {
		body[key] = value
	}
	encoded, marshalErr := json.Marshal(body)
	if marshalErr != nil {
		return result, errors.New("invalid isolated generation parameters")
	}
	req, requestErr := http.NewRequestWithContext(ctx, "POST", endpoint.String(), bytes.NewReader(encoded))
	if requestErr != nil {
		return result, errors.New("invalid isolated generation request")
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	// Clone the shared transport's client before changing redirect policy.
	client := *llmHTTPClient(cfg, time.Duration(seconds)*time.Second)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, requestErr := client.Do(req)
	if requestErr != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, errors.New("isolated generation request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, fmt.Errorf("isolated generation returned HTTP %d", response.StatusCode)
	}
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if readErr != nil || len(raw) > 4<<20 {
		return result, errors.New("isolated generation response is incomplete or exceeds limit")
	}
	var output struct {
		Choices []struct {
			Message struct {
				Content   string            `json:"content"`
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *tokenUsage `json:"usage"`
	}
	if json.Unmarshal(raw, &output) != nil || len(output.Choices) != 1 || len(output.Choices[0].Message.ToolCalls) > 0 {
		return result, errors.New("invalid isolated generation response")
	}
	usage = output.Usage
	if strings.TrimSpace(output.Choices[0].Message.Content) == "" {
		return result, errors.New("isolated generation returned empty content; reasoning is not prose or review evidence")
	}
	result.Content = output.Choices[0].Message.Content
	result.FinishReason = output.Choices[0].FinishReason
	return result, nil
}
