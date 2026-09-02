// Package session implements X-Session-Id continuity with LRU tab eviction.
// Mirrors reference PersistentSessionManager but for Chimera's single-Chromium pool.
package session

import (
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/chimera/chimera/internal/logging"
)

var log = logging.New("session", "./logs", "debug", true)

// Manager keeps session_id → conversation URL + Page, with LRU eviction.
type Manager struct {
	mu              sync.Mutex
	browser         *rod.Browser
	pages           map[string]*rod.Page        // session_id → Page
	urls            map[string]string           // session_id → /c/<id> URL
	lru             map[string]time.Time        // session_id → last used
	locks           map[string]*sync.Mutex      // per-session lock
	maxTabs         int
}

func New(browser *rod.Browser, maxTabs int) *Manager {
	if maxTabs <= 0 {
		maxTabs = 10
	}
	return &Manager{
		browser: browser,
		pages:   make(map[string]*rod.Page),
		urls:    make(map[string]string),
		lru:     make(map[string]time.Time),
		locks:   make(map[string]*sync.Mutex),
		maxTabs: maxTabs,
	}
}

func (m *Manager) SetBrowser(b *rod.Browser) { m.mu.Lock(); defer m.mu.Unlock(); m.browser = b }

func (m *Manager) lockFor(id string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.locks[id]; !ok {
		m.locks[id] = &sync.Mutex{}
	}
	return m.locks[id]
}

// Touch updates LRU timestamp.
func (m *Manager) touch(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lru[id] = time.Now()
}

// SaveURL persists the conversation URL after a turn.
func (m *Manager) SaveURL(id, url string) {
	if id == "" || url == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.Contains(url, "/c/") || strings.Contains(url, "/a/chat/") || strings.Contains(url, "/chat/") {
		m.urls[id] = url
	}
}

// Acquire returns the Page for session_id, creating or restoring as needed.
func (m *Manager) Acquire(id string) (*rod.Page, error) {
	m.mu.Lock()
	if _, exists := m.pages[id]; !exists {
		open := 0
		for _, p := range m.pages {
			if p != nil {
				if _, err := p.Eval(`() => 1`); err == nil {
					open++
				}
			}
		}
		if open >= m.maxTabs {
			var oldestID string
			var oldestTime time.Time
			first := true
			for sid, t := range m.lru {
				if _, ok := m.pages[sid]; !ok {
					continue
				}
				if mu, ok := m.locks[sid]; ok {
					if !mu.TryLock() {
						continue
					}
					mu.Unlock()
				}
				if first || t.Before(oldestTime) {
					oldestID = sid
					oldestTime = t
					first = false
				}
			}
			if oldestID != "" {
				if p, ok := m.pages[oldestID]; ok && p != nil {
					_ = p.Close()
					log.Infof("Session LRU evicted %s", oldestID)
				}
				delete(m.pages, oldestID)
			}
		}
	}
	if p, ok := m.pages[id]; ok && p != nil {
		if _, err := p.Eval(`() => 1`); err == nil {
			m.mu.Unlock()
			m.touch(id)
			return p, nil
		}
		delete(m.pages, id)
	}
	m.mu.Unlock()

	if m.browser == nil {
		return nil, nil
	}
	p, err := m.browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	savedURL := m.urls[id]
	m.mu.Unlock()
	if savedURL != "" {
		_ = p.Timeout(15 * time.Second).MustNavigate(savedURL)
		_ = p.WaitLoad()
	}
	m.mu.Lock()
	m.pages[id] = p
	m.lru[id] = time.Now()
	m.mu.Unlock()
	return p, nil
}
