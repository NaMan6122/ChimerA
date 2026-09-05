package meter

import (
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestRecordAndMonthCount(t *testing.T) {
	s := openTest(t)
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		if err := s.Record(Record{
			TS: now, Tenant: "acme", Provider: "chatgpt",
			Model: "chimera-chatgpt", PromptChars: 100,
			CompletionChars: 50, LatencyMs: 5000, Code: 200,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Other tenant + last month must not count.
	if err := s.Record(Record{TS: now, Tenant: "other", Provider: "qwen", Code: 200}); err != nil {
		t.Fatal(err)
	}
	lastMonth, _ := MonthBounds(now.AddDate(0, -1, 0))
	if err := s.Record(Record{TS: lastMonth.Add(time.Hour), Tenant: "acme", Provider: "chatgpt", Code: 200}); err != nil {
		t.Fatal(err)
	}
	n, err := s.MonthCount("acme", now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("month count = %d, want 3", n)
	}
}

func TestSummarize(t *testing.T) {
	s := openTest(t)
	now := time.Now().UTC()
	records := []Record{
		{TS: now, Tenant: "acme", Provider: "chatgpt", PromptChars: 100, CompletionChars: 40, LatencyMs: 1000, Code: 200},
		{TS: now, Tenant: "acme", Provider: "chatgpt", PromptChars: 200, CompletionChars: 60, LatencyMs: 2000, Code: 200, Streaming: true},
		{TS: now, Tenant: "acme", Provider: "qwen", PromptChars: 50, CompletionChars: 0, LatencyMs: 3000, Code: 500},
	}
	for _, r := range records {
		if err := s.Record(r); err != nil {
			t.Fatal(err)
		}
	}
	start, end := MonthBounds(now)
	u, err := s.Summarize("acme", start, end)
	if err != nil {
		t.Fatal(err)
	}
	if u.Requests != 3 || u.PromptChars != 350 || u.CompletionChars != 100 || u.Errors != 1 {
		t.Fatalf("unexpected summary: %+v", u)
	}
	if u.ByProvider["chatgpt"] != 2 || u.ByProvider["qwen"] != 1 {
		t.Fatalf("unexpected by-provider: %+v", u.ByProvider)
	}
	empty, err := s.Summarize("nobody", start, end)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Requests != 0 || len(empty.ByProvider) != 0 {
		t.Fatalf("expected empty summary: %+v", empty)
	}
}

func TestOpenBadPath(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("empty path should error")
	}
}
