package auth

import (
	"net/http/httptest"
	"testing"
)

func TestSingleKey(t *testing.T) {
	k, err := New("sek", "")
	if err != nil {
		t.Fatal(err)
	}
	if k.IsOpen() {
		t.Fatal("should not be open")
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer sek")
	tenant, ok := k.Authenticate(r)
	if !ok || tenant != DefaultTenant {
		t.Fatalf("got %q,%v want default,true", tenant, ok)
	}
	r2 := httptest.NewRequest("GET", "/", nil)
	if _, ok := k.Authenticate(r2); ok {
		t.Fatal("missing token should fail")
	}
}

func TestMultiKeyAndExtraction(t *testing.T) {
	k, err := New("sek", "acme=a1, basic=b2")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ header, value, tenant string }{
		{"Authorization", "Bearer a1", "acme"},
		{"X-Api-Key", "b2", "basic"},
		{"X-Api-Key", "Bearer b2", "basic"},
		{"Anthropic-Api-Key", "Bearer sek", "default"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set(c.header, c.value)
		tenant, ok := k.Authenticate(r)
		if !ok || tenant != c.tenant {
			t.Fatalf("%s=%q got %q,%v want %q,true", c.header, c.value, tenant, ok, c.tenant)
		}
	}
}

func TestMalformed(t *testing.T) {
	for _, extra := range []string{"acme", "=tok", "acme="} {
		if _, err := New("", extra); err == nil {
			t.Fatalf("extra=%q should error", extra)
		}
	}
	if _, err := New("x", "ok=1,ok=1"); err == nil {
		t.Fatal("duplicate token should error")
	}
}

func TestOpen(t *testing.T) {
	k, err := New("", "")
	if err != nil {
		t.Fatal(err)
	}
	if !k.IsOpen() {
		t.Fatal("should be open")
	}
	r := httptest.NewRequest("GET", "/", nil)
	if _, ok := k.Authenticate(r); !ok {
		t.Fatal("open registry should allow all")
	}
}
