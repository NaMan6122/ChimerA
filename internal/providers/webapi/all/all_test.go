package all

// Registration must be tested, not assumed: a transport whose init() never
// runs fails silently in auto mode (it falls through to launching Chromium),
// which shows up only as ~10x latency — the qwen lesson (see the Mind).

import (
	"slices"
	"testing"

	"github.com/chimera/chimera/internal/providers/webapi"
)

func TestGLMRegistered(t *testing.T) {
	if _, ok := webapi.Lookup("glm"); !ok {
		t.Errorf("glm has no registered webapi transport; registered = %v", webapi.Registered())
	}
	if !slices.Contains(webapi.Registered(), "glm") {
		t.Errorf("Registered() missing glm: %v", webapi.Registered())
	}
}
