package chatgptweb

import (
	"crypto/sha3"
	"encoding/hex"
	"strings"
	"testing"
)

// TestPoWMatchesPythonReference pins the search to vectors produced by the
// chat2api Python reference. The parts are built from the SAME fixed config so Go
// and Python hash identical bytes — the only way to catch a wrong hash, which
// otherwise looks exactly like an unsolvable challenge.
func TestPoWMatchesPythonReference(t *testing.T) {
	cfg := []any{
		3008,
		"Mon Jan 02 2026 10:00:00 GMT-0500 (Eastern Standard Time)",
		4294705152,
		0,
		"Mozilla/5.0 test",
		"", "", "en-US", "en-US,es-US,en,es", 0,
		"webdriver−false",
		"location",
		"window",
		123.456,
		"00000000-0000-4000-8000-000000000000",
		"",
		8,
		999.0,
	}
	part1, part2, part3, err := partsForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range powVectors {
		got, solved, err := searchWithParts(v.seed, v.diff, part1, part2, part3)
		if err != nil {
			t.Fatalf("seed=%q diff=%q: %v", v.seed, v.diff, err)
		}
		if solved != v.solved {
			t.Errorf("seed=%q diff=%q: solved=%v want %v", v.seed, v.diff, solved, v.solved)
		}
		if got != v.want {
			t.Errorf("seed=%q diff=%q:\n got %q\nwant %q", v.seed, v.diff, got, v.want)
		}
	}
}

// TestConfigPartsShape guards the index split (0:3 / 4:9 / 10:) — an off-by-one
// here silently changes every hash.
func TestConfigPartsShape(t *testing.T) {
	cfg := []any{1, "b", 3, 0, "e", "f", "g", "h", "i", 0, "k", "l"}
	p1, p2, p3, err := partsForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if p1 != `[1,"b",3,` {
		t.Errorf("part1 = %q", p1)
	}
	if p2 != `,"e","f","g","h","i",` {
		t.Errorf("part2 = %q", p2)
	}
	if p3 != `,"k","l"]` {
		t.Errorf("part3 = %q", p3)
	}
}

// TestSHA3IsFIPS202 pins the primitive to the standard (FIPS-202) SHA3-512.
// DeepSeekHashV1 needed a non-standard 23-round Keccak; Sentinel uses the
// standard one, and getting this backwards yields "no solution" rather than an
// error, so it must be asserted directly.
func TestSHA3IsFIPS202(t *testing.T) {
	const want = "a69f73cca23a9ac5c8b567dc185a756e97c982164fe25859e0d1dcc1475c80a615b2123af1f5f94c11e3e9402c3ac558f500199d95b6d3e301758586281dcd26"
	sum := sha3.Sum512(nil)
	if got := hex.EncodeToString(sum[:]); got != want {
		t.Errorf("sha3-512(\"\") = %s\n              want %s", got, want)
	}
}

// TestRequirementsTokenShape checks the fixed prefix and that the token is
// generated locally (not read from the server).
func TestRequirementsTokenShape(t *testing.T) {
	tok, err := GetRequirementsToken()
	if err != nil {
		t.Fatal(err)
	}
	const prefix = "gAAAAAC" // 7 chars: "gAAAAA" + "C" (vs "B" for the proof token)
	if !strings.HasPrefix(tok, prefix) {
		t.Fatalf("requirements token %q lacks the %q envelope", tok, prefix)
	}
	if _, err := decodeB64Payload(tok[len(prefix):]); err != nil {
		t.Errorf("requirements token payload is not base64: %v", err)
	}
}

// TestSolveProofRoundTrip solves a challenge and verifies the result against it.
func TestSolveProofRoundTrip(t *testing.T) {
	for _, diff := range []string{"0f", "00ff", "0000ff"} {
		tok, ok, err := SolveProof("seed-abc", diff)
		if err != nil {
			t.Fatalf("difficulty %s: %v", diff, err)
		}
		if !ok {
			t.Fatalf("difficulty %s: no solution within cap", diff)
		}
		if !VerifyProof(tok, "seed-abc", diff) {
			t.Errorf("difficulty %s: VerifyProof rejected %q", diff, tok)
		}
	}
}

// TestVerifyProofRejectsMalformed guards the envelope check. A difficulty that
// is not valid hex must be rejected rather than silently passing.
func TestVerifyProofRejectsMalformed(t *testing.T) {
	for _, tok := range []string{"", "gAAAAAB", "nottheprefixAAAA", "gAAAAAB!!!!"} {
		if VerifyProof(tok, "seed", "0f") {
			t.Errorf("VerifyProof accepted malformed token %q", tok)
		}
	}
	for _, diff := range []string{"", "9", "zz", "0"} {
		if VerifyProof("gAAAAABAAAAAA==", "seed", diff) {
			t.Errorf("VerifyProof accepted difficulty %q", diff)
		}
	}
}

// TestConfigLength pins the array size. The reference config is 18 elements; a
// truncated array produces a proof the server rejects with no diagnostic.
func TestConfigLength(t *testing.T) {
	if got := len(buildConfig()); got != configLen {
		t.Errorf("buildConfig() has %d elements, want %d", got, configLen)
	}
	if configLen != 18 {
		t.Errorf("configLen = %d, want 18", configLen)
	}
}

// TestKeyArraySizes pins the fingerprint arrays, which are generated from the
// reference and must not drift.
func TestKeyArraySizes(t *testing.T) {
	if len(navigatorKey) != 93 {
		t.Errorf("navigatorKey has %d entries, want 93", len(navigatorKey))
	}
	if len(documentKey) != 2 {
		t.Errorf("documentKey has %d entries, want 2", len(documentKey))
	}
	if len(windowKey) != 252 {
		t.Errorf("windowKey has %d entries, want 252", len(windowKey))
	}
}
