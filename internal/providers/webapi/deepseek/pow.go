// Package deepseekweb replays chat.deepseek.com's web API over HTTP — no browser
// (specs/012-webapi-chatgpt-deepseek.md). This file implements the DeepSeekHashV1
// proof of work that gates POST /api/v0/chat/completion.
package deepseekweb

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Challenge is the proof-of-work challenge returned by
// POST /api/v0/chat/create_pow_challenge, at data.biz_data.challenge.
type Challenge struct {
	Algorithm  string `json:"algorithm"`   // "DeepSeekHashV1"
	Challenge  string `json:"challenge"`   // hex of the expected digest
	Salt       string `json:"salt"`        //
	ExpireAt   int64  `json:"expire_at"`   // MILLISECONDS since epoch (13 digits)
	Difficulty int64  `json:"difficulty"`  // exclusive upper bound on the nonce (~144000)
	Signature  string `json:"signature"`   //
	TargetPath string `json:"target_path"` // e.g. "/api/v0/chat/completion"
}

// Answer is the solved proof of work, base64(JSON)-encoded into the
// x-ds-pow-response request header. The field set and order were confirmed
// against a live capture, whose accepted header decoded to exactly these keys.
type Answer struct {
	Algorithm  string `json:"algorithm"`
	Challenge  string `json:"challenge"`
	Salt       string `json:"salt"`
	Answer     int64  `json:"answer"`
	Signature  string `json:"signature"`
	TargetPath string `json:"target_path"`
}

// maxNonceDigits bounds the decimal nonce, matching the reference solver. The
// salt prefix plus this must still fit one sponge block.
const maxNonceDigits = 20

// Solve searches for a nonce such that
//
//	DeepSeekHashV1("{salt}_{expire_at}_{nonce}") == challenge
//
// and returns the header-ready x-ds-pow-response value. The search is bounded by
// Difficulty; a well-formed challenge always has a solution inside that window.
//
// expire_at is used exactly as the server sent it — it is milliseconds, and no
// unit conversion happens here.
func Solve(c Challenge) (string, error) {
	want, err := hex.DecodeString(strings.TrimSpace(c.Challenge))
	if err != nil {
		return "", fmt.Errorf("deepseek pow: challenge is not hex: %w", err)
	}
	if c.Difficulty <= 0 {
		return "", fmt.Errorf("deepseek pow: non-positive difficulty %d", c.Difficulty)
	}

	// prefix is constant across the search; only the decimal nonce varies.
	prefix := c.Salt + "_" + strconv.FormatInt(c.ExpireAt, 10) + "_"
	if len(prefix)+maxNonceDigits > dsRate {
		return "", fmt.Errorf("deepseek pow: salt prefix too long (%d bytes)", len(prefix))
	}

	buf := append(make([]byte, 0, dsRate), prefix...)
	base := len(buf)
	nonceBytes := make([]byte, 0, maxNonceDigits)

	for nonce := int64(0); nonce < c.Difficulty; nonce++ {
		nonceBytes = strconv.AppendInt(nonceBytes[:0], nonce, 10)
		buf = append(buf[:base], nonceBytes...)
		sum := DeepSeekHashV1(buf)
		if bytes.Equal(sum[:], want) {
			return encode(Answer{
				Algorithm:  c.Algorithm,
				Challenge:  c.Challenge,
				Salt:       c.Salt,
				Answer:     nonce,
				Signature:  c.Signature,
				TargetPath: c.TargetPath,
			})
		}
	}
	return "", fmt.Errorf("deepseek pow: no nonce in [0,%d) satisfied the challenge", c.Difficulty)
}

// encode renders an Answer as the base64(JSON) header value.
func encode(a Answer) (string, error) {
	raw, err := json.Marshal(a)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// Verify reports whether a header value solves the challenge. Exported for tests
// and for a startup self-check against a captured session.
func Verify(c Challenge, header string) bool {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header))
	if err != nil {
		return false
	}
	var a Answer
	if err := json.Unmarshal(raw, &a); err != nil {
		return false
	}
	if a.Salt != c.Salt || a.Challenge != c.Challenge {
		return false
	}
	sum := fmt.Sprintf("%s_%d_%d", c.Salt, c.ExpireAt, a.Answer)
	got := DeepSeekHashV1([]byte(sum))
	return hex.EncodeToString(got[:]) == strings.TrimSpace(c.Challenge)
}
