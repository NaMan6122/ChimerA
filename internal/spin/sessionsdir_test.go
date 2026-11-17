package spin

// Where transcripts live when the install is relocated: one root, or the
// transcripts go missing from the place the rest of the state moved to.

import (
	"path/filepath"
	"testing"
)

func TestDefaultSessionsDirFollowsChimeraHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CHIMERA_HOME", home)
	t.Setenv("CHIMERA_SESSIONS_DIR", "")

	if got, want := DefaultSessionsDir(), filepath.Join(home, "sessions"); got != want {
		t.Fatalf("DefaultSessionsDir() = %q, want %q", got, want)
	}

	// The explicit override still wins over both.
	explicit := filepath.Join(t.TempDir(), "elsewhere")
	t.Setenv("CHIMERA_SESSIONS_DIR", explicit)
	if got := DefaultSessionsDir(); got != explicit {
		t.Fatalf("DefaultSessionsDir() = %q, want the explicit %q", got, explicit)
	}
}
