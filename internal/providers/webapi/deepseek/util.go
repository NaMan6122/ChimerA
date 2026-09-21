package deepseekweb

import (
	"crypto/rand"
	"fmt"
)

// uuid4 returns a random v4 UUID. Used for x-device-id, which the web UI
// persists as localStorage "deepseek-device-id:chat". A per-client value is
// sufficient: the header identifies a client instance, not the account, and the
// capture shows the server accepts it without matching a prior value.
func uuid4() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
