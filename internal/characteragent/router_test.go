package characteragent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"worldsim/internal/config"
)

func TestRouterFivePrivateCanariesAtActualHTTPBoundary(t *testing.T) {
	dir, spec := thoughtFixture(t)
	var mu sync.Mutex
	seen := map[string]string{}
	server := func(provider, model, key string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Model     string         `json:"model"`
				Messages  []AgentMessage `json:"messages"`
				MaxTokens int            `json:"max_tokens"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || r.URL.Path != "/v1/chat/completions" || body.Model != model || r.Header.Get("Authorization") != "Bearer "+key || body.MaxTokens != 1024 {
				t.Error("incorrect isolated HTTP routing")
			}
			var local characterContext
			if len(body.Messages) != 2 || json.Unmarshal([]byte(body.Messages[1].Content), &local) != nil {
				t.Error("invalid HTTP context")
				w.WriteHeader(500)
				return
			}
			payload, _ := json.Marshal(body.Messages)
			for id := range fiveCharacters().Entities {
				if bytes.Contains(payload, []byte(id+"-PRIVATE-CANARY")) != (id == local.ActorID) {
					t.Error("unrevealed private canary crossed actual request boundary")
				}
			}
			mu.Lock()
			seen[local.ActorID] = provider
			mu.Unlock()
			reply := mockThought(ModelRequest{Owner: CognitionOwner{CharacterID: local.ActorID}, TurnID: local.TurnID})
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": reply.Content, "reasoning_content": "RAW-REASONING-PRIVATE-CANARY"}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 100, "completion_tokens": 30}})
		}))
	}
	a := server("a", "strong", "A-API-PRIVATE-CANARY")
	defer a.Close()
	b := server("b", "cheap", "B-API-PRIVATE-CANARY")
	defer b.Close()
	providers := config.ProviderRegistry{DefaultProvider: "a", Providers: map[string]config.Provider{
		"a": {Name: "A", Protocol: "openai_compatible", BaseURL: a.URL + "/v1", APIKey: "A-API-PRIVATE-CANARY", Models: []string{"strong"}, MaxParallel: 2},
		"b": {Name: "B", Protocol: "openai_compatible", BaseURL: b.URL + "/v1", APIKey: "B-API-PRIVATE-CANARY", Models: []string{"cheap"}, MaxParallel: 1},
	}}
	configuration := config.AgentRuntimeConfig{SchemaVersion: 1, Tiers: map[config.AgentImportance]config.TierPolicy{}, Characters: map[string]config.CharacterAgentConfig{}}
	for _, tier := range []config.AgentImportance{config.ImportanceCore, config.ImportanceSupporting, config.ImportanceBackground} {
		configuration.Tiers[tier] = config.TierPolicy{ModelRef: config.ModelRef{ProviderID: "b", Model: "cheap"}}
	}
	for id := range fiveCharacters().Entities {
		configuration.Characters[id] = config.CharacterAgentConfig{Importance: config.ImportanceSupporting}
	}
	configuration.Characters["沈砚"] = config.CharacterAgentConfig{Importance: config.ImportanceCore, ModelOverride: &config.ModelRef{ProviderID: "a", Model: "strong"}}
	router, err := NewModelRouter(dir, providers)
	if err != nil {
		t.Fatal(err)
	}
	for id := range fiveCharacters().Entities {
		spec, err = ResolveCharacterModel(configuration, providers, id)
		if err != nil {
			t.Fatal(err)
		}
		agent, _ := NewCharacterAgent(dir, id, router)
		result, err := agent.Think(context.Background(), "five-actors", spec)
		if err != nil || result.Receipt.PromptTokens != 100 || result.Receipt.UsageEstimated {
			t.Fatal("HTTP character call failed", err)
		}
		for _, file := range []string{agent.store.path(), filepath.Join(dir, "agents", agent.store.Owner().AgentID, "audit.jsonl")} {
			data := mustCognitionBytes(t, file)
			for _, secret := range []string{"A-API-PRIVATE-CANARY", "B-API-PRIVATE-CANARY", "RAW-REASONING-PRIVATE-CANARY"} {
				if bytes.Contains(data, []byte(secret)) {
					t.Fatal("credentials or reasoning persisted")
				}
			}
		}
	}
	if len(seen) != 5 || seen["沈砚"] != "a" || seen["罗绮"] != "b" {
		t.Fatal("configured model override was not routed")
	}
}

func TestRouterProviderConcurrencyAndCancellation(t *testing.T) {
	dir, _ := thoughtFixture(t)
	var active, peak atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{}"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()
	p := config.ProviderRegistry{Providers: map[string]config.Provider{"p": {Name: "P", Protocol: "openai_compatible", BaseURL: server.URL, MaxParallel: 1}}}
	router, _ := NewModelRouter(dir, p)
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for id := range fiveCharacters().Entities {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			s, _ := NewCognitionStore(dir, id)
			state, _ := s.Read()
			spec, _ := normalizedSpec(ModelSpec{ModelRef: config.ModelRef{ProviderID: "p", Model: "m"}})
			req, _, err := buildCharacterRequest(state, "limit", spec)
			if err == nil {
				_, err = router.Complete(context.Background(), req)
			}
			errs <- err
		}(id)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if peak.Load() != 1 {
		t.Fatal("provider limit not enforced")
	}
	s, _ := NewCognitionStore(dir, "沈砚")
	state, _ := s.Read()
	spec, _ := normalizedSpec(ModelSpec{ModelRef: config.ModelRef{ProviderID: "p", Model: "m"}})
	req, _, _ := buildCharacterRequest(state, "cancel", spec)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := router.Complete(ctx, req); err == nil {
		t.Fatal("cancelled request succeeded")
	}
}

func TestRouterRejectsOverridesAndRedactsRemoteErrors(t *testing.T) {
	dir, spec := thoughtFixture(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "API-PRIVATE-CANARY", 500) }))
	defer server.Close()
	for _, extra := range []map[string]any{nil, {"messages": []any{"OTHER-PRIVATE-CANARY"}}, {"model": "wrong"}, {"tools": []any{}}, {"previous_response_id": "shared"}, {"conversation_id": "shared"}, {"thread_id": "shared"}, {"session_id": "shared"}} {
		providers := config.ProviderRegistry{Providers: map[string]config.Provider{"p": {Name: "P", Protocol: "openai_compatible", BaseURL: server.URL, APIKey: "API-PRIVATE-CANARY", ExtraBody: extra}}}
		router, _ := NewModelRouter(dir, providers)
		a, _ := NewCharacterAgent(dir, "沈砚", router)
		before := calls.Load()
		if _, err := a.Think(context.Background(), "fail", spec); err == nil || bytes.Contains([]byte(err.Error()), []byte("PRIVATE-CANARY")) {
			t.Fatal("unsafe request or remote error accepted")
		}
		if extra != nil && calls.Load() != before {
			t.Fatal("protected extra_body field reached HTTP")
		}
		data := mustCognitionBytes(t, filepath.Join(dir, "agents", a.store.Owner().AgentID, "audit.jsonl"))
		if bytes.Contains(data, []byte("PRIVATE-CANARY")) {
			t.Fatal("audit leaked remote secrets")
		}
		state, _ := a.store.Read()
		if len(state.Thoughts) != 0 {
			t.Fatal("provider failure became a character decision")
		}
	}
	// Audit persistence failure blocks a usable result, rather than silently succeeding.
	s, _ := NewCognitionStore(dir, "顾闻")
	path := filepath.Join(dir, "agents", s.Owner().AgentID, "audit.jsonl.tmp")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	router, _ := NewModelRouter(dir, config.ProviderRegistry{Providers: map[string]config.Provider{"p": {Name: "P", Protocol: "openai_compatible", BaseURL: server.URL}}})
	a, _ := NewCharacterAgent(dir, "顾闻", router)
	if _, err := a.Think(context.Background(), "audit-fail", spec); err == nil {
		t.Fatal("audit failure ignored")
	}
}
