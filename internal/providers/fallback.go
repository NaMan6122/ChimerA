package providers

import (
	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/models"
	"github.com/go-rod/rod"
)

// Fallback routes chat turns to a secondary Provider when the primary fails
// retryably (spec 011 §4). Both transports are initialized by the caller with
// their own machinery (browser page vs HTTP session), so Init here is a no-op.
//
// Flow per turn: primary -> one primary retry -> secondary. A conversation
// never switches mid-stream; the flattened prompt is self-contained, so the
// fallback transport can serve the next turn from scratch.
type Fallback struct {
	Primary   Provider
	Secondary Provider

	// FromTransport/ToTransport are metric labels ("webapi", "dom").
	FromTransport string
	ToTransport   string

	// Retryable classifies failures that justify a retry and a fallback.
	Retryable func(error) bool
	// Reason maps a failure to a low-cardinality metric label.
	Reason func(error) string
	// OnFallback is invoked once when routing moves to the secondary.
	OnFallback func(from, to, reason string)
}

// NewFallback wires the wrapper with sensible defaults.
func NewFallback(primary, secondary Provider, retryable func(error) bool, reason func(error) string) *Fallback {
	return &Fallback{
		Primary:       primary,
		Secondary:     secondary,
		Retryable:     retryable,
		Reason:        reason,
		FromTransport: "primary",
		ToTransport:   "secondary",
	}
}

func (f *Fallback) Name() string    { return f.Primary.Name() }
func (f *Fallback) ModelID() string { return f.Primary.ModelID() }

// Init delegates to the primary; members must already be initialized.
func (f *Fallback) Init(_ *rod.Page, cfg *config.Config) error {
	if f.Primary == nil || f.Secondary == nil {
		return nil
	}
	_ = cfg
	return nil
}

// SendMessage tries primary, retries it once, then falls back.
func (f *Fallback) SendMessage(text string, threadID string) (*models.ProviderResponse, error) {
	resp, err := f.Primary.SendMessage(text, threadID)
	if err == nil {
		return resp, nil
	}
	if f.Retryable == nil || !f.Retryable(err) {
		return nil, err
	}

	// One retry before declaring the transport down.
	resp, err = f.Primary.SendMessage(text, threadID)
	if err == nil {
		return resp, nil
	}
	if !f.Retryable(err) {
		return nil, err
	}

	reason := "unknown"
	if f.Reason != nil {
		reason = f.Reason(err)
	}
	if f.OnFallback != nil {
		f.OnFallback(f.FromTransport, f.ToTransport, reason)
	}
	return f.Secondary.SendMessage(text, threadID)
}

// NewChat resets both transports so the next turn starts fresh everywhere.
func (f *Fallback) NewChat() error {
	errPrimary := f.Primary.NewChat()
	errSecondary := f.Secondary.NewChat()
	if errPrimary != nil {
		return errPrimary
	}
	return errSecondary
}

func (f *Fallback) ExtractResponse() (string, error) { return f.Primary.ExtractResponse() }

func (f *Fallback) IsLoggedIn() (bool, error) { return f.Primary.IsLoggedIn() }

var _ Provider = (*Fallback)(nil)
