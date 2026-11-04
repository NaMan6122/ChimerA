package spin

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Record is one line of a session transcript. The transcript is written for
// humans and handoffs; nothing in it is ever read back into a request.
type Record struct {
	TS          string  `json:"ts"`
	Kind        string  `json:"kind"` // session | user | round | tool | result | final | note | error
	Session     string  `json:"session,omitempty"`
	Model       string  `json:"model,omitempty"`
	BaseURL     string  `json:"base_url,omitempty"`
	Root        string  `json:"root,omitempty"`
	Round       int     `json:"round,omitempty"`
	Text        string  `json:"text,omitempty"`
	Name        string  `json:"name,omitempty"`
	Arguments   string  `json:"arguments,omitempty"`
	Result      string  `json:"result,omitempty"`
	PromptChars int     `json:"prompt_chars,omitempty"`
	LatencyS    float64 `json:"latency_s,omitempty"`
}

// Recorder appends a session transcript to disk, one JSON object per line.
//
// It is deliberately outside the agentic loop: it only observes, never returns
// data to the caller, never feeds anything back into a request, and never blocks
// a turn. A write failure disables recording with one warning rather than
// breaking the conversation — losing a transcript must not cost an answer.
type Recorder struct {
	mu      sync.Mutex
	dir     string
	path    string
	session string
	f       *os.File
	warned  bool
}

// DefaultSessionsDir is $CHIMERA_SESSIONS_DIR, else ~/.local/share/chimera/sessions.
func DefaultSessionsDir() string {
	if v := strings.TrimSpace(os.Getenv("CHIMERA_SESSIONS_DIR")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".chimera-sessions"
	}
	return filepath.Join(home, ".local", "share", "chimera", "sessions")
}

// NewRecorder prepares a recorder rooted at dir.
func NewRecorder(dir string) (*Recorder, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Recorder{dir: dir}, nil
}

// Start begins (or rotates to) a transcript for one session.
//
// The header is appended after the lock is released: append() locks too, and
// holding the lock across it deadlocked the first version of this file.
func (r *Recorder) Start(session, model, baseURL, root string) {
	r.mu.Lock()

	if r.session == session && r.f != nil {
		r.mu.Unlock()
		return
	}
	if r.f != nil {
		_ = r.f.Close()
		r.f = nil
	}
	r.session = session

	name := session
	if name == "" {
		name = fmt.Sprintf("session-%d", time.Now().UnixNano())
	}
	r.path = filepath.Join(r.dir, sanitize(name)+".jsonl")

	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		r.mu.Unlock()
		r.warn(err)
		return
	}
	r.f = f
	r.mu.Unlock()

	r.append(Record{Kind: "session", Session: session, Model: model, BaseURL: baseURL, Root: root})
}

// Observe maps a loop event onto a transcript record. It is safe to call from
// any goroutine and never blocks on anything slower than an append.
func (r *Recorder) Observe(ev Event) {
	switch ev.Kind {
	case EventTask:
		r.Note("user", ev.Text)
	case EventRound:
		r.append(Record{Kind: "round", Round: ev.Round, Text: ev.Text,
			PromptChars: ev.PromptChars, LatencyS: ev.Latency.Seconds()})
	case EventTool:
		r.append(Record{Kind: "tool", Round: ev.Round, Name: ev.Tool.Function.Name,
			Arguments: ev.Tool.Function.Arguments})
	case EventResult:
		r.append(Record{Kind: "result", Round: ev.Round, Name: ev.Tool.Function.Name, Result: ev.Result})
	case EventFinal:
		r.append(Record{Kind: "final", Round: ev.Round, Text: ev.Text})
	case EventNudge:
		r.Note("note", "answered without reading anything; asked for evidence")
	}
}

// Note records a free-form line (user message, error, marker).
func (r *Recorder) Note(kind, text string) {
	r.append(Record{Kind: kind, Text: text})
}

// Path is the transcript file currently being written, or "".
func (r *Recorder) Path() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.path
}

// Close flushes and closes the current transcript.
func (r *Recorder) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f != nil {
		_ = r.f.Close()
		r.f = nil
	}
}

func (r *Recorder) append(rec Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return
	}
	rec.TS = time.Now().UTC().Format(time.RFC3339Nano)
	rec.Session = r.session
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if _, err := r.f.Write(append(line, '\n')); err != nil {
		r.warn(err)
	}
}

func (r *Recorder) warn(err error) {
	if r.warned {
		return
	}
	r.warned = true
	fmt.Fprintf(os.Stderr, "chimera: transcript disabled (%v)\n", err)
}

// LoadTranscript reads a session transcript. Used by the TUI to restore a
// previous conversation for display only — the agentic loop is unaffected.
func LoadTranscript(dir, session string) ([]Record, error) {
	path := filepath.Join(dir, sanitize(session)+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for sc.Scan() {
		var rec Record
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue // a torn last line must not lose the rest
		}
		out = append(out, rec)
	}
	return out, sc.Err()
}

// ListSessions returns transcript paths, newest first.
func ListSessions(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	return paths, nil
}

// sanitize keeps session ids from escaping the directory.
func sanitize(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, s)
	if s == "" {
		return "session"
	}
	return s
}
