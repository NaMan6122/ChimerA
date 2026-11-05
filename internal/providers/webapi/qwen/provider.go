package qwenweb

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
	"github.com/go-rod/rod"
)

func init() {
	webapi.Register(config.ProviderQwen, func(cfg *config.Config) providers.Provider {
		return New(cfg)
	})
}

// Provider implements providers.Provider on top of the qwen web API, with no
// browser. It is selected when TRANSPORT=webapi, or TRANSPORT=auto with an
// exported session (spec 011).
type Provider struct {
	cfg         *config.Config
	client      *Client
	sess        *Session
	sessionPath string
	log         *logging.Logger

	mu    sync.Mutex
	convs map[string]convState
}

type convState struct {
	chatID   string
	parentID string
}

// New returns an uninitialized web transport; call Init before serving.
func New(cfg *config.Config) *Provider {
	return &Provider{
		cfg:   cfg,
		convs: map[string]convState{},
		log:   logging.New("qwen-web", cfg.LogDir, cfg.LogLevel, cfg.Verbose),
	}
}

// Name is the provider identifier; telemetry and routing keep using "qwen".
func (p *Provider) Name() string { return config.ProviderQwen }

// ModelID stays identical to the DOM transport so clients do not change.
func (p *Provider) ModelID() string { return "chimera-qwen" }

// Browserless reports that this transport replays the web API, so the DOM
// send-button length limit does not apply.
func (p *Provider) Browserless() bool { return true }

// Init loads the storage-state session. The rod.Page argument is unused (the
// interface still carries it until spec 011's transport split lands).
func (p *Provider) Init(_ *rod.Page, cfg *config.Config) error {
	sess, err := webapi.LoadSession(cfg.QwenSessionPath())
	if err != nil {
		return err
	}
	timeout := cfg.ResponseTimeout + 30*time.Second
	p.sess = sess
	p.sessionPath = cfg.QwenSessionPath()
	p.client = NewClient(sess, timeout)

	// Access tokens live ~15 minutes now; the refresh_token cookie lives ~30
	// days. Refresh up front so a long-idle gateway starts with a live token
	// instead of failing its first turn.
	if err := p.refreshSession(); err != nil {
		p.log.Warnf("qwen token refresh failed (%v); trying the stored token", err)
	}

	if ok, err := p.IsLoggedIn(); err != nil {
		return fmt.Errorf("qwen web session check: %w", err)
	} else if !ok {
		return fmt.Errorf("qwen web session rejected (re-export %s)", p.sessionPath)
	}
	return nil
}

// refreshSession mints a new access token from the refresh_token cookie and
// persists both, so a restart does not need another round trip.
func (p *Provider) refreshSession() error {
	res, err := p.client.RefreshAccessToken()
	if err != nil {
		return err
	}
	p.client.SetToken(res.AccessToken)
	if p.sess == nil {
		return nil
	}
	p.sess.AccessToken = res.AccessToken
	if res.RefreshToken != "" {
		if p.sess.Cookies == nil {
			p.sess.Cookies = map[string]string{}
		}
		p.sess.Cookies["refresh_token"] = res.RefreshToken
	}
	if p.sessionPath != "" {
		if err := webapi.WriteSession(p.sess, p.sessionPath); err != nil {
			p.log.Warnf("qwen session refresh could not be saved: %v", err)
		}
	}
	p.log.Infof("qwen access token refreshed (valid ~15m)")
	return nil
}

// needsRefresh reports failures that a fresh token can plausibly fix.
func needsRefresh(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, webapi.ErrWAF) || errors.Is(err, webapi.ErrSessionExpired) {
		return true
	}
	var he *webapi.HTTPError
	if errors.As(err, &he) && (he.Status == 401 || he.Status == 403) {
		return true
	}
	return false
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

	// A rejected token looks like a WAF block from here. Refresh once and retry
	// the same turn before giving up: the access token only lives ~15 minutes.
	if needsRefresh(err) {
		p.log.Warnf("qwen turn rejected (%v); refreshing token and retrying", err)
		if rerr := p.refreshSession(); rerr == nil {
			res, err = p.client.Send(p.cfg.QwenWebModel, conv.chatID, conv.parentID, text, p.cfg.QwenWebThinking)
		}
	}
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	parent := res.ResponseID
	if parent == "" {
		parent = conv.parentID
	}
	p.convs[threadID] = convState{chatID: conv.chatID, parentID: parent}
	p.mu.Unlock()

	// Per-turn usage for latency/token reporting (parsed by scripts/latency_probe.py).
	in := usageInt(res.Usage, "input_tokens")
	out := usageInt(res.Usage, "output_tokens")
	cached := 0
	if details, ok := res.Usage["prompt_tokens_details"].(map[string]any); ok {
		cached = usageInt(details, "cached_tokens")
	}
	gen := res.Duration - res.TTFT
	tps := 0.0
	if gen > 0 && out > 0 {
		tps = float64(out) / gen.Seconds()
	}
	p.log.Infof("turn done chat=%s prompt_chars=%d in=%d out=%d cached=%d ttft_ms=%d total_ms=%d gen_ms=%d out_tok_s=%.1f reasoning_chars=%d",
		shortID(conv.chatID), len(text), in, out, cached,
		res.TTFT.Milliseconds(), res.Duration.Milliseconds(), gen.Milliseconds(), tps, len(res.Reasoning))

	return &models.ProviderResponse{
		Message:   res.Content,
		ThreadID:  threadID,
		ElapsedMs: time.Since(start).Milliseconds(),
	}, nil
}

func usageInt(u map[string]any, key string) int {
	if v, ok := u[key].(float64); ok {
		return int(v)
	}
	return 0
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
	return "", fmt.Errorf("ExtractResponse is not supported by the qwen web transport")
}

// IsLoggedIn validates the session.
//
// The stored JWT's exp claim is checked before any network probe: measured, a
// token 18 days past exp still listed models successfully while chat creation
// returned "unauthorized". Reporting OK there is worse than a slow check — it
// tells the user to retry a session that cannot work.
func (p *Provider) IsLoggedIn() (bool, error) {
	if p.sess != nil {
		if exp, ok := p.sess.ExpiresAt(); ok && time.Now().After(exp) {
			return false, fmt.Errorf("%w: token expired %s", webapi.ErrSessionExpired, exp.Format(time.RFC3339))
		}
	}
	if p.client == nil {
		return false, nil
	}
	if _, err := p.client.ListModels(); err != nil {
		return false, err
	}
	return true, nil
}

// SessionExpiry reports the token's expiry when the JWT carries an exp claim.
func (p *Provider) SessionExpiry() (time.Time, bool) {
	if p.sess == nil {
		return time.Time{}, false
	}
	return p.sess.ExpiresAt()
}

var _ providers.Provider = (*Provider)(nil)
