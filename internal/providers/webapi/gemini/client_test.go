package geminiweb

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/chimera/chimera/internal/providers/webapi"
)

func testSession() *webapi.Session {
	return &webapi.Session{
		AccessToken: "cookie-session",
		Cookies:     map[string]string{"__Secure-1PSID": "x", "SAPISID": "y"},
	}
}

const testTimeout = 30 * time.Second

// TestSAPISIDHASH pins the Authorization format to an externally computed
// vector (printf '1700000000 SAPISIDTEST https://gemini.google.com' |
// shasum -a 1) so a self-consistent but wrong construction cannot pass —
// the same rule spec 012 applied to the PoW solvers.
func TestSAPISIDHASH(t *testing.T) {
	got := sapisidAuth(1700000000, "SAPISIDTEST", "https://gemini.google.com")
	want := "SAPISIDHASH 1700000000_71764f844cb26f6366f9ae6e94c9bb3671d9a2de"
	if got != want {
		t.Fatalf("sapisidAuth mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestScrapeNonceExtraction(t *testing.T) {
	html := `<html><script>var WIZ_global_data = {"SNlM0e":"AFinitNonce42","cfb2h":"boq_assistant-bard-web-server_20260521.02_p1"};</script></html>`
	c := &Client{BaseURL: BaseURL}
	nonce := firstMatch(nonceRe, html)
	bl := firstMatch(blRe, html)
	if nonce != "AFinitNonce42" {
		t.Fatalf("nonce = %q", nonce)
	}
	if bl != "boq_assistant-bard-web-server_20260521.02_p1" {
		t.Fatalf("bl = %q", bl)
	}
	_ = c
}

func TestScrapeNonceMissing(t *testing.T) {
	html := `<html><script>var WIZ_global_data = {"FdrFJe":"-1"};</script></html>`
	if m := firstMatch(nonceRe, html); m != "" {
		t.Fatalf("expected no nonce, got %q", m)
	}
}

// frameLine builds one wire-format frame line: the inner array is passed
// through batchexecute's double encoding (inner JSON as a string inside the
// wrb.fr element).
func frameLine(t *testing.T, inner []any) string {
	t.Helper()
	innerJSON, err := json.Marshal(inner)
	if err != nil {
		t.Fatal(err)
	}
	frame := []any{[]any{"wrb.fr", nil, string(innerJSON), nil, "generic"}}
	line, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	return string(line)
}

func candidate(text any, status float64) []any {
	return []any{"rc_123", text, nil, nil, nil, nil, nil, nil, []any{status}}
}

func innerOf(cands []any, cid, rid string) []any {
	return []any{nil, []any{cid, rid}, nil, nil, cands}
}

// body assembles a StreamGenerate body: guard, length prefixes, frames.
func body(lines ...string) []byte {
	var b strings.Builder
	b.WriteString(")]}'\n\n")
	for _, l := range lines {
		b.WriteString("1234\n")
		b.WriteString(l)
		b.WriteString("\n")
	}
	return []byte(b.String())
}

func TestParseFramesCumulativeSnapshots(t *testing.T) {
	// Frame 1: short snapshot, still running. Frame 2: longer cumulative
	// snapshot, done. The answer must be replaced, not appended.
	f1 := frameLine(t, innerOf([]any{candidate("Hello", 1)}, "c_abc", "r_def"))
	f2 := frameLine(t, innerOf([]any{candidate("Hello world, this is Gemini speaking.", 2)}, "c_abc", "r_def"))
	res, err := parseFrames(body(f1, f2))
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "Hello world, this is Gemini speaking." {
		t.Fatalf("content = %q (snapshot must replace, never append)", res.Content)
	}
	if res.ConversationID != "c_abc" || res.ResponseID != "r_def" || res.CandidateID != "rc_123" {
		t.Fatalf("ids = %s/%s/%s", res.ConversationID, res.ResponseID, res.CandidateID)
	}
	if res.Status != "done" {
		t.Fatalf("status = %q", res.Status)
	}
}

func TestParseFramesLongestCandidateWins(t *testing.T) {
	// n0madic's rule: within a frame, the longest string across the
	// candidate's [1] slots is the snapshot; alternates are truncations.
	f := frameLine(t, innerOf([]any{
		[]any{"rc_1", []any{"short"}, nil, nil, nil, nil, nil, nil, []any{2}},
		[]any{"rc_2", []any{"the much longer primary answer text"}, nil, nil, nil, nil, nil, nil, []any{2}},
	}, "c_abc", "r_def"))
	res, err := parseFrames(body(f))
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "the much longer primary answer text" {
		t.Fatalf("content = %q", res.Content)
	}
	if res.CandidateID != "rc_2" {
		t.Fatalf("rcid = %q (last candidate wins)", res.CandidateID)
	}
}

func TestParseFramesCardContentIsNotText(t *testing.T) {
	// A transient error card arrives as a card_content URL in the text slot;
	// it must never become the answer. With no other text and status done,
	// the turn is an error, not a success.
	f := frameLine(t, innerOf([]any{
		[]any{"rc_1", []any{"https://googleusercontent.com/card_content/xyz"}, nil, nil, nil, nil, nil, nil, []any{2}},
	}, "c_abc", "r_def"))
	_, err := parseFrames(body(f))
	if err == nil || !strings.Contains(err.Error(), "empty turn") {
		t.Fatalf("want empty-turn error, got %v", err)
	}
}

func TestParseFramesNoFrames(t *testing.T) {
	if _, err := parseFrames([]byte(")]}'\n\n[[\"other\"]]\n")); err == nil {
		t.Fatal("expected error for a body without wrb.fr frames")
	}
}

func TestBuildInnerContinuation(t *testing.T) {
	cont := buildInner("second question", TurnMeta{
		ConversationID: "c_a", ResponseID: "r_b", CandidateID: "rc_c", TurnIndex: 1,
	}, "uuid-1")
	if len(cont) != innerSlots {
		t.Fatalf("inner slots = %d, want %d", len(cont), innerSlots)
	}
	promptArr, ok := cont[0].([]any)
	if !ok || promptArr[0] != "second question" {
		t.Fatalf("inner[0] = %v", cont[0])
	}
	md, ok := cont[2].([]any)
	if !ok || md[0] != "c_a" || md[1] != "r_b" || md[2] != "rc_c" {
		t.Fatalf("inner[2] = %v (continuation triple)", cont[2])
	}
	turn, ok := cont[17].([]any)
	if !ok || len(turn) != 1 {
		t.Fatalf("inner[17] = %v", cont[17])
	}
	if idx, ok := turn[0].([]any); !ok || len(idx) != 1 || idx[0] != 1 {
		t.Fatalf("inner[17][0] = %v (turn index)", turn[0])
	}
	if cont[59] != "uuid-1" {
		t.Fatalf("inner[59] = %v (request uuid)", cont[59])
	}

	fresh := buildInner("first question", TurnMeta{}, "")
	md, ok = fresh[2].([]any)
	if !ok || md[0] != "" || md[1] != "" || md[2] != "" {
		t.Fatalf("fresh inner[2] = %v (must be the empty metadata)", md)
	}
	if fresh[59] != nil {
		t.Fatalf("fresh inner[59] = %v", fresh[59])
	}
}

func TestModelHeader(t *testing.T) {
	c := NewClient(testSession(), testTimeout, "flash", false)
	hdr, ok := c.modelHeader()
	if !ok {
		t.Fatal("flash must produce a header")
	}
	var arr []any
	if err := json.Unmarshal([]byte(hdr), &arr); err != nil {
		t.Fatalf("header not JSON: %v (%s)", err, hdr)
	}
	if arr[4] != "fbb127bbb056c959" {
		t.Fatalf("hdr[4] = %v", arr[4])
	}
	if arr[15] != float64(1) {
		t.Fatalf("hdr[15] = %v (thinking off → level 1)", arr[15])
	}

	c = NewClient(testSession(), testTimeout, "auto", true)
	if _, ok := c.modelHeader(); ok {
		t.Fatal("auto must omit the header")
	}

	c = NewClient(testSession(), testTimeout, "9d8ca3786ebdfbea", true)
	hdr, ok = c.modelHeader()
	if !ok {
		t.Fatal("raw hash must pass through")
	}
	if err := json.Unmarshal([]byte(hdr), &arr); err != nil {
		t.Fatal(err)
	}
	if arr[4] != "9d8ca3786ebdfbea" || arr[15] != float64(2) {
		t.Fatalf("raw hash / thinking level wrong: %v", arr)
	}
}
