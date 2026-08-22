package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestWindowSlidingLimit(t *testing.T) {
	w := &window{}
	now := time.Unix(1000, 0)
	if !w.allow(now, 2) || !w.allow(now.Add(time.Second), 2) {
		t.Fatal("first two admissions should pass")
	}
	if w.allow(now.Add(2*time.Second), 2) {
		t.Fatal("third admission inside the window should be rejected")
	}
	if !w.allow(now.Add(time.Minute+time.Second), 2) {
		t.Fatal("expired admissions should leave the window")
	}
}

func TestLimitPrecedence(t *testing.T) {
	cfg := pluginConfig{DefaultRPM: 10, Providers: map[string]int{"codex": 20}, Auths: map[string]int{"a": 30}}
	if got := limitFor(cfg, pluginapi.SchedulerAuthCandidate{ID: "a", Provider: "codex"}); got != 30 {
		t.Fatalf("auth override=%d", got)
	}
	if got := limitFor(cfg, pluginapi.SchedulerAuthCandidate{ID: "b", Provider: "codex"}); got != 20 {
		t.Fatalf("provider override=%d", got)
	}
	if got := limitFor(cfg, pluginapi.SchedulerAuthCandidate{ID: "c", Provider: "openai"}); got != 10 {
		t.Fatalf("default=%d", got)
	}
}

func TestConfigureRejectsNegativeLimits(t *testing.T) {
	raw, err := json.Marshal(lifecycleRequest{ConfigYAML: []byte("default_rpm: -1\n")})
	if err != nil {
		t.Fatal(err)
	}
	if err := configure(raw); err == nil {
		t.Fatal("negative default_rpm was accepted")
	}
}

func TestConfigureNormalizesProviderKeysWithoutMutatingWhileRanging(t *testing.T) {
	raw, err := json.Marshal(lifecycleRequest{ConfigYAML: []byte("providers:\n  Codex: 10\nauths:\n  account-1: 20\n")})
	if err != nil {
		t.Fatal(err)
	}
	if err := configure(raw); err != nil {
		t.Fatal(err)
	}
	cfg := loaded()
	if cfg.Providers["codex"] != 10 || len(cfg.Providers) != 1 {
		t.Fatalf("providers = %#v", cfg.Providers)
	}
}

func TestRateLimitErrorCarriesRetryableHTTPStatus(t *testing.T) {
	if err := configure(testJSON(lifecycleRequest{ConfigYAML: []byte("default_rpm: 1\n")})); err != nil {
		t.Fatal(err)
	}
	request := testJSON(pluginapi.SchedulerPickRequest{
		Provider:   "codex",
		Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "limited-account", Provider: "codex"}},
	})
	if _, err := pick(request); err != nil {
		t.Fatal(err)
	}
	raw, err := pick(request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"http_status":429`) || !strings.Contains(string(raw), `"retryable":true`) {
		t.Fatalf("rate-limit response = %s", raw)
	}
}

func TestManagementRegistrationExposesMenuAndProtectedSettingsRoutes(t *testing.T) {
	raw, err := handleMethod(pluginabi.MethodManagementRegister, nil)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{"/menu", "Provider Rate Limiter", "GET", "PUT", "/plugins/provider-rate-limiter/settings"} {
		if !strings.Contains(text, want) {
			t.Fatalf("management registration missing %q: %s", want, text)
		}
	}
}

func TestManagementMenuProvidesAccountSelectorUI(t *testing.T) {
	html := menuHTML()
	for _, want := range []string{"/v0/management/auth-files", "accountRows", "auth-limit", "providerFilter", "Save runtime settings"} {
		if !strings.Contains(html, want) {
			t.Fatalf("management menu missing %q", want)
		}
	}
}

func TestManagementSettingsCanUpdateRuntimeConfig(t *testing.T) {
	body := testJSON(pluginConfig{DefaultRPM: 77, Providers: map[string]int{"Codex": 88}, Auths: map[string]int{"account-a": 99}})
	raw, err := testJSONBytes(managementRequest{Method: "PUT", Path: "/v0/management/plugins/provider-rate-limiter/settings", Body: body})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handleManagement(raw); err != nil {
		t.Fatal(err)
	}
	got := loaded()
	if got.DefaultRPM != 77 || got.Providers["codex"] != 88 || got.Auths["account-a"] != 99 {
		t.Fatalf("runtime config = %#v", got)
	}
}

func testJSON(v any) []byte {
	raw, err := testJSONBytes(v)
	if err != nil {
		panic(err)
	}
	return raw
}

func testJSONBytes(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return raw, nil
}
