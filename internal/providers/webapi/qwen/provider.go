package qwenweb

import (
	"fmt"
	"sync"
	"time"

	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/models"
	"github.com/chimera/chimera/internal/providers"
	"github.com/go-rod/rod"
)

// Provider implements providers.Provider on top of the qwen web API, with no
// browser. It is selected when TRANSPORT=webapi, or TRANSPORT=auto with an
// exported session (spec 011).
type Provider struct {
	cfg    *config.Config
	client *Client

	mu    sync.Mutex
	convs map[string]convState
}

type convState struct {
	chatID   string
	parentID string
}

// New returns an uninitialized web transport; call Init before serving.
func New(cfg *config.Config) *Provider {
	return &Provider{cfg: cfg, convs: map[string]convState{}}
}

// Name is the provider identifier; telemetry and routing keep using "qwen".
func (p *Provider) Name() string { return config.ProviderQwen }

// ModelID stays identical to the DOM transport so clients do not change.
func (p *Provider) ModelID() string { return "chimera-qwen" }

// Init loads the storage-state session. The rod.Page argument is unused (the
// interface still carries it until spec 011's transport split lands).
func (p *Provider) Init(_ *rod.Page, cfg *config.Config) error {
	sess, err := LoadSession(cfg.QwenSessionPath())
	if err != nil {
		return err
	}
	timeout := cfg.ResponseTimeout + 30*time.Second
	p.client = NewClient(sess, timeout)
	if ok, err := p.IsLoggedIn(); err != nil {
		return fmt.Errorf("qwen web session check: %w", err)
	} else if !ok {
		return fmt.Errorf("qwen web session rejected (re-export %s)", cfg.QwenSessionPath())
	}
	return nil
}

// SendMessage performs one turn. A stable threadID reuses its conversation;
// otherwise a new chat is created (the flattened prompt is self-contained).
func (p *Provider) SendMessage(text string, threadID string) (*models.ProviderResponse, error) {
	start := time.Now()

	p.mu.Lock()
	conv, ok := p.convs[threadID]
	p.mu.Unlock()

	if !ok {
		chatID, err := p.client.NewChat(p.cfg.QwenWebModel)
		if err != nil {
			return nil, err
		}
		conv = convState{chatID: chatID}
	}

	res, err := p.client.Send(p.cfg.QwenWebModel, conv.chatID, conv.parentID, text, p.cfg.QwenWebThinking)
	if err != nil {
		return nil, err
	}

	parent := res.ResponseID
	if parent == "" {
		parent = conv.parentID
	}
	p.mu.Lock()
	p.convs[threadID] = convState{chatID: conv.chatID, parentID: parent}
	p.mu.Unlock()

	return &models.ProviderResponse{
		Message:   res.Content,
		ThreadID:  threadID,
		ElapsedMs: time.Since(start).Milliseconds(),
	}, nil
}

// NewChat forgets every conversation so the next turn starts fresh.
func (p *Provider) NewChat() error {
	p.mu.Lock()
	p.convs = map[string]convState{}
	p.mu.Unlock()
	return nil
}

// ExtractResponse is DOM-only and unused on this transport.
func (p *Provider) ExtractResponse() (string, error) {
	return "", fmt.Errorf("ExtractResponse is not supported by the qwen web transport")
}

// IsLoggedIn validates the session against /api/models.
func (p *Provider) IsLoggedIn() (bool, error) {
	if p.client == nil {
		return false, nil
	}
	if _, err := p.client.ListModels(); err != nil {
		return false, err
	}
	return true, nil
}

var _ providers.Provider = (*Provider)(nil)
