package novel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"worldsim/internal/characteragent"
	"worldsim/internal/config"
	"worldsim/internal/engine"
	"worldsim/internal/llm"
	"worldsim/internal/sim"
)

func independentWriterFixture(t *testing.T) (*Writer, *engine.StateEngine, string, []sim.ChronicleEntry) {
	t.Helper()
	dir := t.TempDir()
	e := engine.NewStateEngine(engine.Rules{}, filepath.Join(dir, "event_log.jsonl"))
	e.State().WorldLevel.Protagonist = "A"
	e.State().WorldLevel.NarrativePOV = "A"
	for _, id := range []string{"A", "B", "C", "D", "E"} {
		e.State().Entities[id] = engine.Entity{Alive: true, Status: "active", Location: "room", Money: 10, Extra: map[string]any{"persona": id + "-PRIVATE-CANARY"}}
	}
	registry, _ := characteragent.NewRegistry(dir)
	_, _ = registry.Reconcile(e.State())
	providers := config.ProviderRegistry{Providers: map[string]config.Provider{"p": {Name: "p", Protocol: "openai_compatible", BaseURL: "http://unused.local"}}}
	policy := config.AgentRuntimeConfig{SchemaVersion: 1, Tiers: map[config.AgentImportance]config.TierPolicy{}, Characters: map[string]config.CharacterAgentConfig{}}
	for _, tier := range []config.AgentImportance{config.ImportanceCore, config.ImportanceSupporting, config.ImportanceBackground} {
		policy.Tiers[tier] = config.TierPolicy{ModelRef: config.ModelRef{ProviderID: "p", Model: "mock"}}
	}
	runtime, err := characteragent.NewIndependentRuntime(dir, e, policy, providers, characteragent.CompletionFunc(func(context.Context, characteragent.ModelRequest) (characteragent.ModelReply, error) {
		t.Error("setup invoked a model")
		return characteragent.ModelReply{}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.InitializeCards(context.Background(), 42, 1); err != nil {
		t.Fatal(err)
	}
	w := &Writer{BookDir: filepath.Join(dir, "book"), APICfg: &config.APIConfig{Model: "mock"}, HeroName: "A", DaysPerCh: 3, WorldRefs: "GLOBAL-PRIVATE-CANARY", NarrativeContext: "AUTHOR-PRIVATE-CANARY", Foreshadows: "FUTURE-PRIVATE-CANARY", Decisions: "DRAFT-PRIVATE-CANARY"}
	state, _ := e.Snapshot()
	entries, err := w.PrepareIndependentSources(dir, state)
	if err != nil {
		t.Fatal(err)
	}
	return w, e, dir, entries
}

func approvingReview(t *testing.T, user string) SemanticReview {
	t.Helper()
	var req struct {
		Capsule struct {
			POV          string            `json:"pov"`
			SourceDigest string            `json:"source_digest"`
			Facts        []IndependentFact `json:"facts"`
		} `json:"capsule"`
		TextDigest string `json:"text_digest"`
	}
	if err := json.Unmarshal([]byte(user), &req); err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, fact := range req.Capsule.Facts {
		ids = append(ids, fact.ID)
	}
	return SemanticReview{SourceDigest: req.Capsule.SourceDigest, TextDigest: req.TextDigest, POV: req.Capsule.POV, Facts: true, Perspective: true, Continuity: true, RevealOrder: true, Issues: []string{}, CheckedSources: ids}
}

func TestIndependentNovelUsesOnlyPOVReceiptsAndPersistsReview(t *testing.T) {
	w, e, dir, entries := independentWriterFixture(t)
	var game struct {
		Game struct {
			Hands map[string][]string `json:"hands"`
		} `json:"game"`
	}
	b, _ := json.Marshal(e.State().WorldLevel.Runtime)
	_ = json.Unmarshal(b, &game)
	calls := 0
	w.IndependentCall = func(_ context.Context, _ *config.APIConfig, system, user string) (llm.CompletionResult, error) {
		calls++
		for _, secret := range []string{"B-PRIVATE-CANARY", "GLOBAL-PRIVATE-CANARY", "AUTHOR-PRIVATE-CANARY", "FUTURE-PRIVATE-CANARY", "DRAFT-PRIVATE-CANARY"} {
			if strings.Contains(system+user, secret) {
				t.Error("global/private context reached novel request")
			}
		}
		for _, card := range game.Game.Hands["B"] {
			if strings.Contains(system+user, card) {
				t.Error("B hand leaked into writer or reviewer")
			}
		}
		if strings.Contains(system, "审稿员") {
			review := approvingReview(t, user)
			raw, _ := json.Marshal(review)
			return llm.CompletionResult{Content: string(raw), FinishReason: "stop"}, nil
		}
		return llm.CompletionResult{Content: validChapter("第一手"), FinishReason: "stop", ReasoningContent: "REASONING-PRIVATE-CANARY"}, nil
	}
	before, _ := os.ReadFile(filepath.Join(dir, "event_log.jsonl"))
	plans, err := w.EnsurePlans(context.Background(), entries)
	if err != nil || len(plans) != 1 {
		t.Fatal(plans, err)
	}
	if calls != 0 {
		t.Fatal("independent chapter planning called a model")
	}
	if _, err := w.WriteChapter(context.Background(), plans[0], entries, nil, e.State().Entities); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("expected one generation and one review")
	}
	raw, err := os.ReadFile(filepath.Join(w.BookDir, "published", "001.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pub Publication
	if json.Unmarshal(raw, &pub) != nil || pub.SemanticReview == nil || pub.Quality["pov_semantics"] != "model_review_passed" || len(pub.SourceIDs) == 0 {
		t.Fatal("review/reader provenance missing")
	}
	if strings.Contains(string(raw), "REASONING-PRIVATE-CANARY") {
		t.Fatal("reasoning persisted")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "event_log.jsonl"))
	if string(before) != string(after) {
		t.Fatal("novel writing changed world facts")
	}
}

func TestIndependentReviewFailuresNeverPublishOrReplaceExistingChapter(t *testing.T) {
	for _, mode := range []string{"facts", "perspective", "continuity", "reveal", "source_digest", "text_digest", "coverage", "format", "truncated", "body_truncated", "duplicate_keys"} {
		t.Run(mode, func(t *testing.T) {
			w, _, _, entries := independentWriterFixture(t)
			plan := ChapterPlan{Num: 1, DayStart: 1, DayEnd: 1, Days: []int{1}, SourceMode: "independent_v1"}
			plan.SourceDigest = w.sourceDigest(plan, entries)
			w.IndependentCall = func(_ context.Context, _ *config.APIConfig, system, user string) (llm.CompletionResult, error) {
				if !strings.Contains(system, "审稿员") {
					reason := "stop"
					if mode == "body_truncated" {
						reason = "length"
					}
					return llm.CompletionResult{Content: validChapter("不可发布"), FinishReason: reason}, nil
				}
				review := approvingReview(t, user)
				switch mode {
				case "facts":
					review.Facts = false
				case "perspective":
					review.Perspective = false
				case "continuity":
					review.Continuity = false
				case "reveal":
					review.RevealOrder = false
				case "source_digest":
					review.SourceDigest = "wrong"
				case "text_digest":
					review.TextDigest = "wrong"
				case "coverage":
					review.CheckedSources = nil
				case "format":
					return llm.CompletionResult{Content: "{} trailing", FinishReason: "stop"}, nil
				}
				raw, _ := json.Marshal(review)
				if mode == "duplicate_keys" {
					raw = []byte(strings.TrimSuffix(string(raw), "}") + `,"facts":false,"facts":true}`)
				}
				reason := "stop"
				if mode == "truncated" {
					reason = "length"
				}
				return llm.CompletionResult{Content: string(raw), FinishReason: reason}, nil
			}
			// Existing user chapter must survive every failed candidate verbatim.
			_ = os.MkdirAll(filepath.Join(w.BookDir, "chapters"), 0755)
			path := filepath.Join(w.BookDir, "chapters", "001_original.md")
			_ = os.WriteFile(path, []byte("USER-ORIGINAL"), 0600)
			if _, err := w.WriteChapter(context.Background(), plan, entries, nil, nil); err == nil {
				t.Fatal("failed semantic gate accepted")
			}
			raw, _ := os.ReadFile(path)
			if string(raw) != "USER-ORIGINAL" {
				t.Fatal("old chapter replaced")
			}
			versions, _ := filepath.Glob(filepath.Join(w.BookDir, "versions", "*", "*", "publication.json"))
			if len(versions) != 0 {
				t.Fatal("rejected candidate became formal version")
			}
		})
	}
}

func TestIndependentRevealOrderUsesChapterBoundaryAndExactAudience(t *testing.T) {
	w, e, dir, _ := independentWriterFixture(t)
	commit := func(id string, day int, audience []string, text string) {
		t.Helper()
		if err := e.Submit(context.Background(), &engine.Proposal{CommandID: id, ActorID: "world_agent", BaseRevision: e.State().Revision, Type: "adjudication", Changes: []engine.Change{{Path: "world_level.day", Op: "set", Value: day}}, Occurrences: []engine.Occurrence{{Kind: "utterance", Actor: "B", Location: "room", Audience: audience, Content: text}}}); err != nil {
			t.Fatal(err)
		}
		r, _ := characteragent.NewRegistry(dir)
		_, err := r.Reconcile(e.State())
		if err != nil {
			t.Fatal(err)
		}
	}
	commit("private", 1, []string{"B", "C"}, "UNHEARD-PRIVATE-CANARY")
	commit("reveal", 2, []string{"A", "B"}, "PUBLIC-REVEAL-CANARY")
	state, _ := e.Snapshot()
	_, err := w.PrepareIndependentSources(dir, state)
	if err != nil {
		t.Fatal(err)
	}
	early, _ := json.Marshal(w.chapterFacts(ChapterPlan{DayStart: 1, DayEnd: 1}))
	late, _ := json.Marshal(w.chapterFacts(ChapterPlan{DayStart: 2, DayEnd: 2}))
	if strings.Contains(string(early), "PRIVATE-CANARY") || strings.Contains(string(early), "PUBLIC-REVEAL-CANARY") {
		t.Fatal("future or unheard revelation entered early chapter")
	}
	if !strings.Contains(string(late), "PUBLIC-REVEAL-CANARY") || strings.Contains(string(late), "UNHEARD-PRIVATE-CANARY") {
		t.Fatal("revelation audience/boundary incorrect")
	}
	if !strings.Contains(string(late), "public_statement") {
		t.Fatal("heard claim promoted to truth")
	}
	// Rewriting earlier plans remains bound to old sources, not current global state.
	p := ChapterPlan{DayStart: 1, DayEnd: 1}
	first := w.sourceDigest(p, nil)
	w.Independent.Facts = append(w.Independent.Facts, IndependentFact{ID: "future", Cycle: 3, Text: "future"})
	if first != w.sourceDigest(p, nil) {
		t.Fatal("future event changed earlier chapter source boundary")
	}
}
