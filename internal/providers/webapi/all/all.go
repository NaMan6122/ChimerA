// Package all blank-imports every browserless transport so its init() runs and
// registers it with the webapi registry. cmd/chimera imports this once; adding a
// vendor therefore never requires editing main.go (spec 012, acceptance).
package all

import (
	_ "github.com/chimera/chimera/internal/providers/webapi/chatgpt"
	_ "github.com/chimera/chimera/internal/providers/webapi/deepseek"
	_ "github.com/chimera/chimera/internal/providers/webapi/qwen"
)
