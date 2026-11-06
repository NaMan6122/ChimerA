// tools.go — the MCP tool surface: chat, refresh, providers.
//
// Every tool resolves providers and takes locks exactly the way
// internal/api does (providerForModel + acquireProviderLock semantics), feeds
// SendMessage its threadID the way handleChatCompletions does, and keeps
// telemetry/metering consistent with the HTTP path.
package mcp

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chimera/chimera/internal/config"
	"github.com/chimera/chimera/internal/logging"
	"github.com/chimera/chimera/internal/meter"
	"github.com/chimera/chimera/internal/providers"
	"github.com/chimera/chimera/internal/session"
	"github.com/chimera/chimera/internal/telemetry"
)

// Deps carries everything the MCP server needs — the same provider map and
// per-provider mutexes the HTTP path passes around. Meter is nil when
// metering is disabled; SessionMgr may be nil (session URLs are then not
// recorded).
type Deps struct {
	Cfg        *config.Config
	Providers  map[string]providers.Provider
	Mus        map[string]*sync.Mutex
	SessionMgr *session.Manager
	Meter      *meter.Store
	Log        *logging.Logger
	Version    string
}

// logger is the slice of *logging.Logger the package uses; a no-op stands in
// for tests that build Deps without a logger.
type logger interface {
	Debugf(format string, args ...any)
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
}

type nopLog struct{}

func (nopLog) Debugf(string, ...any) {}
func (nopLog) Infof(string, ...any)  {}
func (nopLog) Warnf(string, ...any)  {}
func (nopLog) Errorf(string, ...any) {}

func loggerFor(deps *Deps) logger {
	if deps != nil && deps.Log != nil {
		return deps.Log
	}
	return nopLog{}
}

// ── tools/list ────────────────────────────────────────────────

// toolDefinitions returns the advertised tools. Schemas are plain JSON Schema
// objects; keep them in sync with the argument parsing below.
func toolDefinitions() []map[string]any {
	return []map[string]any{
		{
			"name": "chat",
			"description": "Send one chat message to a Chimera provider (browser-backed ChatGPT/Claude/Qwen/DeepSeek/Kimi or a browserless web transport) " +
				"and return the assistant's reply text. Pass session_id to continue an upstream conversation; omit it for a fresh conversation per call.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"message": map[string]any{
						"type":        "string",
						"description": "The user message to send upstream.",
					},
					"provider": map[string]any{
						"type":        "string",
						"description": `Provider name or model id (e.g. "chatgpt", "chimera-qwen"). Defaults to the configured provider.`,
					},
					"session_id": map[string]any{
						"type":        "string",
						"description": "Reuse an id to continue its upstream conversation; omit for a fresh conversation per call.",
					},
				},
				"required": []string{"message"},
			},
		},
		{
			"name":        "refresh",
			"description": "Start a fresh upstream conversation for a provider (same as POST /v1/refresh), discarding provider-held thread state.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"provider": map[string]any{
						"type":        "string",
						"description": "Provider name or model id. Defaults to the configured provider.",
					},
				},
			},
		},
		{
			"name":        "providers",
			"description": "List registered Chimera providers with model ids, transport shape and cached login state. Never touches a browser.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

// ── tools/call routing ────────────────────────────────────────

// callTool runs one tool. Returns (text, isError) for a ToolResult, or a
// non-nil rpcErr for protocol-level failures (-32602). Provider-level
// failures are NOT rpcErrs: they become isError:true results with the
// provider's message so a driving model can react to them.
func (s *server) callTool(name string, args json.RawMessage) (string, bool, *RPCError) {
	switch name {
	case "chat":
		return s.toolChat(args)
	case "refresh":
		return s.toolRefresh(args)
	case "providers":
		return s.toolProviders(args)
	default:
		return "", false, &RPCError{
			Code:    codeInvalidParams,
			Message: fmt.Sprintf("unknown tool: %q (available: chat, refresh, providers)", name),
		}
	}
}

// chatArgs is the chat tool's input schema.
type chatArgs struct {
	Message   string `json:"message"`
	Provider  string `json:"provider"`
	SessionID string `json:"session_id"`
}

// toolChat resolves the provider, takes the provider lock the way the API
// layer does, sends one message, and reports the reply. Session continuity
// mirrors internal/api: session_id becomes the provider threadID and the
// conversation URL is recorded for the session manager; without one, every
// call gets a fresh thread (a pure, stateless function of the request).
func (s *server) toolChat(args json.RawMessage) (string, bool, *RPCError) {
	var a chatArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return "", false, &RPCError{Code: codeInvalidParams, Message: "invalid params for chat: " + err.Error()}
		}
	}
	if strings.TrimSpace(a.Message) == "" {
		return "", false, &RPCError{Code: codeInvalidParams, Message: `invalid params for chat: "message" is required`}
	}

	p, mu, name := s.providerFor(a.Provider)
	if p == nil {
		return fmt.Sprintf("no provider available for %q (registered: %s)", a.Provider, strings.Join(s.providerNames(), ", ")), true, nil
	}
	model := p.ModelID()

	threadID := a.SessionID
	if threadID == "" {
		// Fresh thread per call: a unique id means the provider starts a new
		// upstream conversation instead of continuing a stale one.
		threadID = fmt.Sprintf("mcp-%d", time.Now().UnixNano())
	}

	start := time.Now()
	if !s.acquire(mu) {
		telemetry.ObserveLockWait(name, time.Since(start))
		telemetry.ObserveProviderError(name, "lock_timeout")
		telemetry.ObserveChatRequest(name, model, "504")
		s.record(name, model, len(a.Message), 0, time.Since(start), 504)
		return fmt.Sprintf("provider busy: %s did not release its lock in time, try again later", name), true, nil
	}
	telemetry.ObserveLockWait(name, time.Since(start))

	sendStart := time.Now()
	resp, err := p.SendMessage(a.Message, threadID)
	mu.Unlock()
	telemetry.ObserveResponseDuration(name, false, time.Since(sendStart))
	if err != nil {
		s.log.Errorf("MCP chat via %s failed: %v", name, err)
		telemetry.ObserveProviderError(name, "send_error")
		telemetry.ObserveChatRequest(name, model, "500")
		s.record(name, model, len(a.Message), 0, time.Since(sendStart), 500)
		return fmt.Sprintf("provider error: %v", err), true, nil
	}

	telemetry.RecordSuccess(name)
	telemetry.ObserveChatRequest(name, model, "200")
	s.record(name, model, len(a.Message), len(resp.Message), time.Since(sendStart), 200)
	s.saveSessionURL(a.SessionID, p)

	text := resp.Message
	if line := quotaLine(name, p); line != "" {
		text += line
	}
	return text, false, nil
}

// toolRefresh starts a fresh upstream conversation under the provider lock,
// mirroring api.Server.handleRefresh.
func (s *server) toolRefresh(args json.RawMessage) (string, bool, *RPCError) {
	var a struct {
		Provider string `json:"provider"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return "", false, &RPCError{Code: codeInvalidParams, Message: "invalid params for refresh: " + err.Error()}
		}
	}
	p, mu, name := s.providerFor(a.Provider)
	if p == nil {
		return fmt.Sprintf("no provider available for %q (registered: %s)", a.Provider, strings.Join(s.providerNames(), ", ")), true, nil
	}

	start := time.Now()
	if !s.acquire(mu) {
		telemetry.ObserveLockWait(name, time.Since(start))
		telemetry.ObserveProviderError(name, "lock_timeout")
		return fmt.Sprintf("provider busy: %s did not release its lock in time, try again later", name), true, nil
	}
	telemetry.ObserveLockWait(name, time.Since(start))
	err := p.NewChat()
	mu.Unlock()
	if err != nil {
		telemetry.ObserveProviderError(name, "refresh_error")
		return fmt.Sprintf("refresh failed for %s: %v", name, err), true, nil
	}
	s.log.Infof("MCP refresh for %s done in %v", name, time.Since(start))
	return fmt.Sprintf("new chat started for %s (%s)", name, p.ModelID()), false, nil
}

// providerEntry is one row of the providers tool. logged_in appears only when
// a login check has been recorded (cached state) — the tool never probes a
// browser or a vendor API inside a tool call.
type providerEntry struct {
	Name        string `json:"name"`
	ModelID     string `json:"model_id"`
	Browserless bool   `json:"browserless"`
	LoggedIn    *bool  `json:"logged_in,omitempty"`
}

// toolProviders lists registered providers from in-memory state plus the
// telemetry login cache. No IsLoggedIn call happens here: browserless
// transports would spend a network round trip and DOM transports would touch
// the browser, and neither belongs inside a tool call (spec 001).
func (s *server) toolProviders(json.RawMessage) (string, bool, *RPCError) {
	names := s.providerNames()
	modelsByName := make(map[string]string, len(names))
	for _, n := range names {
		modelsByName[n] = s.deps.Providers[n].ModelID()
	}
	checks := map[string]telemetry.ProviderHealth{}
	for _, h := range telemetry.Snapshot(modelsByName) {
		checks[h.Provider] = h
	}

	entries := make([]providerEntry, 0, len(names))
	for _, n := range names {
		p := s.deps.Providers[n]
		e := providerEntry{
			Name:        n,
			ModelID:     p.ModelID(),
			Browserless: browserless(p),
		}
		if h, ok := checks[n]; ok && h.LastCheck != nil {
			li := h.LoggedIn
			e.LoggedIn = &li
		}
		entries = append(entries, e)
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return "internal error: providers list encoding failed: " + err.Error(), true, nil
	}
	return string(raw), false, nil
}

// ── shared plumbing (mirrors internal/api) ────────────────────

// providerFor resolves a provider name or model id, mirroring
// api.Server.providerForModel: exact ModelID match, then name containment,
// then the default chain. An explicit ref that matches nothing returns nil —
// silently defaulting would send a message to the wrong vendor — while an
// empty ref resolves through the default chain (cfg.Provider, the single
// provider, chatgpt, then the sorted-first name).
func (s *server) providerFor(ref string) (providers.Provider, *sync.Mutex, string) {
	provs := s.deps.Providers
	if len(provs) == 0 {
		return nil, s.fallbackMu(), ""
	}
	names := s.providerNames()

	ref = strings.TrimSpace(ref)
	if ref != "" {
		for _, n := range names {
			if provs[n].ModelID() == ref {
				return provs[n], s.lockFor(n), n
			}
		}
		lower := strings.ToLower(ref)
		for _, n := range names {
			if strings.Contains(lower, n) {
				return provs[n], s.lockFor(n), n
			}
		}
		return nil, s.fallbackMu(), ""
	}

	// Default chain.
	if s.deps.Cfg != nil {
		if p, ok := provs[s.deps.Cfg.Provider]; ok {
			return p, s.lockFor(s.deps.Cfg.Provider), s.deps.Cfg.Provider
		}
	}
	if len(provs) == 1 {
		for _, n := range names {
			return provs[n], s.lockFor(n), n
		}
	}
	if p, ok := provs[config.ProviderChatGPT]; ok {
		return p, s.lockFor(config.ProviderChatGPT), config.ProviderChatGPT
	}
	n := names[0]
	return provs[n], s.lockFor(n), n
}

func (s *server) providerNames() []string {
	names := make([]string, 0, len(s.deps.Providers))
	for k := range s.deps.Providers {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// lockFor returns the per-provider mutex (from the browser pool in pooled
// mode), falling back to a server-level mutex like api.Server does.
func (s *server) lockFor(name string) *sync.Mutex {
	if mu, ok := s.deps.Mus[name]; ok && mu != nil {
		return mu
	}
	return s.fallbackMu()
}

func (s *server) fallbackMu() *sync.Mutex { return &s.defaultMu }

// acquire mirrors api.acquireProviderLock: bounded TryLock polling so a hung
// browser cannot hang MCP forever. The budget matches doProviderTurn:
// ResponseTimeout + 30s.
func (s *server) acquire(mu *sync.Mutex) bool {
	if mu == nil {
		return true
	}
	timeout := 30 * time.Second
	if s.deps.Cfg != nil && s.deps.Cfg.ResponseTimeout > 0 {
		timeout = s.deps.Cfg.ResponseTimeout + 30*time.Second
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if mu.TryLock() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// saveSessionURL mirrors api.Server.saveSessionURL: record the provider's
// current conversation URL so the session manager can restore the thread's
// page later. No-op without a session id or manager.
func (s *server) saveSessionURL(sessionID string, p providers.Provider) {
	if sessionID == "" || s.deps.SessionMgr == nil || p == nil {
		return
	}
	type urlGetter interface{ CurrentURL() string }
	if g, ok := p.(urlGetter); ok {
		if u := g.CurrentURL(); u != "" {
			s.deps.SessionMgr.SaveURL(sessionID, u)
		}
	}
}

// quotaLine renders a cached quota hint (spec 020) for providers that
// implement providers.QuotaReporter. Cached only — implementations promise
// the last known snapshot, never a live call.
func quotaLine(name string, p providers.Provider) string {
	qr, ok := p.(providers.QuotaReporter)
	if !ok {
		return ""
	}
	q := telemetry.QuotaFromMap(qr.Quota())
	if q == nil {
		return ""
	}
	if q.Total != nil {
		line := fmt.Sprintf("\n\nquota (%s): %d/%d remaining", name, q.Remaining, *q.Total)
		if q.Window != "" {
			line += " (" + q.Window + ")"
		}
		return line
	}
	return fmt.Sprintf("\n\nquota (%s): %d remaining", name, q.Remaining)
}

// record stores one metered attempt keyed by the MCP client name from
// initialize (tenant "mcp:<client>", fallback "mcp:client"), mirroring
// api.Server.recordUsage. No-op when metering is disabled.
func (s *server) record(provider, model string, promptChars, completionChars int, latency time.Duration, code int) {
	if s.deps.Meter == nil {
		return
	}
	if err := s.deps.Meter.Record(meter.Record{
		TS:              time.Now(),
		Tenant:          s.tenant(),
		Provider:        provider,
		Model:           model,
		Streaming:       false,
		PromptChars:     promptChars,
		CompletionChars: completionChars,
		LatencyMs:       latency.Milliseconds(),
		Code:            code,
	}); err != nil {
		s.log.Errorf("Usage record failed for tenant %q: %v", s.tenant(), err)
	}
}

// tenant keys metering rows to the MCP client: clientInfo.name from
// initialize, or the fallback when the client never identified itself.
func (s *server) tenant() string {
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client != "" {
		return "mcp:" + client
	}
	return "mcp:client"
}

// browserless reports the transport shape; providers predating the interface
// (plain DOM) are browser-backed by definition.
func browserless(p providers.Provider) bool {
	if b, ok := p.(providers.Browserless); ok {
		return b.Browserless()
	}
	return false
}

// SeedLoginStates probes each already-initialized provider's login state
// once, in parallel with an overall budget, and caches the result in
// telemetry — the same mechanism /v1/health/providers uses. Call it once at
// startup (after Init, before Serve): the `providers` tool then reports this
// cached state and never probes a browser or vendor API inside a tool call.
func (d *Deps) SeedLoginStates() {
	if d == nil || len(d.Providers) == 0 {
		return
	}
	log := loggerFor(d)
	names := make([]string, 0, len(d.Providers))
	for n := range d.Providers {
		names = append(names, n)
	}
	sort.Strings(names)

	type result struct {
		name string
		ok   bool
		err  error
	}
	results := make(chan result, len(names))
	for _, n := range names {
		go func(n string) {
			p := d.Providers[n]
			if p == nil {
				results <- result{name: n, err: fmt.Errorf("provider not initialized")}
				return
			}
			ok, err := p.IsLoggedIn()
			results <- result{name: n, ok: ok, err: err}
		}(n)
	}
	deadline := time.After(15 * time.Second)
	for range names {
		select {
		case r := <-results:
			errStr := ""
			if r.err != nil {
				errStr = r.err.Error()
			}
			telemetry.RecordLoginCheck(r.name, r.ok, errStr)
			log.Infof("Login check for %s: logged_in=%v", r.name, r.ok)
		case <-deadline:
			log.Warnf("Login-state seed timed out; providers tool will report cached state only")
			return
		}
	}
}
