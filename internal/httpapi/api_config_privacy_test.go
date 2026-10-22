package httpapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"worldsim/internal/config"
)

func TestAPIConfigGetAndPutDoNotReturnSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.json")
	cfg := &config.APIConfig{BaseURL: "https://example.com/v1", Model: "model", APIKey: "STORED-KEY-CANARY", ContextBudgetTokens: 10000, ExtraBody: map[string]any{"authorization": "BODY-CANARY"}, MaxTokens: 9000}
	if err := config.SaveAPIConfig(cfg, path); err != nil {
		t.Fatal(err)
	}
	h := &Handlers{apiCfg: cfg, apiCfgPath: path}
	get := httptest.NewRecorder()
	h.GetAPIConfig(get, httptest.NewRequest("GET", "/api/config/api", nil))
	if get.Code != 200 || strings.Contains(get.Body.String(), "CANARY") || !strings.Contains(get.Body.String(), `"has_api_key":true`) {
		t.Fatal("GET leaked key or lost key-presence flag")
	}
	put := httptest.NewRecorder()
	h.PutAPIConfig(put, httptest.NewRequest("PUT", "/api/config/api", bytes.NewBufferString(`{"model":"updated","api_key":""}`)))
	if put.Code != 200 || strings.Contains(put.Body.String(), "CANARY") {
		t.Fatal("PUT failed or exposed key")
	}
	saved, err := config.LoadAPIConfig(path)
	if err != nil || saved.APIKey != cfg.APIKey || saved.ExtraBody["authorization"] != "BODY-CANARY" || saved.MaxTokens != 9000 {
		t.Fatal("PUT erased stored credentials or advanced fields")
	}
}

func TestAPIConnectionTestUsesStoredKeyWithoutSaving(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			// The client may probe model metadata in addition to the completion.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		calls++
		if r.Header.Get("Authorization") != "Bearer LOCAL-KEY" {
			t.Error("connection test did not use stored credential")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()
	path := filepath.Join(t.TempDir(), "api.json")
	cfg := &config.APIConfig{BaseURL: upstream.URL + "/v1", Model: "model", APIKey: "LOCAL-KEY", ContextBudgetTokens: 10000}
	if err := config.SaveAPIConfig(cfg, path); err != nil {
		t.Fatal(err)
	}
	h := &Handlers{apiCfg: cfg, apiCfgPath: path}
	rec := httptest.NewRecorder()
	h.PostAPITest(rec, httptest.NewRequest("POST", "/api/config/api/test", bytes.NewBufferString(`{"api_key":"","model":"test-model"}`)))
	if rec.Code != 200 || calls != 1 || strings.Contains(rec.Body.String(), "LOCAL-KEY") {
		t.Fatalf("test failed, status %d, calls %d", rec.Code, calls)
	}
	saved, err := config.LoadAPIConfig(path)
	if err != nil || saved.Model != "model" {
		t.Fatal("connection test persisted temporary configuration")
	}
}
