package chatgptweb

import (
	"encoding/base64"
	"testing"
)

// Pin FNV-1a (32-bit) to the published reference vectors, so the hash at the
// centre of the proof is verified independently of the solver logic.
func TestFNV1aPublishedVectors(t *testing.T) {
	cases := map[string]uint32{
		"":       0x811c9dc5,
		"a":      0xe40c292c,
		"foobar": 0xbf9cf968,
	}
	for in, want := range cases {
		if got := fnv1a32([]byte(in)); got != want {
			t.Errorf("fnv1a32(%q) = %08x, want %08x", in, got, want)
		}
	}
}

func TestSolveProofRoundTrip(t *testing.T) {
	config := BuildConfig([]string{"1", "Chrome", "en-US", "US"})
	for difficulty := 1; difficulty <= 12; difficulty++ {
		token, ok := SolveProof("seed-abc", difficulty, config)
		if !ok {
			t.Fatalf("difficulty %d: no solution within cap", difficulty)
		}
		if !VerifyProof(token, "seed-abc", difficulty, config) {
			t.Errorf("difficulty %d: Verify rejected token %q", difficulty, token)
		}
	}
}

func TestSolveProofIsDeterministic(t *testing.T) {
	config := BuildConfig([]string{"x"})
	a, ok1 := SolveProof("s", 6, config)
	b, ok2 := SolveProof("s", 6, config)
	if !ok1 || !ok2 || a != b {
		t.Fatalf("nondeterministic solve: %q(%v) vs %q(%v)", a, ok1, b, ok2)
	}
}

func TestProofTokenShape(t *testing.T) {
	token, ok := SolveProof("seed", 4, BuildConfig([]string{"1"}))
	if !ok {
		t.Fatal("no solution")
	}
	if len(token) <= len(proofTokenPrefix) || token[:len(proofTokenPrefix)] != proofTokenPrefix {
		t.Fatalf("token %q lacks the %q envelope", token, proofTokenPrefix)
	}
	raw, err := base64.StdEncoding.DecodeString(token[len(proofTokenPrefix):])
	if err != nil {
		t.Fatalf("payload not base64: %v", err)
	}
	if len(raw) != 4 {
		t.Errorf("payload = %d bytes, want 4 (big-endian counter)", len(raw))
	}
}

func TestVerifyProofRejectsMalformed(t *testing.T) {
	config := BuildConfig([]string{"1"})
	for _, tok := range []string{
		"",
		"gAAAAAB",
		"nottheprefixAAAA",
		proofTokenPrefix + "!!!!",
		proofTokenPrefix + base64.StdEncoding.EncodeToString([]byte("too-long-payload")),
	} {
		if VerifyProof(tok, "seed", 4, config) {
			t.Errorf("VerifyProof accepted malformed token %q", tok)
		}
	}
	if VerifyProof(proofTokenPrefix+"AAAAAA==", "seed", 99, config) {
		t.Error("VerifyProof accepted out-of-range difficulty")
	}
}

func TestBuildConfig(t *testing.T) {
	if got := BuildConfig([]string{"1", "a", "b"}); got != "[1,a,b]" {
		t.Errorf("BuildConfig = %q, want %q", got, "[1,a,b]")
	}
	if got := BuildConfig(nil); got != "[]" {
		t.Errorf("BuildConfig(nil) = %q, want %q", got, "[]")
	}
}
