package config

import "testing"

func TestPartialConfigPreservesGenerationSettings(t *testing.T) {
	base := &APIConfig{Model: "old", MaxTokens: 12000, HTTPTimeoutSeconds: 321, ExtraBody: map[string]any{"reasoning": false}, ModelTiers: map[string]string{"premium": "writer"}, Novel: &GenerationConfig{Model: "long", MaxTokens: 20000, HTTPTimeoutSeconds: 600}}
	next, err := MergeAPIConfig(base, []byte(`{"model":"new","base_url":"http://localhost","api_key":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	if next.MaxTokens != 12000 || next.HTTPTimeoutSeconds != 321 || next.ExtraBody["reasoning"] != false || next.ModelTiers["premium"] != "writer" || next.Novel.MaxTokens != 20000 {
		t.Fatalf("lost fields: %#v", next)
	}
	if base.Model != "old" {
		t.Fatal("mutated original")
	}
	novel := next.NovelConfig()
	if novel.Model != "long" || novel.MaxTokens != 20000 || novel.HTTPTimeoutSeconds != 600 || next.Model != "new" {
		t.Fatal("novel override leaked")
	}
}
