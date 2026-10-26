// opencode-go-quota — CLIProxyAPI C ABI plugin that surfaces per-account
// OpenCode Go subscription quota (rolling/weekly/monthly windows) through
// CPA's native QuotaProvider capability, so it renders inside CPAMC.
//
// C ABI preamble follows the same pattern as zgs225/cliproxy-plugin-commandcode (MIT).
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"gopkg.in/yaml.v3"
)

var goFileRe = regexp.MustCompile(`^opencode-go-(\d+)\.json$`)

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

var pluginVersion = "0.1.0"

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = 1
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw := handleMethod(C.GoString(method), requestBytes)
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = length
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

// callHost invokes a host callback method (e.g. host.http.do) and returns the
// raw envelope bytes. Unused for now; upstream calls go through net/http.
func callHost(method string, payload []byte) ([]byte, bool) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var response C.cliproxy_buffer
	var req *C.uint8_t
	if len(payload) > 0 {
		req = (*C.uint8_t)(C.CBytes(payload))
		defer C.free(unsafe.Pointer(req))
	}
	rc := C.call_host_api(cMethod, req, C.size_t(len(payload)), &response)
	var out []byte
	if response.ptr != nil {
		out = C.GoBytes(unsafe.Pointer(response.ptr), C.int(response.len))
		C.free_host_buffer(response.ptr, response.len)
	}
	return out, rc == 0
}

// ---------------------------------------------------------------------------
// config
// ---------------------------------------------------------------------------

type pluginConfig struct {
	mu               sync.RWMutex
	OpenCodeAPIKey   string   `yaml:"opencode_api_key"`
	OpenCodeAPIKeys  []string `yaml:"opencode_api_keys"`
	OpenCodeAPIBase  string   `yaml:"opencode_api_base"`
	RefreshSeconds   int      `yaml:"refresh_seconds"`
}

var cfg = &pluginConfig{OpenCodeAPIBase: "https://opencode.ai/zen/go/v1"}

func (c *pluginConfig) update(raw []byte) {
	if len(raw) == 0 {
		return
	}
	var tmp pluginConfig
	if err := yaml.Unmarshal(raw, &tmp); err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := make([]string, 0, len(tmp.OpenCodeAPIKeys)+1)
	seen := map[string]bool{}
	for _, k := range append(tmp.OpenCodeAPIKeys, tmp.OpenCodeAPIKey) {
		k = strings.TrimSpace(k)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		keys = append(keys, k)
	}
	c.OpenCodeAPIKeys = keys
	c.OpenCodeAPIKey = tmp.OpenCodeAPIKey
	if b := strings.TrimSpace(tmp.OpenCodeAPIBase); b != "" {
		c.OpenCodeAPIBase = strings.TrimRight(b, "/")
	}
	if tmp.RefreshSeconds > 0 {
		c.RefreshSeconds = tmp.RefreshSeconds
	}
}

func (c *pluginConfig) snapshot() (base string, keys []string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	base = c.OpenCodeAPIBase
	if base == "" {
		base = "https://opencode.ai/zen/go/v1"
	}
	keys = append([]string(nil), c.OpenCodeAPIKeys...)
	return
}

// ---------------------------------------------------------------------------
// method dispatch
// ---------------------------------------------------------------------------

func handleMethod(method string, request []byte) []byte {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if len(request) > 0 {
			_ = json.Unmarshal(request, &req)
		}
		cfg.update(req.ConfigYAML)
		return okEnvelope(registration())
	case "plugin.quiesce", "plugin.shutdown":
		return okEnvelope(map[string]any{"shutdown": true})

	case "quota.identifier":
		return okEnvelope(map[string]any{"identifier": "opencode-go-quota"})
	case "quota.describe":
		return okEnvelope(map[string]any{
			"supported_providers": []string{"openai-compatibility", "opencode-go-pool", "opencode-go"},
			"display_name":        "OpenCode Go Pool",
			"supports_reset":      false,
		})
	case "auth.identifier":
		return okEnvelope(map[string]any{"identifier": "opencode-go"})
	case "auth.parse":
		return authParse(request)
	case "auth.refresh":
		return okEnvelope(map[string]any{"handled": false})
	case "auth.login.start", "auth.login.poll":
		return errorEnvelope("not_supported", "opencode-go keys are imported via auth files")
	case "quota.fetch":
		return quotaFetch(request)
	case "quota.reset":
		return errorEnvelope("not_supported", "OpenCode Go quota cannot be reset")
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method)
	}
}

func registration() map[string]any {
	return map[string]any{
		"schema_version": 1,
		"metadata": map[string]any{
			"Name":             "opencode-go-quota",
			"Version":          pluginVersion,
			"Author":           "orca-ai-pool",
			"GitHubRepository": "https://github.com/AzureLandin/orca-ai-pool",
			"ConfigFields": []map[string]any{
				{"Name": "opencode_api_keys", "Type": "string", "Description": "OpenCode Go API keys (YAML list) monitored by this quota provider"},
				{"Name": "opencode_api_key", "Type": "string", "Description": "Single OpenCode Go API key (fallback)"},
				{"Name": "opencode_api_base", "Type": "string", "Description": "OpenCode Go API base URL (default https://opencode.ai/zen/go/v1)"},
				{"Name": "refresh_seconds", "Type": "integer", "Description": "Minimum seconds between upstream usage polls per fetch (default 60)"},
			},
		},
		"capabilities": map[string]any{
			"quota_provider": true,
			"auth_provider":  true,
		},
	}
}

// ---------------------------------------------------------------------------
// upstream usage query
// ---------------------------------------------------------------------------

type usageWindow struct {
	Status   string  `json:"status"`
	Percent  float64 `json:"percent"`
	ResetsAt string  `json:"resetsAt"`
}

type usageResponse struct {
	Usage struct {
		Rolling usageWindow `json:"rolling"`
		Weekly  usageWindow `json:"weekly"`
		Monthly usageWindow `json:"monthly"`
	} `json:"usage"`
}

var httpClient = &http.Client{Timeout: 15 * time.Second}

func fetchUsage(ctx context.Context, base, key string) (*usageResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/usage", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "cpa-plugin-opencode-go-quota/"+pluginVersion)
	res, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream HTTP %d", res.StatusCode)
	}
	var out usageResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("unmarshal usage: %w", err)
	}
	return &out, nil
}

func maskKey(key string) string {
	if len(key) < 8 {
		return "***"
	}
	return key[:4] + "…" + key[len(key)-4:]
}

// ---------------------------------------------------------------------------
// auth.parse — turn auths/*.json with type "opencode-go" into credentials so
// each key appears as a first-class entry in CPAMC's 配额管理 view.
// ---------------------------------------------------------------------------

func authParse(request []byte) []byte {
	var req struct {
		Provider string          `json:"provider"`
		FileName string          `json:"file_name"`
		RawJSON  json.RawMessage `json:"raw_json"`
	}
	if err := json.Unmarshal(request, &req); err != nil {
		return okEnvelope(map[string]any{"handled": false})
	}
	var file struct {
		Type   string `json:"type"`
		APIKey string `json:"api_key"`
		Name   string `json:"name"`
		Email  string `json:"email"`
	}
	_ = json.Unmarshal(req.RawJSON, &file)
	if file.Type != "opencode-go" || file.APIKey == "" {
		return okEnvelope(map[string]any{"handled": false})
	}
	name := file.Name
	if name == "" {
		name = strings.TrimSuffix(req.FileName, ".json")
	}
	auth := map[string]any{
		"id":       req.FileName,
		"provider": "opencode-go",
		"file_name": req.FileName,
		"attributes": map[string]string{
			"api_key": file.APIKey,
		},
		"metadata": map[string]any{
			"email": file.Email,
			"name":  name,
			"plan":  "OpenCode Go",
		},
	}
	return okEnvelope(map[string]any{"handled": true, "auth": auth})
}

// ---------------------------------------------------------------------------
// quota.fetch
// ---------------------------------------------------------------------------

// cache avoids hammering upstream when CPAMC polls several times a minute.
var cache struct {
	mu      sync.Mutex
	fetched time.Time
	groups  []map[string]any
	summary []map[string]any
	errs    int
	keys    int
}

func quotaFetch(request []byte) []byte {
	var req struct {
		AuthIndex  string `json:"auth_index"`
		AuthID     string `json:"auth_id"`
		Provider   string `json:"provider"`
		Attributes map[string]string `json:"attributes"`
	}
	_ = json.Unmarshal(request, &req)

	// Per-credential fetch: pool auth (no N suffix) = aggregate all keys;
	// opencode-go-N.json maps to the Nth configured key.
	base, keys := cfg.snapshot()
	singleKey := false
	authID := strings.TrimSpace(req.AuthID)
	if m := goFileRe.FindStringSubmatch(authID); m != nil {
		if n := atoi(m[1]); n >= 1 && n <= len(keys) {
			keys = []string{keys[n-1]}
			singleKey = true
		}
	} else if k := strings.TrimSpace(req.Attributes["api_key"]); k != "" && k != "__pool__" {
		keys = []string{k}
		singleKey = true
	}
	if len(keys) == 0 {
		return errorEnvelope("no_keys", "opencode_api_keys is empty in plugin config")
	}

	refresh := time.Duration(60) * time.Second
	if s := cfg.RefreshSeconds; s > 0 {
		refresh = time.Duration(s) * time.Second
	}

	if !singleKey {
		cache.mu.Lock()
		if time.Since(cache.fetched) < refresh && len(cache.groups) > 0 {
			resp := buildFetchResponse(cache.summary, cache.groups, cache.errs, cache.keys, req)
			cache.mu.Unlock()
			return okEnvelope(resp)
		}
		cache.mu.Unlock()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	type result struct {
		key string
		u   *usageResponse
		err error
	}
	results := make([]result, len(keys))
	var wg sync.WaitGroup
	for i, key := range keys {
		wg.Add(1)
		go func(i int, key string) {
			defer wg.Done()
			u, err := fetchUsage(ctx, base, key)
			results[i] = result{key: key, u: u, err: err}
		}(i, key)
	}
	wg.Wait()

	groups := make([]map[string]any, 0, len(results)+1)
	errs := 0
	minRemaining := 101.0
	// per-window aggregate across healthy keys (average remaining %, earliest reset)
	agg := map[string]struct {
		sum    float64
		n      int
		reset  string
	}{"rolling": {}, "weekly": {}, "monthly": {}}
	for _, r := range results {
		if r.err != nil {
			errs++
			groups = append(groups, map[string]any{
				"displayName": maskKey(r.key),
				"buckets": []map[string]any{
					{"window": "status", "remainingFraction": 0, "description": "query failed: " + r.err.Error()},
				},
			})
			continue
		}
		buckets := make([]map[string]any, 0, 3)
		for _, w := range []struct {
			name  string
			label string
			win   usageWindow
		}{
			{"5h", "5h 滚动限额", r.u.Usage.Rolling},
			{"weekly", "周限额", r.u.Usage.Weekly},
			{"monthly", "月限额", r.u.Usage.Monthly},
		} {
			remaining := 100 - w.win.Percent
			if remaining < 0 {
				remaining = 0
			}
			if remaining < minRemaining {
				minRemaining = remaining
			}
			a := agg[w.name]
			a.sum += remaining
			a.n++
			if w.win.ResetsAt != "" && (a.reset == "" || w.win.ResetsAt < a.reset) {
				a.reset = w.win.ResetsAt
			}
			agg[w.name] = a
			b := map[string]any{
				"window":            w.name,
				"remainingFraction": remaining / 100,
				"description":       fmt.Sprintf("%s · %.0f%% 剩余", w.label, remaining),
			}
			if w.win.ResetsAt != "" {
				b["resetTime"] = w.win.ResetsAt
			}
			buckets = append(buckets, b)
		}
		if singleKey {
			groups = append(groups, map[string]any{
				"displayName": maskKey(r.key),
				"buckets":     buckets,
			})
		}
	}
	if errs < len(results) {
		aggBuckets := make([]map[string]any, 0, 3)
		for _, wname := range []struct{ name, label string }{
			{"5h", "5h 滚动限额"}, {"weekly", "周限额"}, {"monthly", "月限额"},
		} {
			a := agg[wname.name]
			if a.n == 0 {
				continue
			}
			avg := a.sum / float64(a.n)
			b := map[string]any{
				"window":            wname.name,
				"remainingFraction": avg / 100,
				"description":       fmt.Sprintf("%s · %.0f%% 剩余（%d key 均值）", wname.label, avg, a.n),
			}
			if a.reset != "" {
				b["resetTime"] = a.reset
			}
			aggBuckets = append(aggBuckets, b)
		}
		groups = append(groups, map[string]any{"displayName": "所有 key 综合", "buckets": aggBuckets})
	}
	if minRemaining > 100 {
		minRemaining = 0
	}
	summary := []map[string]any{
		{"key": "accounts", "label": "Accounts", "value": float64(len(keys)), "unit": "keys"},
		{"key": "accounts_failing", "label": "Failing", "value": float64(errs), "unit": "keys"},
		{"key": "min_remaining", "label": "Min remaining", "value": minRemaining, "unit": "%"},
	}

	if !singleKey {
		cache.mu.Lock()
		cache.fetched = time.Now()
		cache.groups = groups
		cache.summary = summary
		cache.errs = errs
		cache.keys = len(keys)
		cache.mu.Unlock()
	}

	return okEnvelope(buildFetchResponse(summary, groups, errs, len(keys), req))
}

func buildFetchResponse(summary, groups []map[string]any, errs, nkeys int, req any) map[string]any {
	return map[string]any{
		"subscription": map[string]any{
			"plan":     "OpenCode Go",
			"tierName": fmt.Sprintf("%d keys", nkeys),
		},
		"summary": summary,
		"groups":  groups,
	}
}

// ---------------------------------------------------------------------------
// envelope
// ---------------------------------------------------------------------------

func okEnvelope(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return errorEnvelope("marshal_error", err.Error())
	}
	out, _ := json.Marshal(map[string]any{"ok": true, "result": json.RawMessage(raw)})
	return out
}

func errorEnvelope(code, message string) []byte {
	out, _ := json.Marshal(map[string]any{
		"ok":    false,
		"error": map[string]any{"code": code, "message": message},
	})
	return out
}
