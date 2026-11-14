package webapi

import (
	"fmt"
	"testing"
)

func TestConvStoreBoundsAndEvictsOldest(t *testing.T) {
	var st ConvStore[string]
	for i := 0; i < 600; i++ {
		st.Set(fmt.Sprintf("s-%d", i), fmt.Sprintf("up-%d", i))
	}
	if got := st.Len(); got != DefaultMaxConversations {
		t.Fatalf("Len() = %d, want %d", got, DefaultMaxConversations)
	}
	// First 88 evicted, last 512 retained.
	if _, ok := st.Get("s-0"); ok {
		t.Fatal("expected oldest entry s-0 to be evicted")
	}
	if _, ok := st.Get("s-87"); ok {
		t.Fatal("expected entry s-87 to be evicted")
	}
	if v, ok := st.Get("s-88"); !ok || v != "up-88" {
		t.Fatalf("expected s-88 retained, got (%q, %v)", v, ok)
	}
	if v, ok := st.Get("s-599"); !ok || v != "up-599" {
		t.Fatalf("expected newest entry s-599 retained, got (%q, %v)", v, ok)
	}

	// Updating an existing key does not evict anything.
	st.Set("s-88", "up-88-v2")
	if v, ok := st.Get("s-88"); !ok || v != "up-88-v2" {
		t.Fatalf("updated s-88 = (%q, %v)", v, ok)
	}
	if st.Len() != DefaultMaxConversations {
		t.Fatalf("Len() after update = %d", st.Len())
	}

	st.Delete("s-88")
	if _, ok := st.Get("s-88"); ok {
		t.Fatal("expected s-88 deleted")
	}

	st.Clear()
	if st.Len() != 0 {
		t.Fatalf("Len() after Clear = %d, want 0", st.Len())
	}
}
