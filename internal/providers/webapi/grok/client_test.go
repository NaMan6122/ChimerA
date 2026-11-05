package grokweb

import (
	"encoding/base64"
	"errors"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/chimera/chimera/internal/providers/webapi"
)

func testSession() *webapi.Session {
	return &webapi.Session{
		AccessToken: "cookie-session",
		Cookies:     map[string]string{"sso": "x", "sso-rw": "x", "cf_clearance": "y", "__cf_bm": "y"},
	}
}

const testTimeout = 30 * time.Second

// TestStatsigFallbackShape pins the accepted legacy fallback: base64(std,
// padded) of one of the two TypeError lines. Decoding the header must yield a
// recognizable grok statsig fallback, not arbitrary bytes.
func TestStatsigFallbackShape(t *testing.T) {
	r := rand.New(rand.NewSource(42)) // deterministic
	seenNull, seenUndef := false, false
	for i := 0; i < 64; i++ {
		hdr := statsigFallback(r)
		raw, err := base64.StdEncoding.DecodeString(hdr)
		if err != nil {
			t.Fatalf("not padded std base64: %v (%s)", err, hdr)
		}
		s := string(raw)
		switch {
		case strings.HasPrefix(s, "x1:TypeError: Cannot read properties of null (reading 'children[\"") && strings.HasSuffix(s, "\"]')"):
			seenNull = true
		case strings.HasPrefix(s, "x1:TypeError: Cannot read properties of undefined (reading '") && strings.HasSuffix(s, "')"):
			seenUndef = true
		default:
			t.Fatalf("unexpected fallback shape: %q", s)
		}
	}
	if !seenNull || !seenUndef {
		t.Fatalf("generator must produce both shapes (null=%v undef=%v)", seenNull, seenUndef)
	}
}

// TestBuildPayloadKeys asserts the exact key set the web client sends, the
// modeId mapping, and that responseId appears only on continuation.
func TestBuildPayloadKeys(t *testing.T) {
	fresh := buildPayload("hello", "auto", "")
	want := []string{"collectionIds", "disabledConnectorIds", "deviceEnvInfo", "disableMemory",
		"disableSearch", "disableSelfHarmShortCircuit", "disableTextFollowUps", "enableImageGeneration",
		"enableImageStreaming", "enableSideBySide", "fileAttachments", "forceConcise", "forceSideBySide",
		"imageAttachments", "imageGenerationCount", "isAsyncChat", "message", "modeId", "responseMetadata",
		"returnImageBytes", "returnRawGrokInXaiRequest", "searchAllConnectors", "sendFinalMetadata",
		"temporary", "toolOverrides"}
	for _, k := range want {
		if _, ok := fresh[k]; !ok {
			t.Errorf("missing key %q (must match the web client's payload)", k)
		}
	}
	if _, ok := fresh["responseId"]; ok {
		t.Error("fresh payload must not carry responseId")
	}
	if fresh["temporary"] != false {
		t.Error("temporary must be false: X-Session-Id threads need server-persisted conversations")
	}
	if fresh["message"] != "hello" || fresh["modeId"] != "auto" {
		t.Errorf("message/modeId = %v/%v", fresh["message"], fresh["modeId"])
	}

	cont := buildPayload("next", "expert", "resp-123")
	if cont["responseId"] != "resp-123" {
		t.Errorf("continuation responseId = %v", cont["responseId"])
	}
	if cont["modeId"] != "expert" {
		t.Errorf("modeId = %v", cont["modeId"])
	}
}

// TestParseFrames walks a realistic body: mixed bare/data-prefixed lines,
// conversation id, chaining id, cumulative text tokens, thinking separated,
// tool cards skipped, soft stop honored.
func TestParseFrames(t *testing.T) {
	body := strings.Join([]string{
		`{"result":{"conversation":{"conversationId":"c-abc"}}}`,
		`data:{"result":{"response":{"userResponse":{"responseId":"r-001"}}}}`,
		`{"result":{"response":{"token":"Hel","messageTag":"final"}}}`,
		`data: {"result":{"response":{"token":"lo","messageTag":"final"}}}`,
		`{"result":{"response":{"token":"thinking…","isThinking":true}}}`,
		`{"result":{"response":{"token":"SKIP","messageTag":"tool_usage_card"}}}`,
		`{"result":{"response":{"isSoftStop":true}}}`,
		"", // trailing newline
	}, "\n")
	res, err := parseFrames([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if res.ConversationID != "c-abc" {
		t.Errorf("conversationID = %q", res.ConversationID)
	}
	if res.ResponseID != "r-001" {
		t.Errorf("responseID = %q (next turn chains from this)", res.ResponseID)
	}
	if res.Content != "Hello" {
		t.Errorf("content = %q (tokens append; tool cards skipped)", res.Content)
	}
	if res.Reasoning != "thinking…" {
		t.Errorf("reasoning = %q", res.Reasoning)
	}
	if !res.SoftStop {
		t.Error("soft stop not recorded")
	}
}

// TestParseFramesContinuationShape covers the conversations/{cid}/responses
// framing: the same fields sit directly under result (measured — this is the
// shape the first live continuation returned, which the v0 parser missed).
func TestParseFramesContinuationShape(t *testing.T) {
	body := strings.Join([]string{
		`{"result":{"userResponse":{"responseId":"u-777","message":"What word?"}}}`,
		`{"result":{"token":"MEL","isThinking":false}}`,
		`{"result":{"token":"ON","isThinking":false}}`,
		`{"result":{"isSoftStop":true,"responseId":"resp-9"}}`,
	}, "\n")
	res, err := parseFrames([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "MELON" {
		t.Errorf("content = %q (continuation frames must parse)", res.Content)
	}
	if res.ResponseID != "u-777" {
		t.Errorf("responseID = %q", res.ResponseID)
	}
	if !res.SoftStop {
		t.Error("soft stop not recorded")
	}
}

func TestParseFramesNoFrames(t *testing.T) {
	if _, err := parseFrames([]byte("<html>blocked</html>")); err == nil {
		t.Fatal("expected error for a body without grok frames")
	}
}

// TestClassifyStatus maps the error vocabulary onto the transport classes.
func TestClassifyStatus(t *testing.T) {
	if err := classifyStatus(403, []byte(`{"error":{"code":7,"message":"Request rejected by anti-bot rules."}}`)); !errors.Is(err, webapi.ErrWAF) {
		t.Errorf("code 7 → ErrWAF, got %v", err)
	}
	if err := classifyStatus(200, []byte(`{"error":{"code":8,"message":"Too many requests"}}`)); err == nil {
		t.Fatal("in-band rate limit must error")
	} else {
		var he *webapi.HTTPError
		if !errors.As(err, &he) || he.Status != 429 {
			t.Errorf("in-band rate limit → 429, got %v", err)
		}
	}
	if err := classifyStatus(401, []byte(`bad credentials`)); !errors.Is(err, webapi.ErrSessionExpired) {
		t.Errorf("credential markers → ErrSessionExpired, got %v", err)
	}
	if err := classifyStatus(500, []byte(`oops`)); err == nil {
		t.Error("plain 500 must still error")
	}
}

// TestNewClientModeID: unknown model names fall back to the server's routing.
func TestNewClientModeID(t *testing.T) {
	if c := NewClient(testSession(), testTimeout, "expert"); c.modeID != "expert" {
		t.Errorf("expert → %q", c.modeID)
	}
	if c := NewClient(testSession(), testTimeout, "grok-4-turbo"); c.modeID != "auto" {
		t.Errorf("unknown → auto, got %q", c.modeID)
	}
	if _, ok := modeIDs["auto"]; !ok {
		t.Fatal("mode table drifted")
	}
}

// TestChainFromAssistantNode pins the measured rule: the follow-up responseId
// must be the model's node, not the user's — chaining to the userResponse id
// branches the thread sideways and the model loses all prior context
// (live-verified 2026-11-06).
func TestChainFromAssistantNode(t *testing.T) {
	body := strings.Join([]string{
		`{"result":{"conversation":{"conversationId":"c-1"}}}`,
		`{"result":{"response":{"userResponse":{"responseId":"user-1"}}}}`,
		`{"result":{"response":{"token":"Hi","messageTag":"final"}}}`,
		`{"result":{"response":{"modelResponse":{"responseId":"assistant-1","parentResponseId":"user-1"},"isSoftStop":true}}}`,
	}, "\n")
	res, err := parseFrames([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if res.ResponseID != "assistant-1" {
		t.Fatalf("responseID = %q — must be the assistant node", res.ResponseID)
	}
}
