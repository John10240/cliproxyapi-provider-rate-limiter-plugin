package main

import (
	"testing"
	"time"

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
