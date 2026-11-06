package telemetry

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestQuotaFromMapFull(t *testing.T) {
	q := QuotaFromMap(map[string]any{"remaining": 21, "total": 30, "window": "5h"})
	if q == nil {
		t.Fatal("expected a Quota, got nil")
	}
	if q.Remaining != 21 || q.Total == nil || *q.Total != 30 || q.Window != "5h" {
		t.Fatalf("unexpected quota: %+v", q)
	}
}

func TestQuotaFromMapRemainingOnly(t *testing.T) {
	q := QuotaFromMap(map[string]any{"remaining": 7})
	if q == nil || q.Remaining != 7 || q.Total != nil || q.Window != "" {
		t.Fatalf("unexpected quota: %+v", q)
	}
	raw, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); got != `{"remaining":7}` {
		t.Fatalf("omitempty broken, got %s", got)
	}
}

func TestQuotaFromMapFloat64AfterJSONRoundTrip(t *testing.T) {
	// Simulate the value having passed through a JSON decode: numbers become float64.
	raw := []byte(`{"remaining":21,"total":30}`)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	q := QuotaFromMap(m)
	if q == nil || q.Remaining != 21 || q.Total == nil || *q.Total != 30 {
		t.Fatalf("unexpected quota: %+v", q)
	}
}

func TestQuotaFromMapRejectsGarbage(t *testing.T) {
	for name, m := range map[string]map[string]any{
		"nil map":           nil,
		"empty":             {},
		"missing remaining": {"total": 30},
		"non numeric":       {"remaining": "many"},
		"bool":              {"remaining": true},
	} {
		if q := QuotaFromMap(m); q != nil {
			t.Fatalf("%s: expected nil, got %+v", name, q)
		}
	}
}

func TestQuotaJSONShape(t *testing.T) {
	raw, err := json.Marshal(map[string]*Quota{
		"grok": QuotaFromMap(map[string]any{"remaining": 21, "total": 30, "window": "5h"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"remaining":21`, `"total":30`, `"window":"5h"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("quota JSON missing %s: %s", want, raw)
		}
	}
}
