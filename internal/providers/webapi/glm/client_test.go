package glmweb

// Tests in three layers, per the spec-012 convention:
//  1. the parser against the LIVE-captured SSE fixtures (testdata/*.sse) —
//     the DeepSeek lesson: fixtures first, parser second;
//  2. the mint's internal integrity — the live server remains the final
//     oracle (a capture of an accepted mint is the external evidence);
//  3. the provider over an httptest double, including session rotation and
//     continuity.

import (
	"crypto/md5"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/models"
	"github.com/chimera/chimera/internal/providers/webapi"
)

const fixtureConv = "6af6f2dd80db565e2b844067"

func TestConsumeStreamFixtureTurn1(t *testing.T) {
	raw, err := os.ReadFile("testdata/turn1.sse")
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{}
	res, err := c.consumeStream(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("consumeStream: %v", err)
	}
	if res.Content != "PONG" {
		t.Errorf("content = %q, want PONG (snapshot semantics: appending would duplicate)", res.Content)
	}
	if res.ConversationID != fixtureConv {
		t.Errorf("conversation_id = %q, want %q", res.ConversationID, fixtureConv)
	}
	if res.Reasoning == "" {
		t.Error("reasoning empty — think parts not parsed")
	}
}

func TestConsumeStreamFixtureTurn2Continuity(t *testing.T) {
	raw, err := os.ReadFile("testdata/turn2.sse")
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{}
	res, err := c.consumeStream(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("consumeStream: %v", err)
	}
	// The wire never carried the answer word on turn 2 — the model recalled
	// it server-side. The parser must not lose that.
	if res.Content != "PONG" {
		t.Errorf("content = %q, want PONG", res.Content)
	}
	if res.ConversationID != fixtureConv {
		t.Errorf("conversation_id = %q, want %q (same thread)", res.ConversationID, fixtureConv)
	}
	if !strings.Contains(res.Reasoning, "PONG") {
		t.Errorf("reasoning should reference the recalled word: %q", res.Reasoning)
	}
}

func TestSignMintShape(t *testing.T) {
	now := time.UnixMilli(1731487200123)
	ts, nonce, sign := signMint(now)
	if len(nonce) != 32 {
		t.Errorf("nonce = %q, want 32 hex chars", nonce)
	}
	// The mint must be reproducible from its own public material — a
	// verifier (the server, or a test) recomputes it from ts+nonce+SECRET.
	expect := fmt.Sprintf("%x", md5.Sum([]byte(fmt.Sprintf("%d-%s-%s", ts, nonce, signSecret))))
	if sign != expect {
		t.Errorf("sign not reproducible from (ts,nonce): %s vs %s", sign, expect)
	}
	// The mangle changes only the second-to-last digit, and the new digit is
	// (digit-sum − that digit) mod 10.
	raw := strconv.FormatInt(now.UnixMilli(), 10)
	got := strconv.FormatInt(ts, 10)
	if got[:11] != raw[:11] || got[12:] != raw[12:] {
		t.Errorf("mangle moved other digits: %s -> %s", raw, got)
	}
	sum := 0
	for _, c := range raw {
		sum += int(c - '0')
	}
	want := string(rune('0' + (sum-int(raw[11]-'0'))%10))
	if string(got[11]) != want {
		t.Errorf("mangled digit = %q, want %q", got[11], want)
	}
}

func TestAssistantAndModeSelectors(t *testing.T) {
	cases := []struct{ model, assistant, mode string }{
		{"auto", defaultAssistant, "zero"},
		{"think", defaultAssistant, "zero"},
		{"deepresearch", defaultAssistant, "deep_research"},
		{"65940acff94777010aa6b797", "65940acff94777010aa6b797", "zero"},
	}
	for _, tc := range cases {
		c := &Client{Model: tc.model}
		a, m := c.assistantAndMode()
		if a != tc.assistant || m != tc.mode {
			t.Errorf("model %q → (%q,%q), want (%q,%q)", tc.model, a, m, tc.assistant, tc.mode)
		}
	}
}

// fakeGLM is an httptest double for the two endpoints, asserting the header
// mint and the turn body, and replaying the captured fixtures.
type fakeGLM struct {
	seenSigns   []string
	seenBodies  []string
	refreshHits int
}

func TestProviderEndToEnd(t *testing.T) {
	fake := &fakeGLM{}
	srv := newFakeGLMServer(t, fake)
	defer srv.Close()

	dir := t.TempDir()
	cfg := &config.Config{
		Provider:        "glm",
		GLMWebModel:     "auto",
		GLMURL:          srv.URL,
		AuthDir:         dir,
		ResponseTimeout: 10 * time.Second,
		LogDir:          dir,
		LogLevel:        "error",
	}
	// The real harvested session carries the cookie jar alongside the
	// refresh credential — mirror that (LoadSession rejects token-only
	// sessions by design).
	if err := webapi.WriteSession(&webapi.Session{
		AccessToken: "refresh-original",
		Cookies:     map[string]string{"chatglm_refresh_token": "refresh-original"},
	}, cfg.SessionPath("glm")); err != nil {
		t.Fatal(err)
	}

	p := New(cfg)
	if err := p.Init(nil, cfg); err != nil {
		t.Fatalf("init: %v", err) // refresh must succeed on the fake
	}

	res, err := p.SendMessage("Reply with exactly: PONG", "thread-1")
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if res.Message != "PONG" {
		t.Errorf("turn 1 message = %q", res.Message)
	}

	// Turn 2 on the SAME thread must continue the SAME conversation.
	res2, err := p.SendMessage("and now?", "thread-1")
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if res2.Message != "PONG" {
		t.Errorf("turn 2 message = %q", res2.Message)
	}
	if len(fake.seenBodies) < 2 {
		t.Fatalf("bodies seen = %d", len(fake.seenBodies))
	}
	first, second := fake.seenBodies[0], fake.seenBodies[1]
	if !strings.Contains(second, `"conversation_id":"`+fixtureConv+`"`) {
		t.Errorf("turn 2 body missing continuity id:\n%.200s", second)
	}
	if strings.Contains(first, `"conversation_id":"`+fixtureConv+`"`) && first != second {
		// turn 1 was fresh (empty id) — assert it actually was:
		t.Errorf("turn 1 unexpectedly carried a conversation id:\n%.200s", first)
	}
	if !strings.Contains(first, `"conversation_id":""`) {
		t.Errorf("turn 1 body should carry an empty conversation id:\n%.200s", first)
	}

	// Session rotation: the server hands back a new refresh token; the
	// provider persists it so restarts keep working.
	if fake.refreshHits != 1 {
		t.Errorf("refresh hits = %d, want 1 (token cached across turns)", fake.refreshHits)
	}
	stored, err := webapi.LoadSession(cfg.SessionPath("glm"))
	if err != nil {
		t.Fatal(err)
	}
	if stored.AccessToken != "refresh-rotated" {
		t.Errorf("session refresh token = %q, want the rotated one", stored.AccessToken)
	}

	// Mint integrity: the server-side recomputation check.
	for _, s := range fake.seenSigns {
		parts := strings.SplitN(s, "|", 3)
		if len(parts) != 3 {
			t.Fatalf("bad captured sign record %q", s)
		}
		expect := fmt.Sprintf("%x", md5.Sum([]byte(parts[0]+"-"+parts[1]+"-"+signSecret)))
		if parts[2] != expect {
			t.Errorf("mint mismatch: got %s want %s (ts=%s nonce=%s)", parts[2], expect, parts[0], parts[1])
		}
	}

	ok, err := p.IsLoggedIn()
	if !ok || err != nil {
		t.Errorf("IsLoggedIn = %v, %v", ok, err)
	}
	var _ models.ProviderResponse // shape check
}
