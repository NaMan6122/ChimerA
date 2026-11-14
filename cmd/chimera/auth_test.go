package main

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/providers/webapi"
	_ "github.com/chimera/chimera/internal/providers/webapi/all"
)

func TestAuthSpecsCoverEveryRegisteredWebAPIProvider(t *testing.T) {
	reg := webapi.Registered()
	got := authProviderNames()
	if len(got) != len(authSpecs) {
		t.Fatalf("authProviderNames len=%d, authSpecs len=%d", len(got), len(authSpecs))
	}

	sortedGot := append([]string(nil), got...)
	sort.Strings(sortedGot)
	if !reflect.DeepEqual(sortedGot, reg) {
		t.Fatalf("authSpecs keys %v do not match webapi.Registered() %v", sortedGot, reg)
	}

	supported := supportedAuthProviders()
	for _, name := range reg {
		if !strings.Contains(supported, name) {
			t.Fatalf("supportedAuthProviders() %q missing %q", supported, name)
		}
	}
}

func TestGLMAuthSpecStoresRawCookieValue(t *testing.T) {
	spec, ok := authSpecs[config.ProviderGLM]
	if !ok {
		t.Fatal("missing glm authSpec")
	}
	if spec.CookieName != "chatglm_refresh_token" {
		t.Fatalf("CookieName = %q, want chatglm_refresh_token", spec.CookieName)
	}
	if got := markerForCookie(spec, "raw-refresh-jwt"); got != "raw-refresh-jwt" {
		t.Fatalf("markerForCookie(glm) = %q, want raw cookie value", got)
	}

	// Cookie-custody specs with an explicit Marker still return the marker.
	gpt := authSpecs[config.ProviderChatGPT]
	if got := markerForCookie(gpt, "secret-cookie"); got != "cookie-session" {
		t.Fatalf("markerForCookie(chatgpt) = %q, want cookie-session", got)
	}
}

func TestKimiAuthSpecUsesKimiWebURLWithoutMutatingCallerConfig(t *testing.T) {
	cfg := &config.Config{
		Provider:   config.ProviderKimi,
		KimiURL:    "https://www.kimi.com",
		KimiWebURL: "https://www.kimi.ai",
	}
	spec, ok := authSpecs[config.ProviderKimi]
	if !ok {
		t.Fatal("missing kimi authSpec")
	}
	loginCfg, loginURL := loginConfigFor(cfg, spec)
	if loginURL != "https://www.kimi.ai" {
		t.Fatalf("loginURL = %q, want https://www.kimi.ai", loginURL)
	}
	if loginCfg.ProviderURL() != "https://www.kimi.ai" {
		t.Fatalf("loginCfg.ProviderURL() = %q, want https://www.kimi.ai", loginCfg.ProviderURL())
	}
	if cfg.KimiURL != "https://www.kimi.com" {
		t.Fatalf("caller cfg.KimiURL mutated to %q", cfg.KimiURL)
	}
}
