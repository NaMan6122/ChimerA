package deepseekweb

import (
	"crypto/sha3"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// liveChallenge is an actual challenge fetched from
// POST /api/v0/chat/create_pow_challenge on a live account (2026-09-22). It is
// the strongest vector available: the server accepted nonce 20941 for it, so
// reproducing it proves both the padding (0x06) and the round count (23).
var liveChallenge = Challenge{
	Algorithm:  "DeepSeekHashV1",
	Challenge:  "8bb415dd09a5593fc1cc4b7a91a8f3039be373065c4260de1b0793e43abd4d3a",
	Salt:       "a7bee800cede26f66764",
	ExpireAt:   1790022592780, // milliseconds since epoch
	Difficulty: 144000,
	Signature:  "a186e27ba1b0ee65dd3cc22084205f6d339f86bf265357bd574569a23fd4883a",
	TargetPath: "/api/v0/chat/completion",
}

const liveNonce = 20941

func TestHashMatchesLiveChallenge(t *testing.T) {
	msg := fmt.Sprintf("%s_%d_%d", liveChallenge.Salt, liveChallenge.ExpireAt, liveNonce)
	got := DeepSeekHashV1([]byte(msg))
	if hex.EncodeToString(got[:]) != liveChallenge.Challenge {
		t.Fatalf("DeepSeekHashV1(%q) = %x\nwant                %s", msg, got, liveChallenge.Challenge)
	}
}

func TestSolveLiveChallenge(t *testing.T) {
	header, err := Solve(liveChallenge)
	if err != nil {
		t.Fatalf("Solve against a real server challenge failed: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		t.Fatalf("header is not base64: %v", err)
	}
	var a Answer
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatalf("header is not base64(JSON): %v", err)
	}
	if a.Answer != liveNonce {
		t.Errorf("answer nonce = %d, want %d", a.Answer, liveNonce)
	}
	if a.Salt != liveChallenge.Salt || a.Challenge != liveChallenge.Challenge ||
		a.Signature != liveChallenge.Signature || a.TargetPath != liveChallenge.TargetPath {
		t.Errorf("answer did not echo the challenge: %+v", a)
	}
	if !Verify(liveChallenge, header) {
		t.Error("Verify rejected a solution Solve just produced")
	}
}

// expire_at is milliseconds. Interpreting it as seconds must not satisfy the
// challenge — this is the unit bug that made the first live turn fail.
func TestExpireAtIsMilliseconds(t *testing.T) {
	wrong := liveChallenge
	wrong.ExpireAt = liveChallenge.ExpireAt / 1000
	if header, err := Solve(wrong); err == nil && Verify(wrong, header) {
		t.Fatal("a seconds-interpreted expire_at unexpectedly satisfied the challenge")
	}
}

// TestDeepSeekHashV1IsNonStandard documents WHY this package cannot use a
// standard-library hash: DeepSeekHashV1 pads SHA-3 style (0x06), so it is not
// legacy Keccak (0x01, what x/crypto provides), and it runs only 23 rounds, so it
// is not FIPS SHA3-256 (24 rounds, what crypto/sha3 provides). Either deviation
// alone changes the digest, which is why the permutation is vendored here.
func TestDeepSeekHashV1IsNonStandard(t *testing.T) {
	fips := sha3.Sum256(nil)
	// Prove the comparison is meaningful before relying on it.
	if hex.EncodeToString(fips[:]) != "a7ffc6f8bf1ed76651c14756a061d662f580ff4de43b49fa82d80a4b80f8434a" {
		t.Fatalf("unexpected stdlib sha3 output: %x", fips)
	}
	got := DeepSeekHashV1(nil)
	if got == fips {
		t.Fatal("DeepSeekHashV1(\"\") equals FIPS SHA3-256; the 23-round deviation is missing")
	}
}

func TestSolveDeterministic(t *testing.T) {
	c := Challenge{Challenge: strings.Repeat("00", 32), Salt: "s", ExpireAt: 1, Difficulty: 5}
	_, err1 := Solve(c) // unreachable target -> both calls must fail identically
	_, err2 := Solve(c)
	if (err1 == nil) != (err2 == nil) {
		t.Fatalf("nondeterministic outcome: %v vs %v", err1, err2)
	}
	if err1 == nil {
		t.Fatal("expected no solution for an all-zero target")
	}
}

func TestSolveRejectsBadInput(t *testing.T) {
	if _, err := Solve(Challenge{Challenge: "zz", Salt: "s", Difficulty: 10}); err == nil {
		t.Error("expected error for non-hex challenge")
	}
	if _, err := Solve(Challenge{Challenge: "ab", Salt: "s", Difficulty: 0}); err == nil {
		t.Error("expected error for non-positive difficulty")
	}
	// A salt that cannot fit one sponge block alongside a nonce must be rejected
	// rather than silently producing a wrong hash.
	long := Challenge{Challenge: strings.Repeat("ab", 32), Salt: strings.Repeat("x", 130), Difficulty: 10}
	if _, err := Solve(long); err == nil {
		t.Error("expected error for an over-long salt prefix")
	}
}

func TestVerifyRejectsTampering(t *testing.T) {
	header, err := Solve(liveChallenge)
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	if !Verify(liveChallenge, header) {
		t.Fatal("sanity: valid header rejected")
	}
	if Verify(liveChallenge, "not-base64!!") {
		t.Error("Verify accepted garbage")
	}
	// Flip the nonce: re-encode an answer whose nonce does not match the digest.
	bad, _ := encode(Answer{Salt: liveChallenge.Salt, Challenge: liveChallenge.Challenge, Answer: liveNonce + 1})
	if Verify(liveChallenge, bad) {
		t.Error("Verify accepted a wrong nonce")
	}
}
