package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func configureForTest(t *testing.T, yamlText string) {
	t.Helper()
	raw := mustJSON(t, map[string]any{
		"config_yaml":    []byte(yamlText),
		"schema_version": 2,
	})
	if err := configure(raw); err != nil {
		t.Fatalf("configure: %v", err)
	}
}

func decodeIntercept(t *testing.T, raw []byte) interceptResponse {
	t.Helper()
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *envelopeError  `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("envelope failed: %+v", env.Error)
	}
	var response interceptResponse
	if err := json.Unmarshal(env.Result, &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return response
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func decodeMessages(t *testing.T, body []byte) []chatMessage {
	t.Helper()
	var root struct {
		Messages []chatMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return root.Messages
}

func TestPrependOpenAI(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`)
	out, changed, supported := injectPrompt(body, "JB", "prepend")
	if !changed || !supported {
		t.Fatalf("changed=%v supported=%v", changed, supported)
	}
	messages := decodeMessages(t, out)
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d: %s", len(messages), out)
	}
	if messages[0].Role != "system" || messages[0].Content != "JB" {
		t.Fatalf("unexpected system message: %+v", messages[0])
	}
	if messages[1].Role != "user" || messages[1].Content != "hi" {
		t.Fatalf("user message changed: %+v", messages[1])
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	if string(root["max_tokens"]) != "8" {
		t.Fatalf("max_tokens not preserved: %s", root["max_tokens"])
	}
}

func TestReplaceOpenAI(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"old"},{"role":"developer","content":"dev"},{"role":"user","content":"hi"}]}`)
	out, changed, supported := injectPrompt(body, "JB", "replace")
	if !changed || !supported {
		t.Fatalf("changed=%v supported=%v", changed, supported)
	}
	messages := decodeMessages(t, out)
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d: %s", len(messages), out)
	}
	if messages[0].Role != "system" || messages[0].Content != "JB" {
		t.Fatalf("unexpected first message: %+v", messages[0])
	}
	if messages[1].Role != "user" || messages[1].Content != "hi" {
		t.Fatalf("unexpected second message: %+v", messages[1])
	}
}

func TestAppendOpenAI(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"old"},{"role":"user","content":"hi"}]}`)
	out, changed, supported := injectPrompt(body, "JB", "append")
	if !changed || !supported {
		t.Fatalf("changed=%v supported=%v", changed, supported)
	}
	messages := decodeMessages(t, out)
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d: %s", len(messages), out)
	}
	if messages[0].Content != "old" || messages[1].Content != "JB" || messages[2].Content != "hi" {
		t.Fatalf("unexpected message order: %+v", messages)
	}
}

func TestPrependIsIdempotent(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	first, changed, _ := injectPrompt(body, "JB", "prepend")
	if !changed {
		t.Fatal("first injection did not change the body")
	}
	second, changed, supported := injectPrompt(first, "JB", "prepend")
	if changed || !supported {
		t.Fatalf("second injection changed=%v supported=%v", changed, supported)
	}
	if string(second) != string(first) {
		t.Fatalf("idempotent body changed: %s -> %s", first, second)
	}
}

func TestAnthropicStringSystem(t *testing.T) {
	body := []byte(`{"model":"m","system":"old","messages":[{"role":"user","content":"hi"}]}`)
	out, changed, supported := injectPrompt(body, "JB", "prepend")
	if !changed || !supported {
		t.Fatalf("changed=%v supported=%v", changed, supported)
	}
	var root struct {
		System string `json:"system"`
	}
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	if root.System != "JB\n\nold" {
		t.Fatalf("unexpected system: %q", root.System)
	}
}

func TestAnthropicBlockSystem(t *testing.T) {
	body := []byte(`{"system":[{"type":"text","text":"old"}],"messages":[]}`)
	out, changed, supported := injectPrompt(body, "JB", "append")
	if !changed || !supported {
		t.Fatalf("changed=%v supported=%v", changed, supported)
	}
	var root struct {
		System []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"system"`
	}
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	if len(root.System) != 2 || root.System[0].Text != "old" || root.System[1].Text != "JB" {
		t.Fatalf("unexpected blocks: %+v", root.System)
	}
}

func TestNullSystem(t *testing.T) {
	body := []byte(`{"system":null,"messages":[]}`)
	out, changed, supported := injectPrompt(body, "JB", "prepend")
	if !changed || !supported {
		t.Fatalf("changed=%v supported=%v", changed, supported)
	}
	var root struct {
		System string `json:"system"`
	}
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	if root.System != "JB" {
		t.Fatalf("unexpected system: %q", root.System)
	}
}

func TestResponsesInstructions(t *testing.T) {
	body := []byte(`{"model":"m","instructions":"old","input":"hi"}`)
	out, changed, supported := injectPrompt(body, "JB", "replace")
	if !changed || !supported {
		t.Fatalf("changed=%v supported=%v", changed, supported)
	}
	var root struct {
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	if root.Instructions != "JB" {
		t.Fatalf("unexpected instructions: %q", root.Instructions)
	}
}

func TestResponsesInputOnly(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi"}`)
	out, changed, supported := injectPrompt(body, "JB", "prepend")
	if !changed || !supported {
		t.Fatalf("changed=%v supported=%v", changed, supported)
	}
	var root struct {
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	if root.Instructions != "JB" {
		t.Fatalf("unexpected instructions: %q", root.Instructions)
	}
}

func TestUnsupportedBody(t *testing.T) {
	_, changed, supported := injectPrompt([]byte(`{"model":"m","prompt":"x"}`), "JB", "prepend")
	if changed || supported {
		t.Fatalf("changed=%v supported=%v", changed, supported)
	}
}

func TestSelectProfileExactAndGlob(t *testing.T) {
	configureForTest(t, `
profiles:
  - name: exact
    models: ["deepseek-v4.1-flash"]
    prompt: EXACT
  - name: glob
    models: ["devin/*"]
    prompt: GLOB
`)
	cfgMu.RLock()
	current := cfg
	cfgMu.RUnlock()

	profile, err := selectProfile(current, "deepseek-v4.1-flash")
	if err != nil || profile.Name != "exact" {
		t.Fatalf("exact match failed: profile=%v err=%v", profile, err)
	}
	profile, err = selectProfile(current, "devin/swe-2")
	if err != nil || profile.Name != "glob" {
		t.Fatalf("glob match failed: profile=%v err=%v", profile, err)
	}
	if _, err := selectProfile(current, "unknown"); err == nil {
		t.Fatal("expected no-match error")
	}
}

func TestStripSuffix(t *testing.T) {
	cases := []struct {
		model  string
		want   string
		wantOK bool
	}{
		{"deepseek-v4.1-flash-jailbreak", "deepseek-v4.1-flash", true},
		{"DeepSeek-V4.1-Flash-JailBreak", "DeepSeek-V4.1-Flash", true},
		{"deepseek-v4.1-flash", "", false},
		{"-jailbreak", "", false},
	}
	for _, tc := range cases {
		got, ok := stripSuffix(tc.model, "-jailbreak")
		if got != tc.want || ok != tc.wantOK {
			t.Fatalf("stripSuffix(%q) = %q,%v; want %q,%v", tc.model, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestSelectProfileIsCaseInsensitive(t *testing.T) {
	configureForTest(t, `
profiles:
  - name: deepseek
    models: ["DeepSeek-V4.1-Flash"]
    prompt: JB
`)
	cfgMu.RLock()
	current := cfg
	cfgMu.RUnlock()
	profile, err := selectProfile(current, "deepseek-v4.1-flash")
	if err != nil || profile.Name != "deepseek" {
		t.Fatalf("case-insensitive match failed: profile=%v err=%v", profile, err)
	}
}

func TestConfigureRejectsUnknownMode(t *testing.T) {
	raw := mustJSON(t, map[string]any{
		"config_yaml": []byte(`
profiles:
  - name: bad
    models: ["m"]
    mode: overwrite
    prompt: JB
`),
		"schema_version": 2,
	})
	if err := configure(raw); err == nil || !strings.Contains(err.Error(), "unknown mode") {
		t.Fatalf("expected unknown mode error, got %v", err)
	}
}

func TestConfigureRejectsOldSchema(t *testing.T) {
	raw := mustJSON(t, map[string]any{"schema_version": 1})
	if err := configure(raw); err == nil {
		t.Fatal("expected old schema error")
	}
}

func TestInterceptBeforeNormalModel(t *testing.T) {
	configureForTest(t, `
profiles:
  - name: deepseek
    models: ["deepseek-v4.1-flash"]
    prompt: JB
`)
	body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)
	raw, err := interceptBefore(mustJSON(t, interceptRequest{
		RequestID:      "r1",
		SourceFormat:   "openai",
		Model:          "deepseek-v4.1-flash",
		RequestedModel: "deepseek-v4.1-flash",
		Body:           body,
	}))
	if err != nil {
		t.Fatal(err)
	}
	response := decodeIntercept(t, raw)
	if response.Terminate || len(response.Body) != 0 {
		t.Fatalf("normal model request was modified: %+v", response)
	}
}

func TestInterceptBeforeInjects(t *testing.T) {
	configureForTest(t, `
profiles:
  - name: deepseek
    models: ["deepseek-v4.1-flash"]
    prompt: JB
    mode: prepend
`)
	body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)
	raw, err := interceptBefore(mustJSON(t, interceptRequest{
		RequestID:      "r2",
		SourceFormat:   "openai",
		Model:          "deepseek-v4.1-flash",
		RequestedModel: "deepseek-v4.1-flash-jailbreak",
		Body:           body,
	}))
	if err != nil {
		t.Fatal(err)
	}
	response := decodeIntercept(t, raw)
	if response.Terminate {
		t.Fatalf("unexpected termination: %+v", response)
	}
	messages := decodeMessages(t, response.Body)
	if len(messages) != 2 || messages[0].Role != "system" || messages[0].Content != "JB" {
		t.Fatalf("unexpected injected body: %s", response.Body)
	}
}

func TestInterceptBeforeUnmapped(t *testing.T) {
	configureForTest(t, `
unmapped: error
profiles:
  - name: deepseek
    models: ["deepseek-v4.1-flash"]
    prompt: JB
`)
	raw, err := interceptBefore(mustJSON(t, interceptRequest{
		RequestID:      "r3",
		SourceFormat:   "openai",
		Model:          "devin/swe-2",
		RequestedModel: "devin/swe-2-jailbreak",
		Body:           []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	}))
	if err != nil {
		t.Fatal(err)
	}
	response := decodeIntercept(t, raw)
	if !response.Terminate || response.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 termination, got %+v", response)
	}
	if !strings.Contains(string(response.ResponseBody), "no jailbreak profile") {
		t.Fatalf("unexpected error body: %s", response.ResponseBody)
	}
}

func TestInterceptBeforeUnmappedPass(t *testing.T) {
	configureForTest(t, `
unmapped: pass
profiles:
  - name: deepseek
    models: ["deepseek-v4.1-flash"]
    prompt: JB
`)
	raw, err := interceptBefore(mustJSON(t, interceptRequest{
		RequestID:      "r4",
		SourceFormat:   "openai",
		Model:          "devin/swe-2",
		RequestedModel: "devin/swe-2-jailbreak",
		Body:           []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	}))
	if err != nil {
		t.Fatal(err)
	}
	response := decodeIntercept(t, raw)
	if response.Terminate || len(response.Body) != 0 {
		t.Fatalf("unexpected response: %+v", response)
	}
}
