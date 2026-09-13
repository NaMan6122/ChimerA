// Package qwenweb talks to chat.qwen.ai's web API over HTTP — no browser.
//
// It is the first Transport implementation behind specs/011-provider-transports.md.
// Anti-bot material (ssxmod cookies, bx-ua) is generated client-side, so only the
// session custody (access token + WAF cookies) is imported via a storage-state
// file produced by scripts/qwenweb-spike/cdp-export.mjs.
package qwenweb

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	mrand "math/rand/v2"
	"strings"
	"time"
)

func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

func uuid4() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// generateFingerprint ports qwen-reverse fingerprint.py (MIT).
func generateFingerprint() string {
	now := time.Now().UnixMilli()
	h := func() uint32 { return mrand.Uint32() }
	fields := []string{
		randHex(20), "websdk-2.3.15d", "1765348410850", "91", "1|15",
		"en-US", "480", "16705151|12791",
		"1470|956|283|797|158|0|1470|956|1470|798|0|0", "5", "MacIntel", "10",
		"ANGLE (Apple, ANGLE Metal Renderer: Apple M4, Unspecified Version)|Google Inc. (Apple)",
		"30|30", "0", "28",
		fmt.Sprintf("5|%d", h()),
		fmt.Sprint(h()), fmt.Sprint(h()),
		"1", "0", "1", "0", "P", "0", "0", "0", "416",
		"Google Inc.", "8", "-1|0|0|0|0", fmt.Sprint(h()),
		"11", fmt.Sprint(now), fmt.Sprint(h()), "0", fmt.Sprint(10 + mrand.IntN(91)),
	}
	return strings.Join(fields, "^")
}
