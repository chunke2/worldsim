package characteragent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"worldsim/internal/config"
)

func thoughtFixture(t *testing.T) (string, ModelSpec) {
	t.Helper()
	dir, _, _ := cognitionFixture(t)
	for id := range fiveCharacters().Entities {
		s, _ := NewCognitionStore(dir, id)
		o := Observation{ID: "self", Recipient: s.Owner(), Kind: "profile", Text: id + "-PRIVATE-CANARY", SourceID: "explicit-self-profile"}
		if _, err := s.Append(0, []Observation{o}, nil); err != nil {
			t.Fatal(err)
		}
	}
	return dir, ModelSpec{ModelRef: config.ModelRef{ProviderID: "p", Model: "model"}}
}

func mockThought(request ModelRequest) ModelReply {
	body := map[string]any{"actor_id": request.Owner.CharacterID, "turn_id": request.TurnID, "updates": []map[string]any{{"key": "next", "kind": "goal", "text": "保住自己的筹码", "sources": []string{"self"}, "status": "active"}}}
	b, _ := json.Marshal(body)
	return ModelReply{Content: string(b), FinishReason: "stop", PromptTokens: 100, CompletionTokens: 30}
}

func TestCharacterAgentFiveIndependentRequestsAndIdempotentTurns(t *testing.T) {
	dir, spec := thoughtFixture(t)
	calls := 0
	client := CompletionFunc(func(ctx context.Context, r ModelRequest) (ModelReply, error) {
		calls++
		if len(r.Messages) != 2 || r.Messages[0].Role != "system" || r.Messages[1].Role != "user" {
			t.Fatal("shared conversation history")
		}
		encoded, _ := json.Marshal(r.Messages)
		for id := range fiveCharacters().Entities {
			if got := bytes.Contains(encoded, []byte(id+"-PRIVATE-CANARY")); got != (id == r.Owner.CharacterID) {
				t.Fatal("private context crossed character boundary")
			}
		}
		if strings.Contains(string(encoded), "source_id") || strings.Contains(string(encoded), "world_id") {
			t.Fatal("private storage metadata entered prompt")
		}
		return mockThought(r), nil
	})
	for id := range fiveCharacters().Entities {
		a, err := NewCharacterAgent(dir, id, client)
		if err != nil {
			t.Fatal(err)
		}
		result, err := a.Think(context.Background(), "turn-1", spec)
		if err != nil || result.AlreadyCompleted {
			t.Fatal("independent request failed", err)
		}
		saved, err := a.store.Read()
		if err != nil || len(saved.Records) != 1 || len(saved.Thoughts) != 1 || saved.Records[0].Owner != a.store.Owner() {
			t.Fatal("invalid actor-local commit", err)
		}
		before := mustCognitionBytes(t, a.store.path())
		again, err := a.Think(context.Background(), "turn-1", spec)
		if err != nil || !again.AlreadyCompleted || again.Receipt.RequestID != result.Receipt.RequestID {
			t.Fatal("turn replay not cached", err)
		}
		if !bytes.Equal(before, mustCognitionBytes(t, a.store.path())) {
			t.Fatal("replay changed cognition")
		}
	}
	if calls != 5 {
		t.Fatal("expected five separate calls, no repeated-turn calls")
	}
}

func TestCharacterAgentRejectsBadOutputWithoutCommit(t *testing.T) {
	for _, mode := range []string{"truncated", "reasoning", "wrong-actor", "wrong-turn", "missing-updates", "foreign-source", "unknown-kind", "duplicate-json", "reasoning-field"} {
		t.Run(mode, func(t *testing.T) {
			dir, spec := thoughtFixture(t)
			a, _ := NewCharacterAgent(dir, "沈砚", CompletionFunc(func(_ context.Context, r ModelRequest) (ModelReply, error) {
				reply := mockThought(r)
				switch mode {
				case "truncated":
					reply.FinishReason = "length"
				case "reasoning":
					reply.Content = `{"reasoning":"RAW-PRIVATE-CANARY"}`
				case "wrong-actor":
					reply.Content = strings.Replace(reply.Content, "沈砚", "罗绮", 1)
				case "wrong-turn":
					reply.Content = strings.Replace(reply.Content, "turn-1", "other", 1)
				case "missing-updates":
					reply.Content = `{"actor_id":"沈砚","turn_id":"turn-1"}`
				case "foreign-source":
					reply.Content = strings.Replace(reply.Content, `"self"`, `"another-private-source"`, 1)
				case "unknown-kind":
					reply.Content = strings.Replace(reply.Content, `"goal"`, `"world_fact"`, 1)
				case "duplicate-json":
					reply.Content = `{"actor_id":"罗绮",` + reply.Content[1:]
				case "reasoning-field":
					reply.Content = `{"reasoning":"RAW-PRIVATE-CANARY",` + reply.Content[1:]
				}
				return reply, nil
			}))
			before := mustCognitionBytes(t, a.store.path())
			if _, err := a.Think(context.Background(), "turn-1", spec); err == nil || strings.Contains(err.Error(), "PRIVATE-CANARY") {
				t.Fatal("bad output accepted or private output echoed")
			}
			if !bytes.Equal(before, mustCognitionBytes(t, a.store.path())) {
				t.Fatal("bad output committed")
			}
		})
	}
}

func TestCharacterAgentSingleFlightAndStaleResponse(t *testing.T) {
	dir, spec := thoughtFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	a, _ := NewCharacterAgent(dir, "沈砚", CompletionFunc(func(_ context.Context, r ModelRequest) (ModelReply, error) {
		close(started)
		<-release
		return mockThought(r), nil
	}))
	go func() { _, err := a.Think(context.Background(), "turn-1", spec); finished <- err }()
	<-started
	if _, err := a.Think(context.Background(), "turn-1", spec); err == nil {
		t.Fatal("concurrent character request accepted")
	}
	if _, err := a.store.Append(1, []Observation{{ID: "new-self", Recipient: a.store.Owner(), Kind: "profile", Text: "新增自身资料", SourceID: "explicit-profile"}}, nil); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-finished; !errors.Is(err, ErrCognitionConflict) {
		t.Fatal("stale response committed", err)
	}
	state, _ := a.store.Read()
	if len(state.Thoughts) != 0 || len(state.Records) != 0 {
		t.Fatal("stale model result entered cognition")
	}
}

func TestCharacterAgentBudgetTransportAndWriteFailures(t *testing.T) {
	dir, spec := thoughtFixture(t)
	calls := 0
	a, _ := NewCharacterAgent(dir, "沈砚", CompletionFunc(func(_ context.Context, r ModelRequest) (ModelReply, error) { calls++; return mockThought(r), nil }))
	limited := spec
	limited.ContextBudgetTokens = 1200
	if _, err := a.Think(context.Background(), "turn-budget", limited); err == nil || calls != 0 {
		t.Fatal("budget overflow called model")
	}
	if err := os.Mkdir(a.store.path()+".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Think(context.Background(), "turn-failed-write", spec); err == nil {
		t.Fatal("failed write reported success")
	}
	if state, err := a.store.Read(); err != nil || len(state.Thoughts) != 0 {
		t.Fatal("failed write published receipt")
	}
	if err := os.Remove(a.store.path() + ".tmp"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Think(context.Background(), "turn-failed-write", spec); err != nil {
		t.Fatal("retry failed", err)
	}
	a.client = CompletionFunc(func(context.Context, ModelRequest) (ModelReply, error) {
		return ModelReply{}, errors.New("API-PRIVATE-CANARY")
	})
	if _, err := a.Think(context.Background(), "transport-fail", spec); err == nil || strings.Contains(err.Error(), "PRIVATE-CANARY") {
		t.Fatal("transport error leaked or was accepted")
	}
}
