package characteragent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"worldsim/internal/config"
	"worldsim/internal/fsutil"
	"worldsim/internal/llm"
)

type CallAudit struct {
	Owner            CognitionOwner  `json:"owner"`
	RequestID        string          `json:"request_id"`
	TurnID           string          `json:"turn_id"`
	InputHash        string          `json:"input_hash"`
	Model            config.ModelRef `json:"model"`
	Status           string          `json:"status"`
	Timestamp        string          `json:"timestamp"`
	DurationMS       int64           `json:"duration_ms"`
	PromptTokens     int             `json:"prompt_tokens"`
	CompletionTokens int             `json:"completion_tokens"`
	UsageEstimated   bool            `json:"usage_estimated"`
}

var providerGates sync.Map

type ModelRouter struct {
	registry  *AgentRegistry
	providers config.ProviderRegistry
}

func NewModelRouter(worldDir string, providers config.ProviderRegistry) (*ModelRouter, error) {
	if err := providers.Validate(); err != nil {
		return nil, errors.New("invalid Provider registry")
	}
	// Copy mutable maps; credentials live only in this private transport object.
	b, err := json.Marshal(providers)
	if err != nil {
		return nil, errors.New("invalid Provider snapshot")
	}
	var snapshot config.ProviderRegistry
	if json.Unmarshal(b, &snapshot) != nil {
		return nil, errors.New("invalid Provider snapshot")
	}
	r, err := NewRegistry(worldDir)
	if err != nil {
		return nil, err
	}
	return &ModelRouter{registry: r, providers: snapshot}, nil
}

func ResolveCharacterModel(c config.AgentRuntimeConfig, providers config.ProviderRegistry, id string) (ModelSpec, error) {
	ref, err := c.ResolveModel(id, providers)
	if err != nil {
		return ModelSpec{}, err
	}
	tier := c.Tiers[c.Characters[id].Importance]
	return normalizedSpec(ModelSpec{ModelRef: ref, MaxTokens: tier.MaxTokens, ContextBudgetTokens: tier.ContextBudgetTokens, Temperature: tier.Temperature})
}

func (r *ModelRouter) appendAudit(entry CallAudit) error {
	r.registry.mu.Lock()
	defer r.registry.mu.Unlock()
	s := &CognitionStore{registry: r.registry, owner: entry.Owner}
	if err := s.checkBindingLocked(); err != nil {
		return err
	}
	relative := filepath.Join("agents", entry.Owner.AgentID, "audit.jsonl")
	path, err := privatePath(r.registry.dir, relative)
	if err != nil {
		return err
	}
	if _, err := privatePath(r.registry.dir, relative+".tmp"); err != nil {
		return err
	}
	old, err := readPrivateFile(r.registry.dir, relative)
	if err != nil && !os.IsNotExist(err) {
		return errors.New("cannot read character call audit")
	}
	if len(old) > 0 && old[len(old)-1] != '\n' {
		return errors.New("character call audit has incomplete tail")
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return errors.New("cannot encode character audit")
	}
	if len(old)+len(line)+1 > 8<<20 {
		return errors.New("character call audit storage limit exceeded")
	}
	if err := fsutil.WriteFileAtomic(path, append(old, append(line, '\n')...)); err != nil {
		return errors.New("character call audit write failed")
	}
	return nil
}

func (r *ModelRouter) RecordRejection(request ModelRequest) error {
	return r.appendAudit(CallAudit{Owner: request.Owner, RequestID: request.RequestID, TurnID: request.TurnID, InputHash: request.InputHash, Model: request.Spec.ModelRef, Status: "cognition_rejected", Timestamp: time.Now().UTC().Format(time.RFC3339Nano)})
}

// Complete never retries, follows redirects, discovers models or retains chat history.
func (r *ModelRouter) Complete(ctx context.Context, request ModelRequest) (reply ModelReply, err error) {
	if request.Owner.WorldID == "" || request.RequestID != thoughtID(request.Owner, request.TurnID) || !validDigest(request.InputHash) {
		return reply, errors.New("invalid routed character request")
	}
	r.registry.mu.Lock()
	s := &CognitionStore{registry: r.registry, owner: request.Owner}
	bindingErr := s.checkBindingLocked()
	r.registry.mu.Unlock()
	if bindingErr != nil {
		return reply, bindingErr
	}
	entry := CallAudit{Owner: request.Owner, RequestID: request.RequestID, TurnID: request.TurnID, InputHash: request.InputHash, Model: request.Spec.ModelRef, Status: "failed", Timestamp: time.Now().UTC().Format(time.RFC3339Nano)}
	start := time.Now()
	defer func() {
		entry.DurationMS = time.Since(start).Milliseconds()
		entry.PromptTokens = reply.PromptTokens
		entry.CompletionTokens = reply.CompletionTokens
		entry.UsageEstimated = reply.UsageEstimated
		if ctx.Err() != nil {
			entry.Status = "cancelled"
		}
		if auditErr := r.appendAudit(entry); auditErr != nil {
			reply = ModelReply{}
			err = errors.New("character call audit failed; result not committed")
		}
	}()
	if err := request.Spec.ModelRef.Validate(r.providers); err != nil {
		return reply, errors.New("character model reference is unavailable")
	}
	spec, err := normalizedSpec(request.Spec)
	if err != nil {
		return reply, err
	}
	provider := r.providers.Providers[spec.ProviderID]
	limit := provider.MaxParallel
	if limit == 0 {
		limit = 1
	}
	key := privateHash([]byte(spec.ProviderID + "\x00" + provider.BaseURL + "\x00" + provider.APIKey + "\x00" + strconv.Itoa(limit)))
	gate, _ := providerGates.LoadOrStore(key, make(chan struct{}, limit))
	slots := gate.(chan struct{})
	seconds := provider.TimeoutSeconds
	if seconds == 0 {
		seconds = 120
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-callCtx.Done():
		entry.Status = "timeout"
		return reply, errors.New("character provider queue cancelled or timed out")
	}
	if callCtx.Err() != nil {
		return reply, errors.New("character provider request cancelled")
	}
	body := map[string]any{}
	if err := config.ValidateIsolatedExtraBody(provider.ExtraBody); err != nil {
		return reply, err
	}
	for k, v := range provider.ExtraBody {
		body[k] = v
	}
	if len(request.Messages) != 2 || request.Messages[0].Role != "system" || request.Messages[1].Role != "user" {
		return reply, errors.New("character messages must be a fresh system and user pair")
	}
	messageBytes, _ := json.Marshal(request.Messages)
	var local struct {
		ActorID string `json:"actor_id"`
		TurnID  string `json:"turn_id"`
	}
	if privateHash(messageBytes) != request.InputHash || json.Unmarshal([]byte(request.Messages[1].Content), &local) != nil || local.ActorID != request.Owner.CharacterID || local.TurnID != request.TurnID {
		return reply, errors.New("character context identity or input hash mismatch")
	}
	body["model"] = spec.Model
	body["messages"] = request.Messages
	body["stream"] = false
	body["max_tokens"] = spec.MaxTokens
	if spec.Temperature != nil {
		body["temperature"] = *spec.Temperature
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return reply, errors.New("invalid character provider parameters")
	}
	req, err := http.NewRequestWithContext(callCtx, "POST", llm.ResolveChatCompletionsURL(provider.BaseURL, provider.URLStrict), bytes.NewReader(encoded))
	if err != nil {
		return reply, errors.New("invalid provider endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	if provider.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		if callCtx.Err() != nil {
			entry.Status = "timeout"
		}
		return reply, errors.New("character provider request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return reply, errors.New("character provider returned unsuccessful status")
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if err != nil || len(b) > 1<<20 {
		return reply, errors.New("character provider response exceeds limit or is incomplete")
	}
	var output struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(b, &output) != nil || len(output.Choices) != 1 {
		return reply, errors.New("invalid character provider response")
	}
	reply.Content = output.Choices[0].Message.Content
	reply.FinishReason = output.Choices[0].FinishReason
	if output.Usage != nil {
		reply.PromptTokens = output.Usage.PromptTokens
		reply.CompletionTokens = output.Usage.CompletionTokens
	} else {
		reply.UsageEstimated = true
		for _, m := range request.Messages {
			reply.PromptTokens += 2 * utf8.RuneCountInString(m.Content)
		}
		reply.CompletionTokens = 2 * utf8.RuneCountInString(reply.Content)
	}
	if reply.PromptTokens < 0 || reply.CompletionTokens < 0 {
		return ModelReply{}, errors.New("invalid character provider token usage")
	}
	entry.Status = "received"
	if reply.FinishReason != "stop" {
		entry.Status = "incomplete"
	}
	return reply, nil
}
