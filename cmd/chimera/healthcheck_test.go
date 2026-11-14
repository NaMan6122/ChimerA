package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProbeLocalHealth(t *testing.T) {
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer okSrv.Close()
	if code := probeLocalHealth(okSrv.URL, time.Second); code != 0 {
		t.Fatalf("expected exit 0 on 200 OK, got %d", code)
	}

	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer badSrv.Close()
	if code := probeLocalHealth(badSrv.URL, time.Second); code != 1 {
		t.Fatalf("expected exit 1 on 503, got %d", code)
	}

	if code := probeLocalHealth("http://127.0.0.1:1/health", 100*time.Millisecond); code != 1 {
		t.Fatalf("expected exit 1 on unreachable endpoint, got %d", code)
	}
}
