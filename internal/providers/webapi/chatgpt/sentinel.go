// Package chatgptweb replays chatgpt.com's backend-api over HTTP — no browser
// (specs/012-webapi-chatgpt-deepseek.md). This file implements the Sentinel proof
// of work that accompanies /backend-api/conversation.
package chatgptweb

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/fnv"
)

// PoWMaxIterations bounds the counter search. difficulty is a bit-count (8-24 in
// practice), so the expected number of iterations is 2^(difficulty-1) — this cap
// is only a guard against a malformed challenge.
const PoWMaxIterations = 500_000

// proofTokenPrefix is the base64 envelope marker every proof token carries. It is
// the encoding of a small fixed header, not part of the solved value.
const proofTokenPrefix = "gAAAAAB"

// fnv1a32 is FNV-1a, 32-bit. stdlib hash/fnv implements exactly this, so there is
// no hand-rolled hash to get wrong.
func fnv1a32(chunks ...[]byte) uint32 {
	h := fnv.New32a()
	for _, c := range chunks {
		_, _ = h.Write(c)
	}
	return h.Sum32()
}

// SolveProof searches for the counter i that satisfies the Sentinel challenge:
//
//	FNV-1a(seed ‖ byte(difficulty) ‖ be32(i) ‖ config) < 1<<(32-difficulty)
//
// and returns the header-ready openai-sentinel-proof-token value. config is a
// client-constructed browser-fingerprint string (see BuildConfig), not read from
// a live browser. Returns ok=false if the search cap is exhausted.
func SolveProof(seed string, difficulty int, config string) (string, bool) {
	if difficulty < 0 || difficulty > 32 {
		return "", false
	}
	threshold := uint64(1) << uint(32-difficulty) // uint64 so difficulty=0 cannot overflow
	seedB := []byte(seed)
	diffB := []byte{byte(difficulty)}
	configB := []byte(config)
	var ctr [4]byte

	for i := 0; i < PoWMaxIterations; i++ {
		binary.BigEndian.PutUint32(ctr[:], uint32(i))
		sum := fnv1a32(seedB, diffB, ctr[:], configB)
		if uint64(sum) < threshold {
			return proofTokenPrefix + base64.StdEncoding.EncodeToString(ctr[:]), true
		}
	}
	return "", false
}

// VerifyProof reports whether a proof token satisfies the challenge. Exported for
// tests and for a startup self-check against a captured session. It re-derives
// the value for the counter embedded in the token, so it does not need the
// original iteration count.
func VerifyProof(token, seed string, difficulty int, config string) bool {
	if difficulty < 0 || difficulty > 32 {
		return false
	}
	if len(token) <= len(proofTokenPrefix) || token[:len(proofTokenPrefix)] != proofTokenPrefix {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(token[len(proofTokenPrefix):])
	if err != nil || len(raw) != 4 {
		return false
	}
	sum := fnv1a32([]byte(seed), []byte{byte(difficulty)}, raw, []byte(config))
	return uint64(sum) < uint64(1)<<uint(32-difficulty)
}

// BuildConfig renders the browser-fingerprint string mixed into the PoW. The
// field order matters to the server; the shape follows the widely-used reference
// (gpt4free sentinel.py). Left as a function so it is pinned in exactly one place.
func BuildConfig(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return fmt.Sprintf("[%s]", out)
}
