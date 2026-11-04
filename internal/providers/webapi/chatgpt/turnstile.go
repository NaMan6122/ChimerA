// Turnstile token solver for chatgpt.com.
//
// The token is NOT minted by Cloudflare: the server sends turnstile.dx, a base64
// blob XOR-obfuscated with the `p` token from the same request, which decodes to
// a small bytecode program this file interprets. Buchodi's decryption (Mar 2026,
// 377 programs) established the chain; this is a port of the reference
// interpreter (pi-gpt src/turnstile.ts, MIT, itself from lanqian528/chat2api).
//
//	outer = XOR(base64decode(dx), p)   -> JSON: [[op, ...args], ...]
//	run the opcodes                   -> a fingerprint JSON string
//	token  = base64(fingerprint)      -> OpenAI-Sentinel-Turnstile-Token
//
// STATUS (spec 012 §3.4): the turn succeeds WITHOUT this token for a session
// holding a browser-earned cookie jar, so it is advisory today. It is
// implemented because a future OpenAI change could make it mandatory, and
// because the `so` (Signal Orchestrator) block uses the same mechanism.
package chatgptweb

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"time"
)

// orderedMap mirrors the reference's OrderedMap: insertion-ordered key/value,
// used as the Reflect.set target and JSON.stringified later.
type orderedMap struct {
	keys   []string
	values map[string]any
}

func newOrderedMap() *orderedMap { return &orderedMap{values: map[string]any{}} }

func (o *orderedMap) add(k string, v any) {
	if _, ok := o.values[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.values[k] = v
}

// toStr mirrors the reference toStr(): numbers stringify bare, and a handful of
// window.* probes map to their native-code descriptions.
func toStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "undefined"
	case string:
		if s, ok := nativeProbe[x]; ok {
			return s
		}
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case []any:
		allStr := true
		parts := make([]string, 0, len(x))
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				allStr = false
				break
			}
			parts = append(parts, s)
		}
		if allStr {
			return strings.Join(parts, ",")
		}
		return fmt.Sprint(x)
	case []string:
		return strings.Join(x, ",")
	default:
		return fmt.Sprint(v)
	}
}

var nativeProbe = map[string]string{
	"window.Math":            "[object Math]",
	"window.Reflect":         "[object Reflect]",
	"window.performance":     "[object Performance]",
	"window.localStorage":    "[object Storage]",
	"window.Object":          "function Object() { [native code] }",
	"window.Reflect.set":     "function set() { [native code] }",
	"window.performance.now": "function () { [native code] }",
	"window.Object.create":   "function create() { [native code] }",
	"window.Object.keys":     "function keys() { [native code] }",
	"window.Math.random":     "function random() { [native code] }",
}

// xorStr XORs two strings char-wise with b repeated, for tests and the
// synthetic dx builder.
func xorStr(a, b string) string {
	return string(xorBytes([]byte(a), b))
}

// xorBytes XORs raw BYTES with the key repeated. This must not operate on runes:
// the base64-decoded dx is binary, and decoding it as UTF-8 replaces invalid
// sequences with U+FFFD, corrupting the bytecode. Buchodi's decryption is
// explicitly byte-wise: base64decode(dx)[i] ^ p[i % len(p)].
func xorBytes(a []byte, key string) []byte {
	if len(key) == 0 {
		return a
	}
	kb := []byte(key)
	out := make([]byte, len(a))
	for i := range a {
		out[i] = a[i] ^ kb[i%len(kb)]
	}
	return out
}

// localStorageKeys is the fixed key list the reference returns for
// window.Object.keys(window.localStorage).
var localStorageKeys = []any{
	"STATSIG_LOCAL_STORAGE_INTERNAL_STORE_V4",
	"STATSIG_LOCAL_STORAGE_STABLE_ID",
	"client-correlated-secret",
	"oai/apps/capExpiresAt",
	"oai-did",
	"STATSIG_LOCAL_STORAGE_LOGGING_REQUEST",
	"UiState.isNavigationCollapsed.1",
}

// buildFuncMap constructs the opcode table. Ops are addressed by integer id;
// `res` receives the final token via op 3.
func buildFuncMap(res *string, tokenList []any, p string, start time.Time) map[int]any {
	pm := map[int]any{}
	get := func(k int) any { return pm[k] }
	set := func(k int, v any) { pm[k] = v }

	// op 1 — XOR-assign
	pm[1] = func(args ...any) {
		e := argInt(args, 0)
		t := argInt(args, 1)
		set(e, xorStr(toStr(get(e)), toStr(get(t))))
	}
	// op 2 — set literal
	pm[2] = func(args ...any) {
		e := argInt(args, 0)
		set(e, argVal(args, 1))
	}
	// op 3 — RESOLVE: base64 of the value becomes the token.
	//
	// The reference passes the argument straight to Buffer.from(), implying a
	// literal; Buchodi's decrypted listing shows the final RESOLVE taking a
	// register address. Accept both: resolve through the register table when the
	// argument is numeric and holds a value, else treat it as a literal.
	pm[3] = func(args ...any) {
		a := argVal(args, 0)
		var s string
		if _, isStr := a.(string); isStr {
			s = toStr(a)
		} else if v, ok := pm[argInt(args, 0)]; ok && v != nil {
			s = toStr(v)
		} else {
			s = toStr(a)
		}
		*res = base64.StdEncoding.EncodeToString([]byte(s))
	}
	// op 5 — concat / append / add (two arguments: appends t onto e)
	pm[5] = func(args ...any) {
		e, t := argInt(args, 0), argInt(args, 1)
		n, tres := get(e), get(t)
		switch cur := n.(type) {
		case []any:
			set(e, append(append([]any{}, cur...), tres))
		default:
			if isNum(n) && isNum(tres) {
				set(e, toFloat(n)+toFloat(tres))
			} else if _, ok := n.(string); ok {
				set(e, toStr(n)+toStr(tres))
			} else if _, ok := tres.(string); ok {
				set(e, toStr(n)+toStr(tres))
			} else {
				set(e, "NaN")
			}
		}
	}
	// op 6 — property join, with the document.location special case
	pm[6] = func(args ...any) {
		e, t, n := argInt(args, 0), argInt(args, 1), argInt(args, 2)
		tv, nv := toStr(get(t)), toStr(get(n))
		out := tv + "." + nv
		if out == "window.document.location" {
			out = "https://chatgpt.com/"
		}
		set(e, out)
	}
	// op 7 — Reflect.set
	pm[7] = func(args ...any) {
		e := argInt(args, 0)
		vals := make([]any, 0, len(args)-1)
		for _, a := range args[1:] {
			vals = append(vals, get(argInt([]any{a}, 0)))
		}
		ev := get(e)
		if s, ok := ev.(string); ok && s == "window.Reflect.set" {
			if om, ok := vals[0].(*orderedMap); ok {
				om.add(toStr(vals[1]), vals[2])
			}
			return
		}
		if f, ok := ev.(func(...any)); ok {
			f(vals...)
		}
	}
	// op 8 — copy
	pm[8] = func(args ...any) { set(argInt(args, 0), get(argInt(args, 1))) }
	// op 10 — the window object
	pm[10] = "window"
	// op 14 — JSON.parse
	pm[14] = func(args ...any) {
		e, t := argInt(args, 0), argInt(args, 1)
		if s, ok := get(t).(string); ok {
			var doc any
			if json.Unmarshal([]byte(s), &doc) == nil {
				set(e, doc)
			}
		}
	}
	// op 15 — JSON.stringify
	pm[15] = func(args ...any) {
		e, t := argInt(args, 0), argInt(args, 1)
		b, err := json.Marshal(get(t))
		if err == nil {
			set(e, string(b))
		}
	}
	// op 17 — CALL on a resolved native
	pm[17] = func(args ...any) {
		e, t := argInt(args, 0), argInt(args, 1)
		callArgs := make([]any, 0, len(args)-2)
		for _, a := range args[2:] {
			callArgs = append(callArgs, get(argInt([]any{a}, 0)))
		}
		tv := get(t)
		switch s := tv.(type) {
		case string:
			switch s {
			case "window.performance.now":
				elapsed := float64(time.Since(start).Nanoseconds()) / 1e6
				set(e, (elapsed*1e6+rand.Float64())/1e6)
			case "window.Object.create":
				set(e, newOrderedMap())
			case "window.Object.keys":
				if len(callArgs) > 0 && toStr(callArgs[0]) == "window.localStorage" {
					set(e, localStorageKeys)
				}
			case "window.Math.random":
				set(e, rand.Float64())
			}
		case func(...any):
			s(callArgs...)
		}
	}
	// op 18 — base64 decode
	pm[18] = func(args ...any) {
		e := argInt(args, 0)
		if raw, err := base64.StdEncoding.DecodeString(toStr(get(e))); err == nil {
			set(e, string(raw))
		}
	}
	// op 19 — base64 encode
	pm[19] = func(args ...any) {
		e := argInt(args, 0)
		set(e, base64.StdEncoding.EncodeToString([]byte(toStr(get(e)))))
	}
	// op 20 — conditional call
	pm[20] = func(args ...any) {
		e, t, n := argInt(args, 0), argInt(args, 1), argInt(args, 2)
		if toStr(get(e)) == toStr(get(t)) {
			if f, ok := get(n).(func(...any)); ok {
				vals := make([]any, 0, len(args)-3)
				for _, a := range args[3:] {
					vals = append(vals, get(argInt([]any{a}, 0)))
				}
				f(vals...)
			}
		}
	}
	// op 21 — no-op
	pm[21] = func(args ...any) {}
	// op 23 — call if target is a function
	pm[23] = func(args ...any) {
		e, t := argInt(args, 0), argInt(args, 1)
		if get(e) != nil {
			if f, ok := get(t).(func(...any)); ok {
				vals := make([]any, 0, len(args)-2)
				for _, a := range args[2:] {
					vals = append(vals, get(argInt([]any{a}, 0)))
				}
				f(vals...)
			}
		}
	}
	// op 24 — plain property join
	pm[24] = func(args ...any) {
		e, t, n := argInt(args, 0), argInt(args, 1), argInt(args, 2)
		set(e, toStr(get(t))+"."+toStr(get(n)))
	}
	return pm
}

// SolveTurnstile produces the OpenAI-Sentinel-Turnstile-Token from dx and a key.
func SolveTurnstile(dx, key string) (string, error) {
	tok, _, err := solveTurnstileAny(dx, key)
	return tok, err
}

// solveTurnstileAny tries each candidate XOR key and returns the first that
// decodes to valid bytecode, along with the key that worked. The reference uses
// the proof token when present and `p` otherwise (pi-gpt: `out.proof || p`);
// which one is correct is not documented, so both are attempted rather than
// guessed.
func solveTurnstileAny(dx string, keys ...string) (token, usedKey string, err error) {
	decoded, err := base64.StdEncoding.DecodeString(dx)
	if err != nil {
		// dx is sometimes unpadded.
		decoded, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(dx, "="))
		if err != nil {
			return "", "", fmt.Errorf("dx is not base64: %w", err)
		}
	}
	var lastErr error
	for _, key := range keys {
		if key == "" {
			continue
		}
		plain := xorBytes(decoded, key)
		var tokenList []any
		if err := json.Unmarshal(plain, &tokenList); err != nil {
			lastErr = fmt.Errorf("key(len=%d): %w; first 60 bytes: %q",
				len(key), err, truncate(string(plain), 60))
			continue
		}
		tok, err := runTurnstileVM(tokenList, key)
		if err != nil {
			lastErr = err
			continue
		}
		return tok, key, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no candidate key supplied")
	}
	return "", "", fmt.Errorf("no key decoded the turnstile blob: %w", lastErr)
}

// runTurnstileVM interprets a decoded bytecode program.
func runTurnstileVM(tokenList []any, p string) (string, error) {
	var res string
	start := time.Now()
	pm := buildFuncMap(&res, tokenList, p, start)
	pm[9] = tokenList
	pm[16] = p

	executed := 0
	for _, tok := range tokenList {
		arr, ok := tok.([]any)
		if !ok || len(arr) == 0 {
			continue
		}
		op := int(toFloat(arr[0]))
		f, ok := pm[op].(func(...any))
		if !ok {
			continue
		}
		func() {
			defer func() { _ = recover() }() // one malformed instruction must not abort the run
			f(arr[1:]...)
		}()
		executed++
	}
	if res == "" {
		return "", fmt.Errorf("VM produced no token after %d/%d instructions", executed, len(tokenList))
	}
	return res, nil
}

func argInt(args []any, i int) int {
	if i >= len(args) {
		return -1
	}
	return int(toFloat(args[i]))
}

func argVal(args []any, i int) any {
	if i >= len(args) {
		return nil
	}
	return args[i]
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	}
	return math.NaN()
}

func isNum(v any) bool {
	switch v.(type) {
	case float64, int:
		return true
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
