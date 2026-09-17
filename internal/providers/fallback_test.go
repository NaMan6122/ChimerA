package providers

import (
	"errors"
	"testing"

	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/models"
	"github.com/go-rod/rod"
)

type fakeProvider struct {
	name  string
	reply string
	errs  []error // consumed one per call; empty = success
	calls int
}

func (f *fakeProvider) Name() string                             { return f.name }
func (f *fakeProvider) ModelID() string                          { return "chimera-" + f.name }
func (f *fakeProvider) Init(_ *rod.Page, _ *config.Config) error { return nil }
func (f *fakeProvider) NewChat() error                           { return nil }
func (f *fakeProvider) ExtractResponse() (string, error)         { return f.reply, nil }
func (f *fakeProvider) IsLoggedIn() (bool, error)                { return true, nil }

func (f *fakeProvider) SendMessage(text, threadID string) (*models.ProviderResponse, error) {
	f.calls++
	if f.calls <= len(f.errs) {
		return nil, f.errs[f.calls-1]
	}
	return &models.ProviderResponse{Message: f.reply, ThreadID: threadID}, nil
}

var errRetryable = errors.New("waf challenge")

func newFallback(primary, secondary *fakeProvider, onFallback func(from, to, reason string)) *Fallback {
	fb := NewFallback(primary, secondary,
		func(err error) bool { return errors.Is(err, errRetryable) },
		func(err error) string { return "waf" })
	fb.FromTransport, fb.ToTransport = "webapi", "dom"
	fb.OnFallback = onFallback
	return fb
}

func TestFallbackPrimarySuccess(t *testing.T) {
	primary := &fakeProvider{name: "qwen", reply: "ok"}
	secondary := &fakeProvider{name: "qwen", reply: "dom"}
	fb := newFallback(primary, secondary, func(string, string, string) {
		t.Fatal("fallback must not fire on success")
	})

	resp, err := fb.SendMessage("hi", "")
	if err != nil || resp.Message != "ok" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if primary.calls != 1 || secondary.calls != 0 {
		t.Fatalf("calls primary=%d secondary=%d", primary.calls, secondary.calls)
	}
}

func TestFallbackAfterRetry(t *testing.T) {
	primary := &fakeProvider{name: "qwen", errs: []error{errRetryable, errRetryable}}
	secondary := &fakeProvider{name: "qwen", reply: "dom"}
	var from, to, reason string
	fb := newFallback(primary, secondary, func(f, tt, r string) { from, to, reason = f, tt, r })

	resp, err := fb.SendMessage("hi", "s1")
	if err != nil || resp.Message != "dom" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if primary.calls != 2 {
		t.Fatalf("primary calls = %d, want one retry (2)", primary.calls)
	}
	if secondary.calls != 1 {
		t.Fatalf("secondary calls = %d, want 1", secondary.calls)
	}
	if from != "webapi" || to != "dom" || reason != "waf" {
		t.Fatalf("fallback labels = %s/%s/%s", from, to, reason)
	}
}

func TestFallbackRetrySucceeds(t *testing.T) {
	primary := &fakeProvider{name: "qwen", reply: "recovered", errs: []error{errRetryable}}
	secondary := &fakeProvider{name: "qwen", reply: "dom"}
	fb := newFallback(primary, secondary, func(string, string, string) {
		t.Fatal("fallback must not fire when the retry succeeds")
	})

	resp, err := fb.SendMessage("hi", "")
	if err != nil || resp.Message != "recovered" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if primary.calls != 2 || secondary.calls != 0 {
		t.Fatalf("calls primary=%d secondary=%d", primary.calls, secondary.calls)
	}
}

func TestFallbackNonRetryable(t *testing.T) {
	hard := errors.New("bad request")
	primary := &fakeProvider{name: "qwen", errs: []error{hard}}
	secondary := &fakeProvider{name: "qwen", reply: "dom"}
	fb := newFallback(primary, secondary, func(string, string, string) {
		t.Fatal("fallback must not fire on non-retryable errors")
	})

	if _, err := fb.SendMessage("hi", ""); !errors.Is(err, hard) {
		t.Fatalf("err = %v, want %v", err, hard)
	}
	if primary.calls != 1 || secondary.calls != 0 {
		t.Fatalf("calls primary=%d secondary=%d", primary.calls, secondary.calls)
	}
}
