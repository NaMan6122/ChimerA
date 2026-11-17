package config

// A machine-wide settings file is the point of this package, which means the
// developer's own file would otherwise decide test outcomes. Point the resolver
// at an empty directory for the whole package; tests that care about the layout
// set CHIMERA_HOME themselves.

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "chimera-config-test")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("CHIMERA_HOME", dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
