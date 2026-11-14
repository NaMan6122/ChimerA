// Package glmweb — the provider half. The client half (client.go) speaks the
// wire; this file implements providers.Provider on top of it: session load,
// thread→conversation continuity, and the self-healing retry.
package glmweb

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
	webapi.Register(config.ProviderGLM, func(cfg *config.Config) providers.Provider {
		return New(cfg)
	})
}

// Provider implements providers.Provider on top of chatglm.cn's web API.
// Continuity is NATIVE (server-side conversation_id), so unlike Grok there is
// no transcript folding: the no-history invariant holds — every turn sends
// only what is new.
type Provider struct {
	cfg    *config.Config
	client *Client
	sess   *webapi.Session
	log    *logging.Logger

	convs webapi.ConvStore[string] // threadID → upstream conversation_id
}

// New returns an uninitialized web transport; call Init before serving.
func New(cfg *config.Config) *Provider {
	return &Provider{
		cfg: cfg,
		log: logging.New("glm-web", cfg.LogDir, cfg.LogLevel, cfg.Verbose),
	}
}

// Name is the provider identifier.
func (p *Provider) Name() string { return config.ProviderGLM }

// ModelID stays in the chimera-* family so clients do not change.
func (p *Provider) ModelID() string { return "chimera-glm" }

// Browserless reports that this transport replays the web API, so the DOM
// send-button length limit does not apply.
func (p *Provider) Browserless() bool { return true }

// Init loads the exported session and validates it with a real refresh call
// (which also caches a fresh 24h access token). The rod.Page argument is
// unused (the interface still carries it until spec 011's transport split).
func (p *Provider) Init(_ *rod.Page, cfg *config.Config) error {
	path := cfg.SessionPath(config.ProviderGLM)
	sess, err := webapi.LoadSession(path)
	if err != nil {
		return err
	}
	p.sess = sess
	p.client = NewClient(sess, cfg.ResponseTimeout+30*time.Second, cfg.GLMWebModel, path)
	if cfg.GLMURL != "" {
		p.client.Base = cfg.GLMURL
	}
	if path := os.Getenv("GLM_DUMP"); path != "" {
		if f, err := os.Create(path); err == nil {
			p.client.DebugDump = f
			p.log.Infof("dumping raw glm frames to %s", path)
		}
	}
	// The refresh doubles as the login probe: an accepted mint proves both
	// the session and the signing path in one call.
	if err := p.client.Refresh(); err != nil {
		return fmt.Errorf("glm session rejected (re-export %s): %w", path, err)
	}
	p.log.Infof("glm session accepted (assistant selector %q)", cfg.GLMWebModel)
	return nil
}

// SendMessage performs one turn. threadID reuses its server-side
// conversation; anything else starts fresh. A failed continuation retries
// once on a new conversation — the same self-heal as Grok's stale pointer
// and Gemini's stale triple.
func (p *Provider) SendMessage(text string, threadID string) (*models.ProviderResponse, error) {
	convID, _ := p.convs.Get(threadID)

	res, err := p.client.Send(text, convID)
	if err != nil && convID != "" && !errors.Is(err, webapi.ErrSessionExpired) {
		p.log.Warnf("glm continuation failed (%v); retrying on a fresh conversation", err)
		res, err = p.client.Send(text, "")
	}
	if err != nil {
		return nil, err
	}

	// Hygiene: if this turn replaced a conversation (the self-heal path), the
	// old one is orphaned server-side — delete it off the response path. A
	// failed delete only leaves one entry in the account's history list.
	if orphaned := convID; orphaned != "" && res.ConversationID != "" && orphaned != res.ConversationID {
		go func(id string) {
			if err := p.client.DeleteConversation(id); err != nil {
				p.log.Warnf("conversation hygiene failed for %s: %v", shortID(id), err)
			}
		}(orphaned)
	}

	p.convs.Set(threadID, res.ConversationID)

	// Reasoning is parsed, counted, and discarded at the provider boundary —
	// vendors do not replay it, so it never enters any working copy.
	p.log.Infof("turn done conv=%s prompt_chars=%d out_chars=%d reasoning_chars=%d total_ms=%d",
		shortID(res.ConversationID), len(text), len(res.Content), len(res.Reasoning), res.Duration.Milliseconds())

	return &models.ProviderResponse{
		Message:   res.Content,
		ThreadID:  threadID,
		ElapsedMs: res.Duration.Milliseconds(),
	}, nil
}

// IsLoggedIn probes with a cheap refresh: an accepted mint is the proof.
func (p *Provider) IsLoggedIn() (bool, error) {
	if err := p.client.Refresh(); err != nil {
		return false, err
	}
	return true, nil
}

// NewChat forgets every conversation pointer so the next turn starts a
// fresh upstream conversation.
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
