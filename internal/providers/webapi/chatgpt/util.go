package chatgptweb

import (
	"crypto/rand"
	"fmt"
)

func uuid4() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// DefaultPoWConfig renders the browser-fingerprint string mixed into the Sentinel
// proof of work.
//
// It is deliberately STABLE: the fingerprint describes the client, not the
// request, so it must not vary between the solve and any later verification (nor
// between turns of one session). A per-call random value would produce proofs
// that cannot be re-derived and that the server would reject.
//
// PROVISIONAL (spec 012 §3.1, §Risks item 2): the exact field set is
// undocumented and its absence may or may not be penalised. Values here are
// structurally plausible and pinned in one place so a capture can replace them
// without touching the solver.
func DefaultPoWConfig() string {
	return BuildConfig([]string{
		"1920x1080",
		"MacIntel",
		"en-US",
		"UTC",
		"1", "0", "1", "0", "1",
		"Google Inc.",
		"Apple M4",
		"Chrome/138.0.0.0",
		"1",
	})
}
