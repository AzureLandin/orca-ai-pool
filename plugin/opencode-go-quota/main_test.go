package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func setTestConfig(base string, keys []string, refreshSeconds int) {
	var yaml strings.Builder
	fmt.Fprintf(&yaml, "opencode_api_base: %q\n", base)
	fmt.Fprintf(&yaml, "refresh_seconds: %d\n", refreshSeconds)
	yaml.WriteString("opencode_api_keys:\n")
	for _, key := range keys {
		fmt.Fprintf(&yaml, "  - %q\n", key)
	}
	cfg.update([]byte(yaml.String()))
}

func usageServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/usage" {
			http.NotFound(w, r)
			return
		}
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		percent := map[string]float64{
			"key-aaaaaaaa": 20,
			"key-bbbbbbbb": 10,
		}[key]
		if percent == 0 {
			http.Error(w, "unknown key", http.StatusUnauthorized)
			return
		}
		payload := map[string]any{
			"usage": map[string]any{
				"rolling": map[string]any{
					"percent":  percent,
					"resetsAt": "2026-10-28T00:00:00Z",
				},
				"weekly": map[string]any{
					"percent":  percent + 20,
					"resetsAt": "2026-10-31T00:00:00Z",
				},
				"monthly": map[string]any{
					"percent":  percent + 40,
					"resetsAt": "2026-11-01T00:00:00Z",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}))
}

type quotaEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeQuotaEnvelope(t *testing.T, raw []byte) quotaEnvelope {
	t.Helper()
	var env quotaEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("plugin returned error: %+v", env.Error)
	}
	return env
}

func TestQuotaFetchAggregatesHealthyKeys(t *testing.T) {
	server := usageServer(t)
	defer server.Close()
	setTestConfig(server.URL, []string{"key-aaaaaaaa", "key-bbbbbbbb"}, 300)

	env := decodeQuotaEnvelope(t, quotaFetch([]byte(`{}`)))
	var result struct {
		Summary []map[string]any `json:"summary"`
		Groups  []map[string]any `json:"groups"`
	}
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(result.Groups) != 3 {
		t.Fatalf("expected two key groups plus aggregate, got %d", len(result.Groups))
	}
	if result.Groups[2]["displayName"] != "所有 key 综合" {
		t.Fatalf("unexpected aggregate group: %+v", result.Groups[2])
	}
	for _, item := range result.Summary {
		switch item["key"] {
		case "accounts":
			if item["value"] != float64(2) {
				t.Fatalf("unexpected account count: %+v", item)
			}
		case "accounts_failing":
			if item["value"] != float64(0) {
				t.Fatalf("unexpected failure count: %+v", item)
			}
		case "min_remaining":
			if item["value"] != float64(40) {
				t.Fatalf("unexpected minimum remaining: %+v", item)
			}
		}
	}
}

func TestConfigUpdateInvalidatesQuotaCache(t *testing.T) {
	var requests int
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"usage": {
				"rolling": {"percent": 10},
				"weekly": {"percent": 20},
				"monthly": {"percent": 30}
			}
		}`))
	}))
	defer server.Close()

	setTestConfig(server.URL, []string{"key-a"}, 300)
	decodeQuotaEnvelope(t, quotaFetch([]byte(`{}`)))
	setTestConfig(server.URL, []string{"key-b"}, 300)
	decodeQuotaEnvelope(t, quotaFetch([]byte(`{}`)))

	mu.Lock()
	defer mu.Unlock()
	if requests != 2 {
		t.Fatalf("expected a fresh request after config update, got %d", requests)
	}
}

func TestQuotaFetchReusesAggregateCache(t *testing.T) {
	var requests int
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"usage": {
				"rolling": {"percent": 10},
				"weekly": {"percent": 20},
				"monthly": {"percent": 30}
			}
		}`))
	}))
	defer server.Close()

	setTestConfig(server.URL, []string{"key-aaaaaaaa"}, 300)
	decodeQuotaEnvelope(t, quotaFetch([]byte(`{}`)))
	decodeQuotaEnvelope(t, quotaFetch([]byte(`{}`)))

	mu.Lock()
	defer mu.Unlock()
	if requests != 1 {
		t.Fatalf("expected cached second fetch, got %d upstream requests", requests)
	}
}

func TestConfigUpdateConcurrentWithQuotaFetch(t *testing.T) {
	server := usageServer(t)
	defer server.Close()

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = quotaFetch([]byte(`{}`))
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				setTestConfig(server.URL, []string{"key-aaaaaaaa", "key-bbbbbbbb"}, 1)
			}
		}()
	}
	wg.Wait()
}

func TestQuotaFetchRejectsUnknownPerKeyAuth(t *testing.T) {
	setTestConfig("http://127.0.0.1:1", []string{"key-a"}, 300)
	var env quotaEnvelope
	if err := json.Unmarshal(
		quotaFetch([]byte(`{"auth_id":"opencode-go-2.json"}`)),
		&env,
	); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.OK || env.Error == nil || env.Error.Code != "unknown_auth" {
		t.Fatalf("expected unknown_auth error, got %+v", env)
	}
}

func TestAuthParseImportsOpenCodeGoFile(t *testing.T) {
	env := decodeQuotaEnvelope(t, authParse([]byte(`{
		"file_name": "opencode-go-1.json",
		"raw_json": {"type": "opencode-go", "api_key": "key-a", "name": "primary"}
	}`)))
	var result struct {
		Handled bool `json:"handled"`
		Auth    struct {
			Provider string `json:"provider"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !result.Handled || result.Auth.Provider != "opencode-go" {
		t.Fatalf("unexpected auth parse result: %+v", result)
	}
	if result.Auth.Metadata.Name != "primary" {
		t.Fatalf("unexpected auth name: %+v", result.Auth.Metadata)
	}
}
