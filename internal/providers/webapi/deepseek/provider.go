package deepseekweb

import (
	"errors"
	"fmt"
	"os"
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
	webapi.Register(config.ProviderDeepSeek, func(cfg *config.Config) providers.Provider {
		return New(cfg)
	})
}

// Provider implements providers.Provider on top of the DeepSeek web API, with no
// browser. It is selected when TRANSPORT=webapi, or TRANSPORT=auto with an
// exported session (spec 012).
type Provider struct {
	cfg    *config.Config
	client *Client
	sess   *webapi.Session
	log    *logging.Logger

	mu    sync.Mutex
	convs map[string]convState
}

type convState struct {
	sessionID string
	parentID  string
}

// New returns an uninitialized web transport; call Init before serving.
func New(cfg *config.Config) *Provider {
	return &Provider{
		cfg:   cfg,
		convs: map[string]convState{},
		log:   logging.New("deepseek-web", cfg.LogDir, cfg.LogLevel, cfg.Verbose),
	}
}

// Name is the provider identifier; telemetry and routing keep using "deepseek".
func (p *Provider) Name() string { return config.ProviderDeepSeek }

// ModelID stays identical to the DOM transport so clients do not change.
func (p *Provider) ModelID() string { return "chimera-deepseek" }

// Init loads the storage-state session. The rod.Page argument is unused (the
// interface still carries it until spec 011's transport split lands).
func (p *Provider) Init(_ *rod.Page, cfg *config.Config) error {
	sess, err := webapi.LoadSession(cfg.SessionPath(config.ProviderDeepSeek))
	if err != nil {
		return err
	}
	p.sess = sess
	p.client = NewClient(sess, cfg.ResponseTimeout+30*time.Second)
	p.client.OnPowSolve = func(d time.Duration) { telemetry.ObservePowSolve(p.Name(), d) }
	// DEEPSEEK_SSE_DUMP=<path> writes the raw completion stream for wire-level
	// debugging. This is a vendor patch protocol with no public spec, so being
	// able to see the bytes is the difference between a fix and a guess.
	if path := os.Getenv("DEEPSEEK_SSE_DUMP"); path != "" {
		if f, err := os.Create(path); err == nil {
			p.client.DebugDump = f
			p.log.Infof("dumping raw completion SSE to %s", path)
		}
	}

	// A cheap authenticated call doubles as a session check. A 401/403 means the
	// token is dead; anything else (e.g. a CDN challenge) is left to request time
	// so a transient failure does not take the transport down.
	ok, err := p.IsLoggedIn()
	if err != nil {
		var he *webapi.HTTPError
		if errors.As(err, &he) && (he.Status == 401 || he.Status == 403) {
			return fmt.Errorf("deepseek web session rejected (re-export %s): %w", cfg.SessionPath(config.ProviderDeepSeek), err)
		}
		p.log.Warnf("deepseek session probe inconclusive: %v", err)
		return nil
	}
	if !ok {
		return fmt.Errorf("deepseek web session rejected (re-export %s)", cfg.SessionPath(config.ProviderDeepSeek))
	}
	return nil
}

// SendMessage performs one turn. A stable threadID reuses its conversation;
// otherwise a new chat session is created.
func (p *Provider) SendMessage(text string, threadID string) (*models.ProviderResponse, error) {
	start := time.Now()

	p.mu.Lock()
	conv, ok := p.convs[threadID]
	p.mu.Unlock()

	if !ok {
		sessionID, err := p.client.CreateChatSession()
		if err != nil {
			return nil, err
		}
		conv = convState{sessionID: sessionID}
	}

	res, err := p.client.Send(conv.sessionID, conv.parentID, text, p.cfg.DeepSeekWebThinking)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	parent := res.ResponseID
	if parent == "" {
		parent = conv.parentID
	}
	p.convs[threadID] = convState{sessionID: conv.sessionID, parentID: parent}
	p.mu.Unlock()

	in := usageInt(res.Usage, "prompt_tokens")
	if in == 0 {
		in = usageInt(res.Usage, "input_tokens")
	}
	out := usageInt(res.Usage, "completion_tokens")
	if out == 0 {
		out = usageInt(res.Usage, "output_tokens")
	}
	gen := res.Duration - res.TTFT
	tps := 0.0
	if gen > 0 && out > 0 {
		tps = float64(out) / gen.Seconds()
	}
	p.log.Infof("turn done session=%s prompt_chars=%d in=%d out=%d ttft_ms=%d total_ms=%d gen_ms=%d out_tok_s=%.1f reasoning_chars=%d",
		shortID(conv.sessionID), len(text), in, out,
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
	return "", fmt.Errorf("ExtractResponse is not supported by the deepseek web transport")
}

// IsLoggedIn verifies the session with a cheap authenticated call.
func (p *Provider) IsLoggedIn() (bool, error) {
	if p.client == nil {
		return false, nil
	}
	if _, err := p.client.CreatePowChallenge(CompletionPath); err != nil {
		return false, err
	}
	return true, nil
}

// SessionExpiry reports the token's expiry when it carries an exp claim.
func (p *Provider) SessionExpiry() (time.Time, bool) {
	if p.sess == nil {
		return time.Time{}, false
	}
	return p.sess.ExpiresAt()
}

var _ providers.Provider = (*Provider)(nil)
