// jailbreak-inject is a CLIProxyAPI request interceptor.
//
// It looks at the client-requested model name. A name ending in the configured
// suffix (default "-jailbreak") selects a prompt profile by the base model,
// then rewrites the request body so the upstream model sees that profile as its
// system prompt. The execution model is never changed, so CPA keeps routing to
// the same upstream.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	void* call;
	void* free_buffer;
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
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"unsafe"

	"gopkg.in/yaml.v3"
)

const (
	pluginVersion = "0.1.0"
	defaultSuffix = "-jailbreak"
	schemaVersion = 2
)

type pluginConfig struct {
	Suffix   string          `yaml:"suffix"`
	Unmapped string          `yaml:"unmapped"`
	Profiles []profileConfig `yaml:"profiles"`
}

type profileConfig struct {
	Name     string   `yaml:"name"`
	Models   []string `yaml:"models"`
	Mode     string   `yaml:"mode"`
	Prompt   string   `yaml:"prompt"`
	matchers []*regexp.Regexp
}

var (
	cfgMu sync.RWMutex
	cfg   = pluginConfig{
		Suffix:   defaultSuffix,
		Unmapped: "error",
	}
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type registrationRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type interceptRequest struct {
	RequestID      string         `json:"RequestID"`
	TraceID        string         `json:"TraceID"`
	SourceFormat   string         `json:"SourceFormat"`
	ToFormat       string         `json:"ToFormat"`
	Model          string         `json:"Model"`
	RequestedModel string         `json:"RequestedModel"`
	Stream         bool           `json:"Stream"`
	Headers        http.Header    `json:"Headers"`
	Body           []byte         `json:"Body"`
	CallerScope    string         `json:"CallerScope"`
	Metadata       map[string]any `json:"Metadata"`
}

type interceptResponse struct {
	Path            string      `json:"Path,omitempty"`
	Headers         http.Header `json:"Headers,omitempty"`
	Body            []byte      `json:"Body,omitempty"`
	ClearHeaders    []string    `json:"ClearHeaders,omitempty"`
	Terminate       bool        `json:"Terminate,omitempty"`
	StatusCode      int         `json:"StatusCode,omitempty"`
	ResponseHeaders http.Header `json:"ResponseHeaders,omitempty"`
	ResponseBody    []byte      `json:"ResponseBody,omitempty"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(_ *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
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
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
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

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		if err := configure(request); err != nil {
			return nil, err
		}
		return okEnvelope(pluginRegistration())
	case "request.intercept_before":
		return interceptBefore(request)
	case "request.intercept_after":
		// The body was already rewritten before credential selection. Do not
		// touch the translated upstream payload a second time.
		return okEnvelope(interceptResponse{})
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configure(raw []byte) error {
	var req registrationRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return err
		}
	}
	if req.SchemaVersion > 0 && req.SchemaVersion < schemaVersion {
		return fmt.Errorf("jailbreak-inject requires host schema version %d or newer", schemaVersion)
	}

	next := pluginConfig{
		Suffix:   defaultSuffix,
		Unmapped: "error",
	}
	if len(req.ConfigYAML) > 0 {
		if err := yaml.Unmarshal(req.ConfigYAML, &next); err != nil {
			return err
		}
	}
	next.Suffix = strings.TrimSpace(next.Suffix)
	if next.Suffix == "" || strings.ContainsAny(next.Suffix, " \t\r\n") {
		return fmt.Errorf("suffix must be non-empty and contain no whitespace")
	}
	next.Unmapped = strings.ToLower(strings.TrimSpace(next.Unmapped))
	if next.Unmapped == "" {
		next.Unmapped = "error"
	}
	if next.Unmapped != "error" && next.Unmapped != "pass" {
		return fmt.Errorf("unmapped must be error or pass, got %q", next.Unmapped)
	}

	seenNames := make(map[string]struct{}, len(next.Profiles))
	for index := range next.Profiles {
		profile := &next.Profiles[index]
		profile.Name = strings.TrimSpace(profile.Name)
		if profile.Name == "" {
			return fmt.Errorf("profiles[%d].name is required", index)
		}
		if _, exists := seenNames[profile.Name]; exists {
			return fmt.Errorf("duplicate profile name %q", profile.Name)
		}
		seenNames[profile.Name] = struct{}{}
		if len(profile.Models) == 0 {
			return fmt.Errorf("profile %q has no models", profile.Name)
		}
		profile.Mode = strings.ToLower(strings.TrimSpace(profile.Mode))
		if profile.Mode == "" {
			profile.Mode = "prepend"
		}
		switch profile.Mode {
		case "prepend", "append", "replace":
		default:
			return fmt.Errorf("profile %q has unknown mode %q", profile.Name, profile.Mode)
		}
		profile.Prompt = strings.TrimSpace(profile.Prompt)
		if profile.Prompt == "" {
			return fmt.Errorf("profile %q prompt is empty", profile.Name)
		}
		profile.matchers = make([]*regexp.Regexp, 0, len(profile.Models))
		for _, pattern := range profile.Models {
			re, err := globToRegexp(pattern)
			if err != nil {
				return fmt.Errorf("profile %q model pattern %q: %w", profile.Name, pattern, err)
			}
			profile.matchers = append(profile.matchers, re)
		}
	}

	cfgMu.Lock()
	cfg = next
	cfgMu.Unlock()
	return nil
}

func pluginRegistration() map[string]any {
	return map[string]any{
		"schema_version": schemaVersion,
		"metadata": map[string]any{
			"Name":             "jailbreak-inject",
			"Version":          pluginVersion,
			"Author":           "orca-ai-pool",
			"GitHubRepository": "https://github.com/AzureLandin/orca-ai-pool",
			"ConfigFields": []map[string]any{
				{"Name": "suffix", "Type": "string", "Description": "Model-name suffix that marks a jailbreak alias (default -jailbreak)"},
				{"Name": "unmapped", "Type": "enum", "EnumValues": []string{"error", "pass"}, "Description": "Behavior when no profile matches the requested model"},
				{"Name": "profiles", "Type": "array", "Description": "Model patterns mapped to prompt text and injection mode"},
			},
		},
		"capabilities": map[string]any{
			"request_interceptor": true,
		},
	}
}

func interceptBefore(raw []byte) ([]byte, error) {
	var req interceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}

	cfgMu.RLock()
	current := cfg
	cfgMu.RUnlock()

	baseModel, isJailbreak := stripSuffix(firstNonEmpty(req.RequestedModel, req.Model), current.Suffix)
	if !isJailbreak {
		return okEnvelope(interceptResponse{})
	}

	profile, err := selectProfile(current, baseModel)
	if err != nil {
		if current.Unmapped == "pass" {
			return okEnvelope(interceptResponse{})
		}
		return terminatedResponse(http.StatusBadRequest, err.Error(), req.SourceFormat)
	}

	updated, changed, supported := injectPrompt(req.Body, profile.Prompt, profile.Mode)
	if !supported {
		return terminatedResponse(
			http.StatusBadRequest,
			"unsupported request body for jailbreak injection (expected messages, system, instructions, or input)",
			req.SourceFormat,
		)
	}
	if !changed {
		return okEnvelope(interceptResponse{})
	}
	return okEnvelope(interceptResponse{Body: updated})
}

func stripSuffix(model, suffix string) (string, bool) {
	model = strings.TrimSpace(model)
	suffix = strings.TrimSpace(suffix)
	if model == "" || suffix == "" || len(model) <= len(suffix) {
		return "", false
	}
	if !strings.EqualFold(model[len(model)-len(suffix):], suffix) {
		return "", false
	}
	base := strings.TrimSpace(model[:len(model)-len(suffix)])
	if base == "" {
		return "", false
	}
	return base, true
}

func selectProfile(current pluginConfig, model string) (*profileConfig, error) {
	candidate := strings.TrimSpace(model)
	for index := range current.Profiles {
		for _, pattern := range current.Profiles[index].Models {
			if strings.EqualFold(pattern, candidate) {
				return &current.Profiles[index], nil
			}
		}
	}
	for index := range current.Profiles {
		for _, matcher := range current.Profiles[index].matchers {
			if matcher.MatchString(strings.ToLower(candidate)) {
				return &current.Profiles[index], nil
			}
		}
	}
	return nil, fmt.Errorf("no jailbreak profile matches model %q", candidate)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func globToRegexp(pattern string) (*regexp.Regexp, error) {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	if pattern == "" {
		return nil, fmt.Errorf("empty pattern")
	}
	var builder strings.Builder
	builder.WriteString("^")
	for _, char := range pattern {
		switch char {
		case '*':
			builder.WriteString(".*")
		case '?':
			builder.WriteString(".")
		default:
			builder.WriteString(regexp.QuoteMeta(string(char)))
		}
	}
	builder.WriteString("$")
	return regexp.Compile(builder.String())
}

// injectPrompt returns (body, changed, supported). supported is false when the
// JSON body has no known system-prompt surface; changed is false when the
// profile is already present (so retries do not duplicate it).
func injectPrompt(body []byte, prompt, mode string) ([]byte, bool, bool) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return body, false, false
	}

	if rawSystem, ok := root["system"]; ok {
		updated, changed, err := injectSystemValue(rawSystem, prompt, mode)
		if err != nil {
			return body, false, false
		}
		if !changed {
			return body, false, true
		}
		root["system"] = updated
		return marshalBody(root)
	}
	if rawInstructions, ok := root["instructions"]; ok {
		updated, changed, err := injectSystemValue(rawInstructions, prompt, mode)
		if err != nil {
			return body, false, false
		}
		if !changed {
			return body, false, true
		}
		root["instructions"] = updated
		return marshalBody(root)
	}
	if rawMessages, ok := root["messages"]; ok {
		updated, changed, err := injectOpenAIMessages(rawMessages, prompt, mode)
		if err != nil {
			return body, false, false
		}
		if !changed {
			return body, false, true
		}
		root["messages"] = updated
		return marshalBody(root)
	}
	if _, ok := root["input"]; ok {
		rawPrompt, _ := json.Marshal(prompt)
		root["instructions"] = rawPrompt
		return marshalBody(root)
	}
	return body, false, false
}

func marshalBody(root map[string]json.RawMessage) ([]byte, bool, bool) {
	out, err := json.Marshal(root)
	if err != nil {
		return nil, false, false
	}
	return out, true, true
}

func injectOpenAIMessages(raw json.RawMessage, prompt, mode string) (json.RawMessage, bool, error) {
	var messages []json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		return raw, false, err
	}
	type parsedMessage struct {
		role    string
		content json.RawMessage
	}
	parsed := make([]parsedMessage, len(messages))
	for index, message := range messages {
		var fields struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(message, &fields); err != nil {
			return raw, false, err
		}
		parsed[index] = parsedMessage{role: fields.Role, content: fields.Content}
	}

	hasPrompt := func(content json.RawMessage) bool {
		var text string
		return json.Unmarshal(content, &text) == nil && text == prompt
	}
	systemCount := 0
	containsPrompt := false
	for _, message := range parsed {
		if message.role == "system" || message.role == "developer" {
			systemCount++
			if hasPrompt(message.content) {
				containsPrompt = true
			}
		}
	}

	switch mode {
	case "prepend":
		if containsPrompt {
			return raw, false, nil
		}
		newMessage, _ := json.Marshal(map[string]any{"role": "system", "content": prompt})
		out := make([]json.RawMessage, 0, len(messages)+1)
		out = append(out, newMessage)
		out = append(out, messages...)
		encoded, err := json.Marshal(out)
		return encoded, err == nil, err
	case "append":
		if containsPrompt {
			return raw, false, nil
		}
		newMessage, _ := json.Marshal(map[string]any{"role": "system", "content": prompt})
		lastSystem := -1
		for index, message := range parsed {
			if message.role == "system" || message.role == "developer" {
				lastSystem = index
			}
		}
		insertAt := lastSystem + 1
		out := make([]json.RawMessage, 0, len(messages)+1)
		out = append(out, messages[:insertAt]...)
		out = append(out, newMessage)
		out = append(out, messages[insertAt:]...)
		encoded, err := json.Marshal(out)
		return encoded, err == nil, err
	case "replace":
		if systemCount == 1 && containsPrompt {
			return raw, false, nil
		}
		firstSystem := -1
		withoutSystem := make([]json.RawMessage, 0, len(messages))
		insertAt := 0
		keptBeforeFirst := 0
		for index, message := range messages {
			if parsed[index].role == "system" || parsed[index].role == "developer" {
				if firstSystem < 0 {
					firstSystem = index
					keptBeforeFirst = len(withoutSystem)
				}
				continue
			}
			withoutSystem = append(withoutSystem, message)
		}
		if firstSystem >= 0 {
			insertAt = keptBeforeFirst
		}
		newMessage, _ := json.Marshal(map[string]any{"role": "system", "content": prompt})
		out := make([]json.RawMessage, 0, len(withoutSystem)+1)
		out = append(out, withoutSystem[:insertAt]...)
		out = append(out, newMessage)
		out = append(out, withoutSystem[insertAt:]...)
		encoded, err := json.Marshal(out)
		return encoded, err == nil, err
	default:
		return raw, false, fmt.Errorf("unknown mode %q", mode)
	}
}

func injectSystemValue(raw json.RawMessage, prompt, mode string) (json.RawMessage, bool, error) {
	if strings.TrimSpace(string(raw)) == "null" {
		return marshalString(prompt), true, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == prompt {
			return raw, false, nil
		}
		switch mode {
		case "prepend":
			return marshalString(prompt + "\n\n" + text), true, nil
		case "append":
			return marshalString(text + "\n\n" + prompt), true, nil
		case "replace":
			return marshalString(prompt), true, nil
		default:
			return raw, false, fmt.Errorf("unknown mode %q", mode)
		}
	}

	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err == nil {
		containsPrompt := false
		for _, block := range blocks {
			var fields struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(block, &fields) == nil && fields.Type == "text" && fields.Text == prompt {
				containsPrompt = true
				break
			}
		}
		if mode != "replace" && containsPrompt {
			return raw, false, nil
		}
		newBlock, _ := json.Marshal(map[string]any{"type": "text", "text": prompt})
		switch mode {
		case "prepend":
			out := make([]json.RawMessage, 0, len(blocks)+1)
			out = append(out, newBlock)
			out = append(out, blocks...)
			encoded, err := json.Marshal(out)
			return encoded, err == nil, err
		case "append":
			out := make([]json.RawMessage, 0, len(blocks)+1)
			out = append(out, blocks...)
			out = append(out, newBlock)
			encoded, err := json.Marshal(out)
			return encoded, err == nil, err
		case "replace":
			out := []json.RawMessage{newBlock}
			encoded, err := json.Marshal(out)
			return encoded, err == nil, err
		default:
			return raw, false, fmt.Errorf("unknown mode %q", mode)
		}
	}
	return raw, false, fmt.Errorf("system value is neither a string nor a content block array")
}

func marshalString(value string) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

func terminatedResponse(statusCode int, message, sourceFormat string) ([]byte, error) {
	var body []byte
	if strings.Contains(strings.ToLower(sourceFormat), "anthropic") ||
		strings.Contains(strings.ToLower(sourceFormat), "claude") {
		body, _ = json.Marshal(map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "invalid_request_error",
				"message": message,
			},
		})
	} else {
		body, _ = json.Marshal(map[string]any{
			"error": map[string]any{
				"message": message,
				"type":    "invalid_request_error",
				"code":    "jailbreak_profile_error",
			},
		})
	}
	headers := http.Header{"Content-Type": {"application/json"}}
	return okEnvelope(interceptResponse{
		Terminate:       true,
		StatusCode:      statusCode,
		ResponseHeaders: headers,
		ResponseBody:    body,
	})
}

func okEnvelope(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, err := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	if err != nil {
		return []byte(`{"ok":false,"error":{"code":"plugin_error","message":"encode error"}}`)
	}
	return raw
}

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
