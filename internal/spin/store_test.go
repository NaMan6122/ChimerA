package spin

// Store tests: one transcript per conversation, round numbering that survives
// eviction and reopening, error turns recorded as notes, and the never-fail
// guarantee (nil store, unwritable dir).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readRecords(t *testing.T, dir, session string) []Record {
	t.Helper()
	path := filepath.Join(dir, sanitize(session)+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	var recs []Record
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("bad record line %q: %v", line, err)
		}
		recs = append(recs, r)
	}
	return recs
}

func TestStoreOneTranscriptPerConversation(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir, 0)
	defer st.Close()

	st.Record(Turn{Session: "chat-1", Model: "chimera-chatgpt", Tags: []string{"gateway", "chatgpt"},
		User: "hi", Answer: "mock reply to: hi", PromptChars: 2, Latency: 1500 * time.Millisecond})
	st.Record(Turn{Session: "chat-1", Model: "chimera-chatgpt", Tags: []string{"gateway", "chatgpt"},
		User: "more", Answer: "second answer", PromptChars: 40})

	recs := readRecords(t, dir, "chat-1")
	headers, users, rounds, finals := 0, 0, 0, 0
	for _, r := range recs {
		switch r.Kind {
		case "session":
			headers++
			if len(r.Tags) != 2 || r.Tags[0] != "gateway" || r.Tags[1] != "chatgpt" {
				t.Errorf("header tags = %v, want [gateway chatgpt]", r.Tags)
			}
			if r.Model != "chimera-chatgpt" {
				t.Errorf("header model = %q", r.Model)
			}
		case "user":
			users++
		case "round":
			rounds++
		case "final":
			finals++
		}
	}
	if headers != 1 {
		t.Errorf("session headers = %d, want 1 (one per transcript)", headers)
	}
	if users != 2 || rounds != 2 || finals != 2 {
		t.Errorf("users/rounds/finals = %d/%d/%d, want 2/2/2", users, rounds, finals)
	}
	var lastRound *Record
	for i := range recs {
		if recs[i].Kind == "round" {
			lastRound = &recs[i]
		}
	}
	if lastRound == nil || lastRound.Round != 2 {
		t.Errorf("second turn round number = %+v, want round 2", lastRound)
	}
}

func TestStoreSeparateConversations(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir, 0)
	defer st.Close()

	st.Record(Turn{Session: "a", User: "q1", Answer: "r1"})
	st.Record(Turn{Session: "b", User: "q2", Answer: "r2"})

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("files = %d, want 2 (one per conversation)", len(entries))
	}
}

func TestStoreRoundsContinueAfterEviction(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir, 1) // only one transcript open at a time
	defer st.Close()

	st.Record(Turn{Session: "a", User: "q1", Answer: "r1"})
	st.Record(Turn{Session: "b", User: "q2", Answer: "r2"}) // evicts a
	st.Record(Turn{Session: "a", User: "q3", Answer: "r3"}) // reopens a

	recs := readRecords(t, dir, "a")
	var roundNums []int
	for _, r := range recs {
		if r.Kind == "round" {
			roundNums = append(roundNums, r.Round)
		}
	}
	if len(roundNums) != 2 || roundNums[0] != 1 || roundNums[1] != 2 {
		t.Errorf("round numbers after eviction = %v, want [1 2]", roundNums)
	}
}

func TestStoreErrorTurn(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir, 0)
	defer st.Close()

	st.Record(Turn{Session: "e", User: "hi", Err: "turn failed (HTTP 500): provider error: boom"})

	recs := readRecords(t, dir, "e")
	sawUser, sawError, sawRound := false, false, false
	for _, r := range recs {
		switch r.Kind {
		case "user":
			sawUser = true
		case "error":
			sawError = true
		case "round", "final":
			sawRound = true
		}
	}
	if !sawUser || !sawError {
		t.Errorf("user=%v error=%v, want both recorded", sawUser, sawError)
	}
	if sawRound {
		t.Errorf("failed turn recorded a round/final; want error note only")
	}
}

func TestStoreNilAndUnwritableDirNeverFail(t *testing.T) {
	var nilStore *Store
	nilStore.Record(Turn{Session: "x", Answer: "y"}) // must not panic

	// A path whose parent is a file cannot become a directory: NewRecorder
	// fails, the store warns once, and the caller carries on.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := NewStore(filepath.Join(blocker, "sub"), 0)
	st.Record(Turn{Session: "x", Answer: "y"}) // must not panic
	st.Close()
}

func TestStoreClosedAfterCloseIsSafe(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir, 0)
	st.Record(Turn{Session: "a", Answer: "r1"})
	st.Close()
	st.Record(Turn{Session: "a", Answer: "r2"}) // reopens transparently
	recs := readRecords(t, dir, "a")
	if len(recs) == 0 {
		t.Fatal("no records after reopen")
	}
}
