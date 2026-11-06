package sessions

// Transcript search: a linear scan over every .jsonl in the directory,
// matching case-insensitively against Text, Arguments and Result. Deliberately
// no index — transcripts are small, the runbook reader runs rarely, and
// correctness beats cleverness. A re-scan is always as good as a stale index.

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/chimera/chimera/internal/spin"
)

// SearchHitCap bounds how many hits Search returns. The total match count is
// reported regardless, so a busy transcript set is discoverable without
// flooding the terminal.
const SearchHitCap = 200

// Hit is one matching record inside one transcript.
type Hit struct {
	Session string // session id
	Round   int    // 0 for records outside a round (user, note, …)
	Kind    string // record kind: user | round | tool | result | final | note | error
	Line    string // the trimmed line within the field that matched
}

// SearchResult is the outcome of one Search: the capped hits plus the
// uncapped total.
type SearchResult struct {
	Hits  []Hit
	Total int // all matches, including any past SearchHitCap
}

// Search scans every transcript in dir, newest session first, and returns
// every record whose Text, Arguments or Result contains query
// (case-insensitive). tag, when non-empty, restricts the scan to sessions
// whose header carries it (see HasTag). Transcripts that fail to load are
// skipped, not fatal — the index view does the same.
func Search(dir, query, tag string) (*SearchResult, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil, fmt.Errorf("search: empty query")
	}
	paths, err := spin.ListSessions(dir)
	if err != nil {
		return nil, err
	}

	// One load per transcript. Ordering needs each session's start time, and
	// the tag filter and hit ids need its header, both of which only the
	// records know — so no separate LoadSummary pass.
	type transcript struct {
		recs    []spin.Record
		started time.Time
		id      string
	}
	loaded := make([]transcript, 0, len(paths))
	for _, p := range paths {
		recs, err := load(p)
		if err != nil {
			continue // unreadable transcript: skip, don't break the search
		}
		t := transcript{recs: recs}
		for _, r := range recs {
			if ts, err := time.Parse(time.RFC3339Nano, r.TS); err == nil {
				if t.started.IsZero() || ts.Before(t.started) {
					t.started = ts
				}
			}
			if r.Kind == "session" && t.id == "" {
				t.id = r.Session
			}
		}
		if t.id == "" {
			t.id = strings.TrimSuffix(filepath.Base(p), ".jsonl")
		}
		loaded = append(loaded, t)
	}
	sort.Slice(loaded, func(i, j int) bool { return loaded[i].started.After(loaded[j].started) })

	res := &SearchResult{}
	for _, t := range loaded {
		if tag != "" && !HasTag(headerTags(t.recs), tag) {
			continue
		}
		for _, h := range matchRecords(t.recs, t.id, q) {
			res.Total++
			if len(res.Hits) < SearchHitCap {
				res.Hits = append(res.Hits, h)
			}
		}
	}
	return res, nil
}

// matchRecords scans one transcript's records in file order. sessionID is
// stamped onto hits because transcripts may leave per-record Session empty
// (the header carries it); the id resolved for the whole file is authoritative.
func matchRecords(recs []spin.Record, sessionID, queryLower string) []Hit {
	var hits []Hit
	for _, r := range recs {
		line, ok := matchRecord(r, queryLower)
		if !ok {
			continue
		}
		hits = append(hits, Hit{Session: sessionID, Round: r.Round, Kind: r.Kind, Line: line})
	}
	return hits
}

// matchRecord reports whether one record matches the lower-cased query in
// Text, Arguments or Result, along with the trimmed line that matched.
func matchRecord(r spin.Record, q string) (string, bool) {
	if q == "" {
		return "", false
	}
	for _, field := range [...]string{r.Text, r.Arguments, r.Result} {
		if field == "" || !strings.Contains(strings.ToLower(field), q) {
			continue
		}
		return matchingLine(field, q), true
	}
	return "", false
}

// matchingLine narrows a multi-line field to the one line containing the
// match, so a 2 KB tool result surfaces as one readable line.
func matchingLine(field, q string) string {
	for _, line := range strings.Split(field, "\n") {
		if strings.Contains(strings.ToLower(line), q) {
			return strings.TrimSpace(line)
		}
	}
	return strings.TrimSpace(field)
}

// headerTags returns the tags of a transcript's session header record, if any.
func headerTags(recs []spin.Record) []string {
	for _, r := range recs {
		if r.Kind == "session" {
			return r.Tags
		}
	}
	return nil
}
