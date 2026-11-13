package spin

// Store records transcripts for many concurrently active sessions: the
// gateway's shape, where consecutive HTTP turns may belong to different
// conversations. It wraps one Recorder per active session, keeps at most a
// handful of transcripts open at once (each open transcript holds a file
// handle), and reopens a session transparently when a later turn arrives.
//
// Round numbering survives eviction and gateway restarts: a newly opened
// transcript counts the rounds already on disk and continues from there, so
// one conversation is one continuously numbered runbook no matter how the
// process churns underneath.
//
// Like Recorder, a Store never blocks a turn on anything slower than an
// append and never fails a request: write problems disable recording with
// one warning.

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// defaultMaxOpen bounds how many transcripts are held open at once. A
// long-lived gateway meets many session ids; file handles must not grow with
// them.
const defaultMaxOpen = 16

// Store appends turns to per-session transcripts under one directory.
type Store struct {
	dir string
	max int

	mu      sync.Mutex
	open    map[string]*storeEntry
	lru     []string // session ids, least-recently-used first
	warned  bool
	warnErr error
}

// storeEntry is one active session: its open recorder and the number of
// rounds already in the transcript (counted once, at open).
type storeEntry struct {
	rec    *Recorder
	rounds int
}

// Turn is one completed gateway turn to record.
type Turn struct {
	Session     string        // transcript id: client session id or a derived one
	Model       string        // resolved model id, written into the header record once
	Tags        []string      // header tags, written once (see Recorder.Start)
	User        string        // the user message this turn answered ("" on tool-only follow-ups)
	Answer      string        // the assistant reply ("" when the turn failed)
	Err         string        // failure text; non-empty records an error note instead of an answer
	PromptChars int           // size of the prompt as sent upstream
	Latency     time.Duration // turn latency, lock wait included
}

// NewStore prepares a store rooted at dir, holding at most maxOpen
// transcripts open (<=0 selects the default).
func NewStore(dir string, maxOpen int) *Store {
	if maxOpen <= 0 {
		maxOpen = defaultMaxOpen
	}
	return &Store{dir: dir, max: maxOpen, open: map[string]*storeEntry{}}
}

// Record appends one turn. The first turn for a session writes the header
// record (model, tags); every turn writes the user message, then either the
// round + final records (an answer) or an error note (a failure). Safe for
// concurrent use; a nil Store is a no-op, so callers need no nil checks.
func (st *Store) Record(t Turn) {
	if st == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()

	e := st.entryLocked(t.Session, t.Model, t.Tags)
	if e == nil {
		return
	}
	if t.User != "" {
		e.rec.Note("user", t.User)
	}
	if t.Err != "" {
		e.rec.Note("error", t.Err)
		return
	}
	if t.Answer == "" {
		return
	}
	e.rounds++
	e.rec.Observe(Event{Kind: EventRound, Round: e.rounds, Text: t.Answer,
		PromptChars: t.PromptChars, Latency: t.Latency})
	e.rec.Observe(Event{Kind: EventFinal, Round: e.rounds, Text: t.Answer})
}

// Close flushes and closes every open transcript.
func (st *Store) Close() {
	if st == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	for id, e := range st.open {
		e.rec.Close()
		delete(st.open, id)
	}
	st.lru = nil
}

// entryLocked returns the entry for session, opening (or reopening) the
// transcript when needed. Caller holds st.mu.
func (st *Store) entryLocked(session, model string, tags []string) *storeEntry {
	if e, ok := st.open[session]; ok {
		st.touchLocked(session)
		return e
	}
	rec, err := NewRecorder(st.dir)
	if err != nil {
		st.warnLocked(err)
		return nil
	}
	// Start is a no-op when this recorder already holds the session, and the
	// header lands after the recorder's own lock is released (see Start).
	rec.Start(session, model, "", "", tags)
	e := &storeEntry{rec: rec, rounds: roundsOnDisk(st.dir, session)}
	st.open[session] = e
	st.touchLocked(session)
	return e
}

// touchLocked moves session to the most-recently-used end and closes the
// oldest transcript beyond the cap. Caller holds st.mu.
func (st *Store) touchLocked(session string) {
	for i, id := range st.lru {
		if id == session {
			st.lru = append(st.lru[:i], st.lru[i+1:]...)
			break
		}
	}
	st.lru = append(st.lru, session)
	for len(st.lru) > st.max {
		old := st.lru[0]
		st.lru = st.lru[1:]
		if e, ok := st.open[old]; ok {
			e.rec.Close()
			delete(st.open, old)
		}
	}
}

// roundsOnDisk counts the highest round already in a transcript, so a
// reopened session continues numbering instead of restarting at 1.
func roundsOnDisk(dir, session string) int {
	recs, err := LoadTranscript(dir, session)
	if err != nil {
		return 0 // first turn: nothing on disk yet
	}
	n := 0
	for _, r := range recs {
		if r.Round > n && (r.Kind == "round" || r.Kind == "final") {
			n = r.Round
		}
	}
	return n
}

// warnLocked reports a store-level failure once, mirroring Recorder's
// warn-once behaviour. Caller holds st.mu.
func (st *Store) warnLocked(err error) {
	if st.warned {
		return
	}
	st.warned = true
	st.warnErr = err
	fmt.Fprintf(os.Stderr, "chimera: gateway session recording disabled (%v)\n", err)
}
