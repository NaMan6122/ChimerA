package kimiweb

// Tests in three layers, per the spec-012 convention:
//  1. the parser against the LIVE-captured Connect frames (testdata/*.frames) —
//     the DeepSeek lesson: fixtures first, parser second;
//  2. the op semantics (set replaces, append appends) as a unit;
//  3. the provider over an httptest double, including rotation and continuity.

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/providers/webapi"
)

// Ids captured live on 2026-11-13 (logs/kimi-capture{,2}.txt).
const (
	fixtureChat    = "1a1d5296-1152-8b41-8000-0956b5b1ff47"
	fixtureParent1 = "1a1d5296-1152-8b42-8000-0a56db80e04b"
	fixtureParent2 = "1a1d5299-4cd2-8edd-8000-0a56e6ca9af0"
)

func TestConsumeFramesTurn1(t *testing.T) {
	raw, err := os.ReadFile("testdata/turn1.frames")
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{}
	res, err := c.consumeFrames(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("consumeFrames: %v", err)
	}
	if res.Content != "PONG" {
		t.Errorf("content = %q, want PONG", res.Content)
	}
	if res.ChatID != fixtureChat {
		t.Errorf("chat id = %q, want %q", res.ChatID, fixtureChat)
	}
	if res.ParentID != fixtureParent1 {
		t.Errorf("parent id = %q, want %q", res.ParentID, fixtureParent1)
	}
	if res.Reasoning == "" {
		t.Error("reasoning empty — block.think not accumulated")
	}
}

func TestConsumeFramesTurn2Continuity(t *testing.T) {
	raw, err := os.ReadFile("testdata/turn2.frames")
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{}
	res, err := c.consumeFrames(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("consumeFrames: %v", err)
	}
	// The wire never carried the answer word on turn 2 — the model recalled it
	// server-side from chat_id+parent_id. The parser must not lose that.
	if res.Content != "PONG" {
		t.Errorf("content = %q, want PONG", res.Content)
	}
	if res.ChatID != fixtureChat {
		t.Errorf("chat id = %q, want %q (same thread)", res.ChatID, fixtureChat)
	}
	if res.ParentID != fixtureParent2 {
		t.Errorf("parent id = %q, want %q (new assistant message)", res.ParentID, fixtureParent2)
	}
	if !strings.Contains(res.Reasoning, "PONG") {
		t.Errorf("reasoning should reference the recalled word: %q", res.Reasoning)
	}
}

func TestAppendOrSetSemantics(t *testing.T) {
	var b strings.Builder
	appendOrSet(&b, "set", true, "The")
	appendOrSet(&b, "append", true, " user")
	appendOrSet(&b, "append", true, " asks")
	if got := b.String(); got != "The user asks" {
		t.Errorf("append accumulation = %q, want %q", got, "The user asks")
	}
	// set REPLACES — the live capture opens block.think with a set, so a
	// parser that always appends would duplicate the first fragment.
	appendOrSet(&b, "set", true, "PONG")
	if got := b.String(); got != "PONG" {
		t.Errorf("set did not replace: %q", got)
	}
	// A frame that does not carry the field leaves the accumulator alone.
	appendOrSet(&b, "append", false, "ignored")
	if got := b.String(); got != "PONG" {
		t.Errorf("absent field mutated the accumulator: %q", got)
	}
}

func TestProviderEndToEnd(t *testing.T) {
	fake := &fakeKimi{}
	srv := newFakeKimiServer(t, fake)
	defer srv.Close()

	dir := t.TempDir()
	cfg := &config.Config{
		Provider:        "kimi",
		KimiWebModel:    "auto",
		KimiWebURL:      srv.URL,
		AuthDir:         dir,
		ResponseTimeout: 10 * time.Second,
		LogDir:          dir,
		LogLevel:        "error",
	}
	// The real harvested session carries the cookie jar alongside the refresh
	// credential — mirror that (LoadSession rejects token-only sessions).
	if err := webapi.WriteSession(&webapi.Session{
		AccessToken: "refresh-original",
		Cookies:     map[string]string{"kimi_session": "harvested"},
	}, cfg.SessionPath("kimi")); err != nil {
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

	// Turn 2 on the SAME thread must continue the SAME chat, hanging off the
	// assistant message turn 1 produced.
	res2, err := p.SendMessage("and now?", "thread-1")
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if res2.Message != "PONG" {
		t.Errorf("turn 2 message = %q", res2.Message)
	}
	bodies := fake.bodies()
	if len(bodies) < 2 {
		t.Fatalf("bodies seen = %d", len(bodies))
	}
	if !strings.Contains(bodies[1], `"chat_id":"`+fixtureChat+`"`) {
		t.Errorf("turn 2 body missing chat_id:\n%.240s", bodies[1])
	}
	if !strings.Contains(bodies[1], `"parent_id":"`+fixtureParent1+`"`) {
		t.Errorf("turn 2 body missing parent_id:\n%.240s", bodies[1])
	}
	if strings.Contains(bodies[0], `"chat_id"`) {
		t.Errorf("turn 1 unexpectedly carried a chat id:\n%.240s", bodies[0])
	}

	// Rotation: the server hands back a new refresh token; the provider
	// persists it so restarts keep working.
	if got := fake.refreshes(); got != 1 {
		t.Errorf("refresh hits = %d, want 1 (token cached across turns)", got)
	}
	stored, err := webapi.LoadSession(cfg.SessionPath("kimi"))
	if err != nil {
		t.Fatal(err)
	}
	if stored.AccessToken != "refresh-rotated" {
		t.Errorf("session refresh token = %q, want the rotated one", stored.AccessToken)
	}

	// Self-heal: a dead chat pointer retries once on a fresh chat.
	p.convs.Set("thread-2", convRef{chatID: "poison", parentID: "stale"})
	if _, err := p.SendMessage("again", "thread-2"); err != nil {
		t.Fatalf("self-heal turn: %v", err)
	}
	if err := p.NewChat(); err != nil {
		t.Fatalf("NewChat: %v", err)
	}
	if p.convs.Len() != 0 {
		t.Fatalf("NewChat did not clear kimi conversations: len=%d", p.convs.Len())
	}
}
