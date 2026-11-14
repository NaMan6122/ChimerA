// Package kimiweb — the provider half. The client half (client.go) speaks the
// wire; this file implements providers.Provider on top of it: session load,
// thread→chat continuity, and the self-healing retry.
package kimiweb

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/logging"
	"github.com/chimera/chimera/internal/models"
	"github.com/chimera/chimera/internal/providers"
	"github.com/chimera/chimera/internal/providers/webapi"
	"github.com/go-rod/rod"
)

func init() {
	webapi.Register(config.ProviderKimi, func(cfg *config.Config) providers.Provider {
		return New(cfg)
	})
}

// convRef is one thread's server-side position: the chat plus the last
// assistant message the next turn must hang off.
type convRef struct {
	chatID   string
	parentID string
}

// Provider implements providers.Provider on top of kimi.ai's web API.
// Continuity is NATIVE (chat_id + parent_id), so unlike Grok there is no
// transcript folding: the no-history invariant holds — every turn sends only
// what is new.
type Provider struct {
	cfg    *config.Config
	client *Client
	sess   *webapi.Session
	log    *logging.Logger

	convs webapi.ConvStore[convRef] // threadID → upstream chat position
}

// New returns an uninitialized web transport; call Init before serving.
func New(cfg *config.Config) *Provider {
	return &Provider{
		cfg: cfg,
		log: logging.New("kimi-web", cfg.LogDir, cfg.LogLevel, cfg.Verbose),
	}
}

// Name is the provider identifier.
func (p *Provider) Name() string { return config.ProviderKimi }

// ModelID stays in the chimera-* family so clients do not change.
func (p *Provider) ModelID() string { return "chimera-kimi" }

// Browserless reports that this transport replays the web API, so the DOM
// send-button length limit does not apply.
func (p *Provider) Browserless() bool { return true }

// Init loads the exported session and validates it with a real refresh call
// (which also caches a fresh access token). The rod.Page argument is unused
// (the interface still carries it until spec 011's transport split).
func (p *Provider) Init(_ *rod.Page, cfg *config.Config) error {
	path := cfg.SessionPath(config.ProviderKimi)
	sess, err := webapi.LoadSession(path)
	if err != nil {
		return err
	}
	p.sess = sess
	p.client = NewClient(sess, cfg.ResponseTimeout+30*time.Second, cfg.KimiWebModel, path)
	if cfg.KimiWebURL != "" {
		p.client.Base = cfg.KimiWebURL
	}
	if path := os.Getenv("KIMI_DUMP"); path != "" {
		if f, err := os.Create(path); err == nil {
			p.client.DebugDump = f
			p.log.Infof("dumping raw kimi frames to %s", path)
		}
	}
	// The refresh doubles as the login probe: an accepted credential proves
	// the session in one call.
	if err := p.client.Refresh(); err != nil {
		return fmt.Errorf("kimi session rejected (re-export %s): %w", path, err)
	}
	p.log.Infof("kimi session accepted (model selector %q)", cfg.KimiWebModel)
	return nil
}

// SendMessage performs one turn. threadID reuses its server-side chat; anything
// else starts fresh. A failed continuation retries once on a new chat — the
// same self-heal as GLM's stale conversation and Grok's stale pointer.
func (p *Provider) SendMessage(text string, threadID string) (*models.ProviderResponse, error) {
	ref, _ := p.convs.Get(threadID)

	res, err := p.client.Send(text, ref.chatID, ref.parentID)
	if err != nil && ref.chatID != "" && !errors.Is(err, webapi.ErrSessionExpired) {
		p.log.Warnf("kimi continuation failed (%v); retrying on a fresh chat", err)
		res, err = p.client.Send(text, "", "")
	}
	if err != nil {
		return nil, err
	}

	p.convs.Set(threadID, convRef{chatID: res.ChatID, parentID: res.ParentID})

	// Reasoning is parsed, counted, and discarded at the provider boundary —
	// vendors do not replay it, so it never enters any working copy.
	p.log.Infof("turn done chat=%s prompt_chars=%d out_chars=%d reasoning_chars=%d total_ms=%d",
		shortID(res.ChatID), len(text), len(res.Content), len(res.Reasoning), res.Duration.Milliseconds())

	return &models.ProviderResponse{
		Message:   res.Content,
		ThreadID:  threadID,
		ElapsedMs: res.Duration.Milliseconds(),
	}, nil
}

// IsLoggedIn probes with a cheap refresh: an accepted credential is the proof.
func (p *Provider) IsLoggedIn() (bool, error) {
	if err := p.client.Refresh(); err != nil {
		return false, err
	}
	return true, nil
}

// NewChat forgets every chat pointer so the next turn starts a fresh
// upstream chat.
func (p *Provider) NewChat() error {
	p.convs.Clear()
	return nil
}

// ExtractResponse satisfies the DOM-shaped interface; the web transport
// returns complete turns from SendMessage and never extracts from a page.
func (p *Provider) ExtractResponse() (string, error) {
	return "", nil
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
