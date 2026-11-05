package grokweb

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/logging"
	"github.com/chimera/chimera/internal/models"
	"github.com/chimera/chimera/internal/providers"
	"github.com/chimera/chimera/internal/providers/webapi"
	"github.com/go-rod/rod"
)

func init() {
	webapi.Register(config.ProviderGrok, func(cfg *config.Config) providers.Provider {
		return New(cfg)
	})
}

// Provider implements providers.Provider on top of grok.com's web API, with
// no browser (internal spec 014). Selected when TRANSPORT=webapi, or
// TRANSPORT=auto with an exported session.
//
// MEASURED DEVIATION from the no-history invariant: Grok's model backend is
// stateless per turn — the conversation structure persists server-side but
// the model only sees what the client sends (the web shell manages context
// client-side). This provider therefore keeps the transcript itself and
// folds it into every turn after the first, like the DOM transports do.
// The WS gateway (spec 014 §8) is the phase-2 path that may restore
// server-side context.
type Provider struct {
	cfg    *config.Config
	client *Client
	sess   *webapi.Session
	log    *logging.Logger

	mu    sync.Mutex
	convs map[string]convState
}

// convState is one thread's server-side conversation pointer plus the
// transcript this provider accumulated (the model-side context).
type convState struct {
	conversationID string
	responseID     string // last assistant node — the continuation pointer
	history        []turn
}

type turn struct {
	role string // "user" | "assistant"
	text string
}

// maxFoldChars caps the folded transcript; oldest turns are dropped first
// because the newest exchange is the one the model must answer.
const maxFoldChars = 30000

// foldPrompt renders the wire prompt for a turn after the first. Turn 1 goes
// out raw so single-turn behavior stays pristine.
func foldPrompt(history []turn, text string) string {
	if len(history) == 0 {
		return text
	}
	var b strings.Builder
	b.WriteString("<conversation>\n")
	budget := maxFoldChars
	// Oldest-first print, but drop from the oldest when over budget.
	start := 0
	total := 0
	size := make([]int, len(history))
	for i, t := range history {
		size[i] = len(t.role) + len(t.text) + 4
		total += size[i]
	}
	for total > budget && start < len(history)-1 {
		total -= size[start]
		start++
	}
	// history alternates user/assistant; print pairs.
	for i := start; i+1 < len(history); i += 2 {
		b.WriteString("User: ")
		b.WriteString(history[i].text)
		b.WriteString("\n\nAssistant: ")
		b.WriteString(history[i+1].text)
		b.WriteString("\n\n")
	}
	b.WriteString("</conversation>\n\nUser: ")
	b.WriteString(text)
	b.WriteString("\n\nRespond to the latest user message, using the conversation above for context. Reply as the Assistant only.")
	return b.String()
}

// New returns an uninitialized web transport; call Init before serving.
func New(cfg *config.Config) *Provider {
	return &Provider{
		cfg:   cfg,
		convs: map[string]convState{},
		log:   logging.New("grok-web", cfg.LogDir, cfg.LogLevel, cfg.Verbose),
	}
}

// Name is the provider identifier; telemetry and routing keep using "grok".
func (p *Provider) Name() string { return config.ProviderGrok }

// ModelID stays in the chimera-* family so clients do not change.
func (p *Provider) ModelID() string { return "chimera-grok" }

// Browserless reports that this transport replays the web API, so the DOM
// send-button length limit does not apply.
func (p *Provider) Browserless() bool { return true }

// Init loads the storage-state session and probes it with the quota endpoint
// (a cheap authenticated call that also reports remaining queries). The
// rod.Page argument is unused (the interface still carries it until spec
// 011's transport split lands).
func (p *Provider) Init(_ *rod.Page, cfg *config.Config) error {
	sess, err := webapi.LoadSession(cfg.SessionPath(config.ProviderGrok))
	if err != nil {
		return err
	}
	p.sess = sess
	p.client = NewClient(sess, cfg.ResponseTimeout+30*time.Second, cfg.GrokWebModel)
	if path := os.Getenv("GROK_DUMP"); path != "" {
		if f, err := os.Create(path); err == nil {
			p.client.DebugDump = f
			p.log.Infof("dumping raw grok frames to %s", path)
		}
	}
	remaining, err := p.client.RateLimits()
	if err != nil {
		return fmt.Errorf("grok web session rejected (re-export %s): %w", cfg.SessionPath(config.ProviderGrok), err)
	}
	p.log.Infof("grok session accepted, remainingQueries=%d", remaining)
	return nil
}

// SendMessage performs one turn. A stable threadID reuses its conversation
// and its accumulated transcript; anything else starts fresh. A failed
// continuation retries once on a fresh conversation — the same self-heal as
// DeepSeek's stale parent and Gemini's stale triple.
func (p *Provider) SendMessage(text string, threadID string) (*models.ProviderResponse, error) {
	start := time.Now()

	p.mu.Lock()
	st, ok := p.convs[threadID]
	p.mu.Unlock()

	wire := foldPrompt(st.history, text)
	meta := TurnMeta{ConversationID: st.conversationID, ResponseID: st.responseID}
	res, err := p.client.Send(wire, meta)
	if err != nil && ok && !errors.Is(err, webapi.ErrWAF) && !errors.Is(err, webapi.ErrSessionExpired) {
		p.log.Warnf("grok continuation failed (%v); retrying on a fresh conversation", err)
		meta = TurnMeta{}
		res, err = p.client.Send(wire, meta)
	}
	if err != nil {
		return nil, err
	}

	// Accumulate the transcript (the model-side context Grok does not keep).
	if strings.TrimSpace(res.Content) != "" {
		st.history = append(st.history, turn{role: "user", text: text}, turn{role: "assistant", text: res.Content})
	}

	p.mu.Lock()
	p.convs[threadID] = convState{
		conversationID: res.ConversationID,
		responseID:     res.ResponseID,
		history:        st.history,
	}
	p.mu.Unlock()

	p.log.Infof("turn done conv=%s prompt_chars=%d history_turns=%d out_chars=%d reasoning_chars=%d total_ms=%d",
		shortID(res.ConversationID), len(wire), len(st.history), len(res.Content), len(res.Reasoning), res.Duration.Milliseconds())

	return &models.ProviderResponse{
		Message:   res.Content,
		ThreadID:  threadID,
		ElapsedMs: time.Since(start).Milliseconds(),
	}, nil
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// NewChat forgets every conversation and transcript so the next turn starts
// fresh.
func (p *Provider) NewChat() error {
	p.mu.Lock()
	p.convs = map[string]convState{}
	p.mu.Unlock()
	return nil
}

// ExtractResponse is DOM-only and unused on this transport.
func (p *Provider) ExtractResponse() (string, error) {
	return "", fmt.Errorf("ExtractResponse is not supported by the grok web transport")
}

// IsLoggedIn re-proves the jar against the quota endpoint.
func (p *Provider) IsLoggedIn() (bool, error) {
	if p.client == nil {
		return false, nil
	}
	if _, err := p.client.RateLimits(); err != nil {
		return false, nil
	}
	return true, nil
}

// SessionExpiry reports nothing: the credential is a cookie jar, not a JWT.
func (p *Provider) SessionExpiry() (time.Time, bool) {
	return time.Time{}, false
}

var _ providers.Provider = (*Provider)(nil)
