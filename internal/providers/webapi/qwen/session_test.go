package qwenweb

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fakeJWT(exp int64) string {
	enc := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	header := enc(map[string]string{"alg": "HS256", "typ": "JWT"})
	payload := enc(map[string]any{"exp": exp, "id": "user"})
	return header + "." + payload + ".signature"
}

func TestTokenExpiry(t *testing.T) {
	exp := time.Now().Add(30 * 24 * time.Hour).Unix()
	got, ok := tokenExpiry(fakeJWT(exp))
	if !ok || got.Unix() != exp {
		t.Fatalf("tokenExpiry = %v, %v; want %d", got, ok, exp)
	}
	if _, ok := tokenExpiry("opaque-token"); ok {
		t.Fatal("opaque token must have no expiry")
	}
	if _, ok := tokenExpiry("a.b.c"); ok {
		t.Fatal("garbage payload must have no expiry")
	}
}

func TestInstallSession(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.json")
	token := fakeJWT(time.Now().Add(time.Hour).Unix())
	raw, _ := json.Marshal(map[string]any{
		"access_token": token,
		"cookies":      map[string]string{"acw_tc": "abc", "token": token},
	})
	if err := os.WriteFile(src, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "auth", "qwen.json")
	sess, err := InstallSession(src, dst)
	if err != nil {
		t.Fatalf("InstallSession: %v", err)
	}
	if len(sess.Cookies) != 2 {
		t.Fatalf("cookies = %v", sess.Cookies)
	}
	if exp, ok := sess.ExpiresAt(); !ok || time.Until(exp) < 30*time.Minute {
		t.Fatalf("expiry = %v, %v", exp, ok)
	}

	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("installed mode = %v, want 0600", perm)
	}
	if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind: %v", err)
	}

	st := sess.Status(dst)
	if !st.HasExpiry || st.DaysRemain <= 0 {
		t.Fatalf("status = %+v", st)
	}
}

func TestInstallSessionRejectsIncomplete(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"no-token":   `{"access_token":"","cookies":{"a":"b"}}`,
		"no-cookies": `{"access_token":"` + fakeJWT(time.Now().Add(time.Hour).Unix()) + `","cookies":{}}`,
	}
	for name, body := range cases {
		src := filepath.Join(dir, name+".json")
		if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := InstallSession(src, filepath.Join(dir, "out", name+".json")); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}
