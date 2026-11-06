package sessions

import (
	"strings"
	"testing"
	"time"

	"github.com/chimera/chimera/internal/spin"
)

func htmlFixture(id string) []spin.Record {
	ts := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	f := func(d int) string { return ts.Add(time.Duration(d) * time.Second).Format(time.RFC3339Nano) }
	return []spin.Record{
		{TS: f(0), Kind: "session", Session: id, Model: "chimera-grok", BaseURL: "http://localhost:8014/v1",
			Root: "/tmp/ws", Tags: []string{"research", "telemetry"}},
		{TS: f(1), Kind: "user", Text: "Find who calls webapi.Register"},
		{TS: f(2), Kind: "round", Round: 1, PromptChars: 900, LatencyS: 1.4, Text: "Searching the tree."},
		{TS: f(3), Kind: "tool", Round: 1, Name: "grep", Arguments: `{"pattern":"webapi.Register"}`},
		{TS: f(4), Kind: "result", Round: 1, Name: "grep", Result: "a.go:1: noise\nprovider.go:20: webapi.Register(gw)"},
		{TS: f(5), Kind: "note", Text: "answered without reading anything; asked for evidence"},
		{TS: f(6), Kind: "final", Round: 1, Text: "internal/providers/webapi/grok/provider.go:20"},
	}
}

// TestRenderHTMLRunbook: the full content model makes it into the page, tags
// included.
func TestRenderHTMLRunbook(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "rb", htmlFixture("rb"))

	page, err := RenderHTML(dir + "/rb.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<title>Session runbook — rb</title>",
		"Preserved for reading, not replay",
		"chimera-grok",             // metadata
		"http://localhost:8014/v1", // gateway as TEXT (not a link)
		"<span class=\"tag\">research</span>", "<span class=\"tag\">telemetry</span>",
		"<h2>Task</h2>", "Find who calls webapi.Register",
		"<h3>Round 1", "· 1.4s",
		"Searching the tree.",
		"Tool → <code>grep</code>", "{&#34;pattern&#34;:&#34;webapi.Register&#34;}",
		"Result ← <code>grep</code>", "provider.go:20: webapi.Register(gw)",
		"<blockquote>answered without reading anything; asked for evidence</blockquote>",
		"<h2>Final answer</h2>", "internal/providers/webapi/grok/provider.go:20",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("runbook missing %q", want)
		}
	}
}

// TestRenderHTMLSelfContained: no scripts, no links, no fetched assets — the
// style block in particular must not reference the network.
func TestRenderHTMLSelfContained(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "rb", htmlFixture("rb"))
	page, err := RenderHTML(dir + "/rb.jsonl")
	if err != nil {
		t.Fatal(err)
	}

	start := strings.Index(page, "<style>")
	end := strings.Index(page, "</style>")
	if start < 0 || end < 0 || end < start {
		t.Fatalf("page has no style block:\n%s", page)
	}
	style := page[start:end]
	if strings.Contains(style, "http") || strings.Contains(style, "url(") || strings.Contains(style, "@import") {
		t.Errorf("style block references external resources:\n%s", style)
	}
	for _, forbidden := range []string{"<script", "href=", "src=", "<link", "<iframe", "<img ", "@import", "url("} {
		if strings.Contains(page, forbidden) {
			t.Errorf("page contains external-reference construct %q", forbidden)
		}
	}
}

// TestRenderHTMLEscapesContent: transcript content is data, never markup.
func TestRenderHTMLEscapesContent(t *testing.T) {
	dir := t.TempDir()
	recs := htmlFixture("xss")
	recs[4].Result = "<script>alert(1)</script> and <b>bold</b> & text"
	writeTranscript(t, dir, "xss", recs)

	page, err := RenderHTML(dir + "/xss.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page, "<script>") {
		t.Errorf("raw script tag survived escaping")
	}
	if !strings.Contains(page, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("result was not escaped")
	}
	if !strings.Contains(page, "&amp; text") {
		t.Errorf("ampersand not escaped")
	}
}

// TestRenderHTMLUntagged: no tags in the header, no tags row.
func TestRenderHTMLUntagged(t *testing.T) {
	dir := t.TempDir()
	recs := htmlFixture("plain")
	recs[0].Tags = nil
	writeTranscript(t, dir, "plain", recs)

	page, err := RenderHTML(dir + "/plain.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page, "class=\"tag\"") {
		t.Errorf("untagged session rendered tag chips")
	}
}
