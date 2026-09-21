package chatgptweb

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/logging"
	"github.com/chimera/chimera/internal/models"
	"github.com/chimera/chimera/internal/providers"
	"github.com/chimera/chimera/internal/providers/webapi"
	"github.com/chimera/chimera/internal/telemetry"
	"github.com/go-rod/rod"
)

func init() {
	webapi.Register(config.ProviderChatGPT, func(cfg *config.Config) providers.Provider {
		return New(cfg)
	})
}

// Provider implements providers.Provider on top of the ChatGPT backend-api, with
// no browser. It is selected when TRANSPORT=webapi, or TRANSPORT=auto with an
// exported session (spec 012 §3).
type Provider struct {
	cfg    *config.Config
	client *Client
	sess   *webapi.Session
	log    *logging.Logger

	mu    sync.Mutex
	convs map[string]convState
}

type convState struct {
	conversationID  string
	parentMessageID string
}

// New returns an uninitialized web transport; call Init before serving.
func New(cfg *config.Config) *Provider {
	return &Provider{
		cfg:   cfg,
		convs: map[string]convState{},
		log:   logging.New("chatgpt-web", cfg.LogDir, cfg.LogLevel, cfg.Verbose),
	}
}

// Name is the provider identifier; telemetry and routing keep using "chatgpt".
func (p *Provider) Name() string { return config.ProviderChatGPT }

// ModelID stays identical to the DOM transport so clients do not change.
func (p *Provider) ModelID() string { return "chimera-chatgpt" }

// Init loads the storage-state session and mints a bearer from its cookie jar.
// The rod.Page argument is unused (the interface still carries it until spec
// 011's transport split lands).
func (p *Provider) Init(_ *rod.Page, cfg *config.Config) error {
	sess, err := webapi.LoadSession(cfg.SessionPath(config.ProviderChatGPT))
	if err != nil {
		return err
	}
	p.sess = sess
	p.client = NewClient(sess, cfg.ResponseTimeout+30*time.Second)
	p.client.OnPowSolve = func(d time.Duration) { telemetry.ObservePowSolve(p.Name(), d) }

	ok, err := p.IsLoggedIn()
	if err != nil {
		var he *webapi.HTTPError
		if errors.As(err, &he) && (he.Status == 401 || he.Status == 403) {
			return fmt.Errorf("chatgpt web session rejected (re-export %s): %w", cfg.SessionPath(config.ProviderChatGPT), err)
		}
		if errors.Is(err, webapi.ErrSessionExpired) {
			return fmt.Errorf("chatgpt web session rejected (re-export %s): %w", cfg.SessionPath(config.ProviderChatGPT), err)
		}
		p.log.Warnf("chatgpt session probe inconclusive: %v", err)
		return nil
	}
	if !ok {
		return fmt.Errorf("chatgpt web session rejected (re-export %s)", cfg.SessionPath(config.ProviderChatGPT))
	}
	return nil
}

// SendMessage performs one turn. A stable threadID reuses its conversation;
// otherwise a new one is created server-side by omitting conversation_id.
func (p *Provider) SendMessage(text string, threadID string) (*models.ProviderResponse, error) {
	start := time.Now()

	p.mu.Lock()
	conv, ok := p.convs[threadID]
	p.mu.Unlock()

	var conversationID, parentID string
	if ok {
		conversationID, parentID = conv.conversationID, conv.parentMessageID
	}

	res, err := p.client.Send(p.cfg.ChatGPTWebModel, conversationID, parentID, text)
	if err != nil {
		return nil, err
	}
	if res.Conversation == "" {
		res.Conversation = conversationID
	}

	p.mu.Lock()
	parent := res.MessageID
	if parent == "" {
		parent = parentID
	}
	p.convs[threadID] = convState{conversationID: res.Conversation, parentMessageID: parent}
	p.mu.Unlock()

	gen := res.Duration - res.TTFT
	p.log.Infof("turn done conversation=%s prompt_chars=%d ttft_ms=%d total_ms=%d gen_ms=%d",
		shortID(res.Conversation), len(text),
		res.TTFT.Milliseconds(), res.Duration.Milliseconds(), gen.Milliseconds())

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

// NewChat forgets every conversation so the next turn starts fresh.
func (p *Provider) NewChat() error {
	p.mu.Lock()
	p.convs = map[string]convState{}
	p.mu.Unlock()
	return nil
}

// ExtractResponse is DOM-only and unused on this transport.
func (p *Provider) ExtractResponse() (string, error) {
	return "", fmt.Errorf("ExtractResponse is not supported by the chatgpt web transport")
}

// IsLoggedIn verifies the cookie jar by minting a bearer.
func (p *Provider) IsLoggedIn() (bool, error) {
	if p.client == nil {
		return false, nil
	}
	return p.client.IsLoggedIn()
}

// SessionExpiry reports the cookie-token's expiry when it is a JWT. ChatGPT is
// cookie-custody, so a missing exp claim is normal, not an error.
func (p *Provider) SessionExpiry() (time.Time, bool) {
	if p.sess == nil {
		return time.Time{}, false
	}
	return p.sess.ExpiresAt()
}

var _ providers.Provider = (*Provider)(nil)
