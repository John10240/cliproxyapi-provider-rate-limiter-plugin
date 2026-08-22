package main

/*
#include <stdint.h>
#include <stdlib.h>
typedef struct { void* ptr; size_t len; } cliproxy_buffer;
typedef struct { uint32_t abi_version; void* host_ctx; void* call; void* free_buffer; } cliproxy_host_api;
typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);
typedef struct { uint32_t abi_version; cliproxy_plugin_call_fn call; cliproxy_plugin_free_fn free_buffer; cliproxy_plugin_shutdown_fn shutdown; } cliproxy_plugin_api;
extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

type pluginConfig struct {
	DefaultRPM int            `yaml:"default_rpm"`
	Providers  map[string]int `yaml:"providers"`
	Auths      map[string]int `yaml:"auths"`
}
type window struct {
	mu   sync.Mutex
	hits []time.Time
}

func (w *window) allow(now time.Time, rpm int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	cutoff := now.Add(-time.Minute)
	keep := w.hits[:0]
	for _, t := range w.hits {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	w.hits = keep
	if rpm <= 0 {
		return true
	}
	if len(w.hits) >= rpm {
		return false
	}
	w.hits = append(w.hits, now)
	return true
}

var currentConfig atomic.Value
var windows sync.Map

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}
type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Retryable  bool   `json:"retryable,omitempty"`
}
type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}
type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}
type registrationCapability struct {
	Scheduler bool `json:"scheduler"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(_ *C.cliproxy_host_api, p *C.cliproxy_plugin_api) C.int {
	if p == nil {
		return 1
	}
	p.abi_version = C.uint32_t(pluginabi.ABIVersion)
	p.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	p.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	p.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, n C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var b []byte
	if request != nil && n > 0 {
		b = C.GoBytes(unsafe.Pointer(request), C.int(n))
	}
	out, err := handleMethod(C.GoString(method), b)
	if err != nil {
		writeResponse(response, errorEnvelope("plugin_error", err.Error()))
		return 1
	}
	writeResponse(response, out)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}
func handleMethod(method string, raw []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configure(raw); err != nil {
			return nil, err
		}
		return okEnvelope(registrationData())
	case pluginabi.MethodSchedulerPick:
		return pick(raw)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}
func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return err
		}
	}
	cfg := pluginConfig{Providers: map[string]int{}, Auths: map[string]int{}}
	if len(req.ConfigYAML) > 0 {
		if err := yaml.Unmarshal(req.ConfigYAML, &cfg); err != nil {
			return err
		}
	}
	if cfg.DefaultRPM < 0 {
		return fmt.Errorf("default_rpm must be >= 0")
	}
	providers, err := normalizeLimits(cfg.Providers, true)
	if err != nil {
		return fmt.Errorf("providers: %w", err)
	}
	auths, err := normalizeLimits(cfg.Auths, false)
	if err != nil {
		return fmt.Errorf("auths: %w", err)
	}
	cfg.Providers = providers
	cfg.Auths = auths
	currentConfig.Store(cfg)
	return nil
}

func normalizeLimits(input map[string]int, lowerKeys bool) (map[string]int, error) {
	result := make(map[string]int, len(input))
	for rawKey, limit := range input {
		key := strings.TrimSpace(rawKey)
		if lowerKeys {
			key = strings.ToLower(key)
		}
		if key == "" {
			return nil, fmt.Errorf("empty key")
		}
		if limit < 0 {
			return nil, fmt.Errorf("%q has negative limit %d", key, limit)
		}
		if previous, exists := result[key]; exists && previous != limit {
			return nil, fmt.Errorf("duplicate key %q with conflicting limits", key)
		}
		result[key] = limit
	}
	return result, nil
}
func loaded() pluginConfig {
	if v := currentConfig.Load(); v != nil {
		return v.(pluginConfig)
	}
	return pluginConfig{Providers: map[string]int{}, Auths: map[string]int{}}
}
func limitFor(cfg pluginConfig, c pluginapi.SchedulerAuthCandidate) int {
	if n, ok := cfg.Auths[c.ID]; ok {
		return n
	}
	if n, ok := cfg.Providers[strings.ToLower(c.Provider)]; ok {
		return n
	}
	return cfg.DefaultRPM
}
func pick(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	cfg := loaded()
	now := time.Now()
	for _, c := range req.Candidates {
		if strings.TrimSpace(c.ID) == "" {
			continue
		}
		key := c.ID
		v, _ := windows.LoadOrStore(key, &window{})
		if v.(*window).allow(now, limitFor(cfg, c)) {
			return okEnvelope(pluginapi.SchedulerPickResponse{AuthID: c.ID, Handled: true})
		}
	}
	return errorEnvelopeWithStatus("provider_rate_limit_exceeded", fmt.Sprintf("all candidates for provider %q are over the configured rate limit", req.Provider), http.StatusTooManyRequests), nil
}
func registrationData() registration {
	return registration{SchemaVersion: pluginabi.SchemaVersion, Metadata: pluginapi.Metadata{Name: "provider-rate-limiter", Version: "0.2.0", Author: "lsmallice", GitHubRepository: "https://github.com/lsmallice/cliproxyapi-provider-rate-limiter-plugin", ConfigFields: []pluginapi.ConfigField{{Name: "default_rpm", Type: pluginapi.ConfigFieldTypeInteger, Description: "Default RPM applied independently to every candidate."}, {Name: "providers", Type: pluginapi.ConfigFieldTypeObject, Description: "Provider name to RPM override map."}, {Name: "auths", Type: pluginapi.ConfigFieldTypeObject, Description: "AuthID to RPM override map; overrides provider and default."}}}, Capabilities: registrationCapability{Scheduler: true}}
}
func okEnvelope(v any) ([]byte, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	return json.Marshal(envelope{OK: true, Result: b})
}
func errorEnvelope(code, msg string) []byte {
	return errorEnvelopeWithStatus(code, msg, 0)
}

func errorEnvelopeWithStatus(code, msg string, status int) []byte {
	b, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: msg, HTTPStatus: status, Retryable: status == http.StatusTooManyRequests}})
	return b
}
func writeResponse(r *C.cliproxy_buffer, b []byte) {
	if r == nil || len(b) == 0 {
		return
	}
	p := C.CBytes(b)
	if p == nil {
		return
	}
	r.ptr = p
	r.len = C.size_t(len(b))
}
