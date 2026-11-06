package sessions

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chimera/chimera/internal/spin"
)

// probeRecs is a small fixture transcript shaped like a real run: header,
// task, one round with a tool call whose result contains the probe phrase.
func probeRecs(id string) []spin.Record {
	return []spin.Record{
		{Kind: "session", Session: id, Model: "chimera-chatgpt"},
		{Kind: "user", Text: "Find who calls webapi.Register"},
		{Kind: "round", Round: 1, Text: "Searching the tree."},
		{Kind: "tool", Round: 1, Name: "grep", Arguments: `{"pattern":"webapi.Register"}`},
		{Kind: "result", Round: 1, Name: "grep", Result: "a.go:1: noise\nprovider.go:20: webapi.Register(gw)\nb.go:2: noise"},
		{Kind: "final", Round: 1, Text: "internal/providers/webapi/grok/provider.go:20"},
	}
}

// TestMatchRecordsTable covers the pure scan core field by field.
func TestMatchRecordsTable(t *testing.T) {
	recs := probeRecs("s1")
	tests := []struct {
		name      string
		query     string // already lower-cased, as Search passes it
		wantKinds []string
		wantLine  string // substring every hit line must contain, "" if none
	}{
		{"text field", "who calls", []string{"user"}, "who calls"},
		{"arguments field", `"pattern":"webapi`, []string{"tool"}, `{"pattern":"webapi.Register"}`},
		{"result narrows to the matching line", "register(gw)", []string{"result"}, "provider.go:20: webapi.Register(gw)"},
		{"final text", "grok/provider.go", []string{"final"}, "grok/provider.go:20"},
		{"case-insensitive across fields", "webapi.register", []string{"user", "tool", "result"}, "webapi.Register"},
		{"round text matches too", "searching the tree", []string{"round"}, "Searching the tree"},
		{"no match", "zzz-not-there", nil, ""},
		{"empty query matches nothing", "", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hits := matchRecords(recs, "s1", tt.query)
			if len(hits) != len(tt.wantKinds) {
				t.Fatalf("got %d hits (%+v), want %d", len(hits), hits, len(tt.wantKinds))
			}
			for i, h := range hits {
				if h.Kind != tt.wantKinds[i] {
					t.Errorf("hit %d kind = %q, want %q", i, h.Kind, tt.wantKinds[i])
				}
				if h.Session != "s1" {
					t.Errorf("hit %d session = %q, want s1", i, h.Session)
				}
				if tt.wantLine != "" && !strings.Contains(h.Line, tt.wantLine) {
					t.Errorf("hit %d line = %q, want it to contain %q", i, h.Line, tt.wantLine)
				}
			}
		})
	}
}

// TestSearchFindsPhraseAcrossSessions: two fixture sessions, an hour apart,
// both containing the phrase in a tool result; newest must come first.
func TestSearchFindsPhraseAcrossSessions(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	older := probeRecs("older-run")
	for i := range older {
		older[i].TS = base.Format(time.RFC3339Nano)
	}
	newer := probeRecs("newer-run")
	for i := range newer {
		newer[i].TS = base.Add(time.Hour).Format(time.RFC3339Nano)
	}
	writeTranscript(t, dir, "older-run", older)
	writeTranscript(t, dir, "newer-run", newer)

	res, err := Search(dir, "webapi.Register", "")
	if err != nil {
		t.Fatal(err)
	}
	// Both sessions match in three records: user Text, tool Arguments and
	// the tool Result all carry the phrase.
	if res.Total != 6 {
		t.Fatalf("total = %d, want 6", res.Total)
	}
	if len(res.Hits) != 6 {
		t.Fatalf("hits = %d, want 6", len(res.Hits))
	}
	if res.Hits[0].Session != "newer-run" {
		t.Errorf("first hit session = %q, want newest session first", res.Hits[0].Session)
	}
	var sawResultLine bool
	for _, h := range res.Hits {
		if h.Kind == "result" && strings.Contains(h.Line, "webapi.Register(gw)") {
			sawResultLine = true
		}
	}
	if !sawResultLine {
		t.Errorf("no result hit narrowed to the matching line: %+v", res.Hits)
	}
}

// TestSearchRespectsTagFilter: only sessions whose header carries the tag are
// scanned.
func TestSearchRespectsTagFilter(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

	tagged := probeRecs("tagged-run")
	tagged[0].Tags = []string{"research"}
	untagged := probeRecs("untagged-run")
	for i, recs := range [][]spin.Record{tagged, untagged} {
		for j := range recs {
			recs[j].TS = base.Add(time.Duration(i) * time.Hour).Format(time.RFC3339Nano)
		}
		writeTranscript(t, dir, []string{"tagged-run", "untagged-run"}[i], recs)
	}

	res, err := Search(dir, "webapi.Register", "research")
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 3 || len(res.Hits) != 3 {
		t.Fatalf("tagged search: total=%d hits=%d, want 3/3", res.Total, len(res.Hits))
	}
	for _, h := range res.Hits {
		if h.Session != "tagged-run" {
			t.Errorf("untagged session leaked past the filter: %+v", h)
		}
	}

	res, err = Search(dir, "webapi.Register", "nobody-tagged-this")
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 0 || len(res.Hits) != 0 {
		t.Errorf("unknown tag: total=%d hits=%d, want 0/0", res.Total, len(res.Hits))
	}
}

// TestSearchCapsHits: matches past the cap are counted, not returned.
func TestSearchCapsHits(t *testing.T) {
	dir := t.TempDir()
	recs := []spin.Record{{Kind: "session", Session: "big"}}
	for i := 0; i < 250; i++ {
		recs = append(recs, spin.Record{Kind: "note", Text: fmt.Sprintf("needle %d", i)})
	}
	writeTranscript(t, dir, "big", recs)

	res, err := Search(dir, "needle", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 250 {
		t.Errorf("total = %d, want 250 (uncapped count)", res.Total)
	}
	if len(res.Hits) != SearchHitCap {
		t.Errorf("hits = %d, want cap %d", len(res.Hits), SearchHitCap)
	}
	if !strings.Contains(res.Hits[len(res.Hits)-1].Line, "needle 199") {
		t.Errorf("last kept hit = %q, want needle 199", res.Hits[len(res.Hits)-1].Line)
	}
}

// TestSearchEmptyQuery: an empty (or whitespace) query is an error, not a
// match-everything scan.
func TestSearchEmptyQuery(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "one", probeRecs("one"))
	for _, q := range []string{"", "   "} {
		if _, err := Search(dir, q, ""); err == nil {
			t.Errorf("Search(%q) = nil error, want error", q)
		}
	}
}
