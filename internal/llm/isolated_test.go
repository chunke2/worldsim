package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"worldsim/internal/config"
)

func TestIsolatedGenerationStrictTransport(t *testing.T) {
	for _, mode := range []string{"normal", "reasoning_only", "incomplete", "remote_error", "invalid", "tools", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			var redirected, calls atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Authorization") != "Bearer API-PRIVATE-CANARY" {
					t.Error("incorrect auth")
				}
				var body map[string]json.RawMessage
				if json.NewDecoder(r.Body).Decode(&body) != nil {
					t.Error("invalid body")
				}
				var messages []Message
				_ = json.Unmarshal(body["messages"], &messages)
				if len(messages) != 2 || messages[0].Role != "system" || messages[1].Role != "user" || messages[1].Content != "POV-LOCAL-FACT" {
					t.Error("fresh message boundary lost")
				}
				if string(body["enable_thinking"]) != "false" || string(body["max_tokens"]) != "2048" || string(body["stream"]) != "false" {
					t.Error("generation settings lost")
				}
				if _, ok := body["tools"]; ok {
					t.Error("tools attached")
				}
				switch mode {
				case "remote_error":
					http.Error(w, "API-PRIVATE-CANARY", 500)
				case "invalid":
					fmt.Fprint(w, "not JSON: API-PRIVATE-CANARY")
				case "tools":
					fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"content":"unexpected","tool_calls":[{"id":"external"}]}}]}`)
				case "redirect":
					http.Redirect(w, r, target.URL, http.StatusFound)
				default:
					content, reason := "VISIBLE-PROSE", "stop"
					if mode == "reasoning_only" {
						content = ""
					}
					if mode == "incomplete" {
						reason = "length"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": reason, "message": map[string]any{"content": content, "reasoning_content": "RAW-REASONING-PRIVATE-CANARY"}}}})
				}
			}))
			defer server.Close()
			cfg := &config.APIConfig{BaseURL: server.URL, Model: "writer", APIKey: "API-PRIVATE-CANARY", MaxTokens: 2048, ExtraBody: map[string]any{"enable_thinking": false}}
			result, err := CallIsolatedSync(context.Background(), cfg, "SYSTEM", "POV-LOCAL-FACT")
			succeeds := mode == "normal" || mode == "incomplete"
			if succeeds != (err == nil) {
				t.Fatal("unexpected strict result", err)
			}
			if result.ReasoningContent != "" || strings.Contains(fmt.Sprint(err), "PRIVATE-CANARY") || strings.Contains(result.Content, "RAW-REASONING") {
				t.Fatal("reasoning/credentials leaked")
			}
			if mode == "incomplete" && result.FinishReason != "length" {
				t.Fatal("finish reason lost")
			}
			if calls.Load() != 1 || redirected.Load() != 0 {
				t.Fatal("retried or followed redirect")
			}
		})
	}
}

func TestIsolatedGenerationRejectsRequestAndHistoryOverridesBeforeHTTP(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	for _, key := range []string{"messages", "model", "max_tokens", "tools", "previous_response_id", "conversation", "thread_id", "session_id", "instructions", "memory"} {
		cfg := &config.APIConfig{BaseURL: server.URL, Model: "writer", MaxTokens: 2048, ExtraBody: map[string]any{key: "SHARED-PRIVATE-CANARY"}}
		if _, err := CallIsolatedSync(context.Background(), cfg, "system", "local"); err == nil || strings.Contains(err.Error(), "PRIVATE-CANARY") {
			t.Fatal("unsafe option accepted or leaked")
		}
		if _, ok := cfg.ExtraBody[key]; !ok {
			t.Fatal("validation rewrote saved configuration")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("unsafe request reached HTTP")
	}
}
