// quota.go — the quota surface of /v1/health/providers.
//
// Providers that implement the optional providers.QuotaReporter interface
// hand the health handler a plain map; QuotaFromMap normalizes it into the
// wire shape so handler and providers never share a struct. Numbers arrive
// as int from in-process providers and as float64 after any JSON round
// trip; both are accepted. Anything unusable yields nil, which omits the
// field entirely instead of serving garbage.
package telemetry

// Quota is one provider's usage quota as served by /v1/health/providers.
// Remaining is always present once the field exists; Total and Window are
// omitted when the provider does not know them (grok's endpoint reports
// remainingQueries; totalQueries/window have no client-side source yet —
// the fields exist so richer providers need no schema change).
type Quota struct {
	Remaining int    `json:"remaining"`
	Total     *int   `json:"total,omitempty"`
	Window    string `json:"window,omitempty"`
}

// QuotaFromMap converts a QuotaReporter's snapshot map into the wire shape.
// A nil result means "no quota to report".
func QuotaFromMap(m map[string]any) *Quota {
	if len(m) == 0 {
		return nil
	}
	remaining, ok := quotaInt(m["remaining"])
	if !ok {
		return nil
	}
	q := &Quota{Remaining: remaining}
	if total, ok := quotaInt(m["total"]); ok {
		q.Total = &total
	}
	if w, ok := m["window"].(string); ok {
		q.Window = w
	}
	return q
}

// quotaInt coerces the numeric types a cached quota realistically carries.
func quotaInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case float64:
		if n != n { // NaN
			return 0, false
		}
		return int(n), true
	}
	return 0, false
}
