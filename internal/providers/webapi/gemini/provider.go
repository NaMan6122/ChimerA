package geminiweb

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
	webapi.Register(config.ProviderGemini, func(cfg *config.Config) providers.Provider {
		return New(cfg)
	})
}

// Provider implements providers.Provider on top of gemini.google.com's web
// API, with no browser (spec 013). Selected when TRANSPORT=webapi, or
// TRANSPORT=auto with an exported session.
type Provider struct {
	cfg    *config.Config
	client *Client
	sess   *webapi.Session
	log    *logging.Logger

	mu    sync.Mutex
	convs map[string]TurnMeta
}

// New returns an uninitialized web transport; call Init before serving.
func New(cfg *config.Config) *Provider {
	return &Provider{
		cfg:   cfg,
		convs: map[string]TurnMeta{},
		log:   logging.New("gemini-web", cfg.LogDir, cfg.LogLevel, cfg.Verbose),
	}
}

// Name is the provider identifier; telemetry and routing keep using "gemini".
func (p *Provider) Name() string { return config.ProviderGemini }

// ModelID stays in the chimera-* family so clients do not change.
func (p *Provider) ModelID() string { return "chimera-gemini" }

// Browserless reports that this transport replays the web API, so the DOM
// send-button length limit does not apply.
func (p *Provider) Browserless() bool { return true }

// Init loads the storage-state session and proves it with the nonce scrape:
// SNlM0e is only issued to authenticated sessions (spec 013 §2). The rod.Page
// argument is unused (the interface still carries it until spec 011's
// transport split lands).
func (p *Provider) Init(_ *rod.Page, cfg *config.Config) error {
	sess, err := webapi.LoadSession(cfg.SessionPath(config.ProviderGemini))
	if err != nil {
		return err
	}
	p.sess = sess
	p.client = NewClient(sess, cfg.ResponseTimeout+30*time.Second, cfg.GeminiWebModel, cfg.GeminiWebThinking)
	if path := os.Getenv("GEMINI_DUMP"); path != "" {
		if f, err := os.Create(path); err == nil {
			p.client.DebugDump = f
			p.log.Infof("dumping raw gemini frames to %s", path)
		}
	}
	if err := p.client.ScrapeNonce(); err != nil {
		return fmt.Errorf("gemini web session rejected (re-export %s): %w", cfg.SessionPath(config.ProviderGemini), err)
	}
	return nil
}

// SendMessage performs one turn. A stable threadID reuses its (cid, rid,
// rcid) triple; anything else starts a fresh thread. When continuation fails
// — e.g. Google dropped the conversation server-side, the sibling of
// DeepSeek's biz_code 26 — the turn retries once on a fresh thread.
func (p *Provider) SendMessage(text string, threadID string) (*models.ProviderResponse, error) {
	start := time.Now()

	p.mu.Lock()
	meta, ok := p.convs[threadID]
	p.mu.Unlock()

	res, err := p.client.Send(text, meta)
	if err != nil && ok && !errors.Is(err, webapi.ErrWAF) && !errors.Is(err, webapi.ErrSessionExpired) {
		// Continuation-specific failures (stale triple) self-heal the same way
		// DeepSeek's stale parent id does: fresh conversation, replay nothing.
		p.log.Warnf("gemini continuation failed (%v); retrying on a fresh thread", err)
		meta = TurnMeta{}
		res, err = p.client.Send(text, meta)
	}
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	p.convs[threadID] = TurnMeta{
		ConversationID: res.ConversationID,
		ResponseID:     res.ResponseID,
		CandidateID:    res.CandidateID,
		TurnIndex:      meta.TurnIndex + 1,
	}
	p.mu.Unlock()

	p.log.Infof("turn done conv=%s turn=%d prompt_chars=%d out_chars=%d ttft=– total_ms=%d",
		shortID(res.ConversationID), meta.TurnIndex, len(text), len(res.Content), res.Duration.Milliseconds())

	return &models.ProviderResponse{
		Message:   cleanText(res.Content),
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

// NewChat forgets every conversation so the next turn starts fresh.
func (p *Provider) NewChat() error {
	p.mu.Lock()
	p.convs = map[string]TurnMeta{}
	p.mu.Unlock()
	return nil
}

// ExtractResponse is DOM-only and unused on this transport.
func (p *Provider) ExtractResponse() (string, error) {
	return "", fmt.Errorf("ExtractResponse is not supported by the gemini web transport")
}

// IsLoggedIn re-proves the jar: /app only carries SNlM0e for authenticated
// sessions. It also refreshes nonce/build-label as a side effect.
func (p *Provider) IsLoggedIn() (bool, error) {
	if p.client == nil {
		return false, nil
	}
	if err := p.client.ScrapeNonce(); err != nil {
		if strings.Contains(err.Error(), "no SNlM0e") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// SessionExpiry reports nothing: the credential is a cookie jar, not a JWT.
func (p *Provider) SessionExpiry() (time.Time, bool) {
	return time.Time{}, false
}

var _ providers.Provider = (*Provider)(nil)
