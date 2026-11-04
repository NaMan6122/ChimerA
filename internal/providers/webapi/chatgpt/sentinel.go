// Sentinel proof-of-work for chatgpt.com.
//
// This file REPLACES the FNV-1a solver that was here before. That solver did not
// match the protocol: it searched a 4-byte counter under FNV-1a, whereas the
// server implements SHA3-512 over a JSON fingerprint. A wrong hash is
// indistinguishable from an unsolvable challenge — the search simply exhausts —
// so the failure looked like a server block rather than a client bug. See
// spec 012 §3.2.
//
// Two tokens are produced:
//
//	requirements token  "gAAAAAC" + answer  sent as {"p": <token>} to
//	                                     /sentinel/chat-requirements
//	proof token         "gAAAAAB" + answer  sent as openai-sentinel-proof-token
//
// The search is: find i such that the first len(diff)/2 bytes of
//
//	SHA3-512( seed || base64( compactJSON(config) ) )
//
// compare strictly less than fromhex(diff). SHA3-512 is FIPS-202 (crypto/sha3),
// NOT legacy Keccak — the same distinction that bit the DeepSeek solver.
package chatgptweb

import (
	"bytes"
	"crypto/rand"
	"crypto/sha3"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// powMaxIterations mirrors the reference loop bound. Difficulty is a hex string
// whose length is the number of leading bytes to compare, so a typical
// "0634fd" means 3 bytes — expected ~16M hashes, well under a second in Go.
const powMaxIterations = 500_000

// requirementsDifficulty is the FIXED difficulty for the requirements token. It
// is not read from the server: the token is generated locally and sent as `p`.
const requirementsDifficulty = "0fffff"

// userAgent is the browser profile the fingerprint describes. It must stay
// consistent with the cookie jar's origin or the proof is rejected.
const userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"

func pick[T any](xs []T) T { return xs[mrand(len(xs))] }

func mrand(n int) int {
	if n <= 0 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

func mrandFloat() float64 {
	v, err := rand.Int(rand.Reader, big.NewInt(1<<53))
	if err != nil {
		return 0
	}
	return float64(v.Int64()) / float64(int64(1)<<53)
}

func uuid4() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// parseTime renders "Mon Jan 02 2006 15:04:05 GMT-0500 (Eastern Standard Time)"
// for (now UTC - 5h). The reference pins a fixed -0500 offset rather than the
// client's local zone.
func parseTime() string {
	t := time.Now().UTC().Add(-5 * time.Hour)
	return t.Format("Mon Jan 02 2006 15:04:05") + " GMT-0500 (Eastern Standard Time)"
}

// buildConfig builds the 18-element fingerprint array. Indices 3 and 9 are the
// counter slots, which generateAnswer overwrites with two dynamic values.
func buildConfig() []any {
	return []any{
		pick([]int{1920 + 1080, 2560 + 1440, 1920 + 1200, 2560 + 1600}),
		parseTime(),
		4294705152,
		0,
		userAgent,
		"", // script_src
		"", // dpl
		"en-US",
		"en-US,es-US,en,es",
		0,
		pick(navigatorKey),
		pick(documentKey),
		pick(windowKey),
		float64(time.Now().UnixNano()) / 1e6, // ~ performance.now()
		uuid4(),
		"",
		pick(cores),
		float64(time.Now().UnixNano()) / 1e6,
	}
}

// formatFloat renders a float the way Python's repr and JS's JSON.stringify do:
// shortest round-tripping decimal, always with a decimal point for whole values
// ("999.0", not "999"). Go's encoding/json drops the ".0", which changes the
// hashed bytes.
func formatFloat(f float64) string {
	if f == math.Trunc(f) && !math.IsInf(f, 0) && math.Abs(f) < 1e21 {
		return strconv.FormatFloat(f, 'f', 1, 64)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// marshalScalar renders one config element. The config holds only ints, floats
// and strings, so a purpose-built encoder is used instead of encoding/json —
// json.Marshal cannot express Python's ".0" for whole floats.
func marshalScalar(v any) (string, error) {
	switch x := v.(type) {
	case int:
		return strconv.Itoa(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case float64:
		return formatFloat(x), nil
	case string:
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(x); err != nil {
			return "", err
		}
		return strings.TrimRight(buf.String(), "\n"), nil
	default:
		return "", fmt.Errorf("unsupported config element type %T", v)
	}
}

// compactConfig marshals a config slice with no spaces, matching Python's
// json.dumps(..., separators=(",", ":")) and JS JSON.stringify.
func compactConfig(v []any) (string, error) {
	parts := make([]string, 0, len(v))
	for _, e := range v {
		s, err := marshalScalar(e)
		if err != nil {
			return "", err
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ","), nil
}

// pyBytesLE reports a <= b under Python/JS bytes-comparison semantics: compare
// element-wise, and if one is a prefix of the other the shorter sorts first.
// Getting this wrong makes a valid proof look unsolved.
func pyBytesLE(a, b []byte) bool {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) <= len(b)
}

// searchWithParts is the counter search, isolated from config marshalling so it
// can be pinned to externally computed vectors byte-for-byte. final[i] is
//
//	part1 + itoa(i) + part2 + itoa(i>>1) + part3
//
// which is the reference's static_config_part{1,2,3} with the two counter slots
// (indices 3 and 9) filled in.
func searchWithParts(seed, diff, part1, part2, part3 string) (string, bool, error) {
	diffLen := len(diff)
	target, err := hex.DecodeString(diff)
	if err != nil {
		return "", false, fmt.Errorf("difficulty %q is not hex: %w", diff, err)
	}
	seedB := []byte(seed)
	for i := 0; i < powMaxIterations; i++ {
		final := part1 + strconv.Itoa(i) + part2 + strconv.Itoa(i>>1) + part3
		b64 := base64.StdEncoding.EncodeToString([]byte(final))
		h := sha3.Sum512(append(append([]byte{}, seedB...), []byte(b64)...))
		if pyBytesLE(h[:diffLen], target) {
			return b64, true, nil
		}
	}
	// Fallback matches the reference: a fixed sentinel prefix + base64 of `"seed"`.
	return "wQ8Lk5FbGpA2NcR9dShT6gYjU7VxZ4D" + base64.StdEncoding.EncodeToString([]byte(`"`+seed+`"`)), false, nil
}

// partsForConfig renders the three static JSON fragments around the counters.
// The counter slots are indices 3 and 9, so the fragments are [0:3), [4:9) and
// [10:], joined by the two dynamic values.
func partsForConfig(config []any) (string, string, string, error) {
	p1, err := compactConfig(config[0:3])
	if err != nil {
		return "", "", "", err
	}
	p2, err := compactConfig(config[4:9])
	if err != nil {
		return "", "", "", err
	}
	p3, err := compactConfig(config[10:])
	if err != nil {
		return "", "", "", err
	}
	return "[" + p1 + ",", "," + p2 + ",", "," + p3 + "]", nil
}

// generateAnswer searches for the counter satisfying the difficulty target.
func generateAnswer(seed, diff string, config []any) (string, bool, error) {
	part1, part2, part3, err := partsForConfig(config)
	if err != nil {
		return "", false, err
	}
	return searchWithParts(seed, diff, part1, part2, part3)
}

// GetRequirementsToken builds the token sent as {"p": ...} to
// /sentinel/chat-requirements. It is generated LOCALLY at a fixed difficulty; the
// server does not supply it. It is also the XOR key for the turnstile blob.
func GetRequirementsToken() (string, error) {
	cfg := buildConfig()
	ans, _, err := generateAnswer(strconv.FormatFloat(mrandFloat(), 'g', -1, 64), requirementsDifficulty, cfg)
	if err != nil {
		return "", err
	}
	return "gAAAAAC" + ans, nil
}

// SolveProof builds the openai-sentinel-proof-token for a server challenge.
func SolveProof(seed, difficulty string) (string, bool, error) {
	cfg := buildConfig()
	ans, ok, err := generateAnswer(seed, difficulty, cfg)
	if err != nil {
		return "", false, err
	}
	return "gAAAAAB" + ans, ok, nil
}

// decodeB64Payload decodes a base64 payload, tolerating the unpadded form the
// proof tokens use.
func decodeB64Payload(s string) ([]byte, error) {
	if raw, err := base64.StdEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}

// VerifyProof reports whether a proof token satisfies the challenge. Exported for
// tests and for a startup self-check against a captured session.
//
// The token's payload is base64 of the compact JSON config, and the hash is taken
// over the base64 TEXT (not the decoded bytes) — the same construction the solver
// searches over. Hashing the decoded JSON instead would reject every valid proof.
func VerifyProof(token, seed, difficulty string) bool {
	if difficulty == "" || len(difficulty)%2 != 0 {
		return false
	}
	if len(token) <= 7 || token[:7] != "gAAAAAB" {
		return false
	}
	// The payload must be valid base64, but the hash is over its text form.
	if _, err := decodeB64Payload(token[7:]); err != nil {
		return false
	}
	h := sha3.Sum512(append([]byte(seed), token[7:]...))
	target, err := hex.DecodeString(difficulty)
	if err != nil {
		return false
	}
	return pyBytesLE(h[:len(target)], target)
}
