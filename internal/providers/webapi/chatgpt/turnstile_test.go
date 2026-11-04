package chatgptweb

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// encodeDx is the inverse of the solver's first step: JSON -> XOR with p ->
// base64. It lets a synthetic program round-trip without a live session.
func encodeDx(t *testing.T, program []any, p string) string {
	t.Helper()
	raw, err := json.Marshal(program)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString([]byte(xorStr(string(raw), p)))
}

// TestTurnstileRoundTrip exercises the whole path: XOR decode, JSON parse,
// opcode dispatch, and token capture via op 3.
func TestTurnstileRoundTrip(t *testing.T) {
	p := "gAAAAACsomeRequirementsToken"
	fingerprint := `{"navigator":{"webdriver":false},"screen":[1920,1080]}`
	program := []any{
		[]any{2, 5, fingerprint}, // op 2: set register 5
		[]any{3, fingerprint},    // op 3: resolve -> base64(value)
	}
	dx := encodeDx(t, program, p)

	got, err := SolveTurnstile(dx, p)
	if err != nil {
		t.Fatalf("SolveTurnstile: %v", err)
	}
	want := base64.StdEncoding.EncodeToString([]byte(fingerprint))
	if got != want {
		t.Errorf("token = %q, want %q", got, want)
	}
}

// TestTurnstileConcatAndResolve exercises ops 2 (set), 5 (concat) and 3
// (resolve), which is how the real program assembles the fingerprint.
//
// Data registers must not collide with opcode slots: in the real VM a register
// and an opcode share the same table, so writing data to register 3 would erase
// the RESOLVE opcode. The reference programs use high register numbers for this
// reason, and so does this test.
func TestTurnstileConcatAndResolve(t *testing.T) {
	p := "key"
	// op 5 is two-argument: it appends the value at t onto the value at e.
	program := []any{
		[]any{2, 40, "ab"}, // reg40 = "ab"
		[]any{2, 41, "cd"}, // reg41 = "cd"
		[]any{5, 40, 41},   // reg40 = "ab"+"cd" = "abcd"
		[]any{3, 40},       // resolve -> base64(reg40)
	}
	dx := encodeDx(t, program, p)
	got, err := SolveTurnstile(dx, p)
	if err != nil {
		t.Fatalf("SolveTurnstile: %v", err)
	}
	if want := base64.StdEncoding.EncodeToString([]byte("abcd")); got != want {
		t.Errorf("token = %q, want %q", got, want)
	}
}

// TestTurnstileWrongKeyFails confirms a wrong key yields no token rather than a
// bogus one — the failure mode that matters when diagnosing a 403.
func TestTurnstileWrongKeyFails(t *testing.T) {
	program := []any{[]any{3, "secret"}}
	dx := encodeDx(t, program, "right-key")
	if _, err := SolveTurnstile(dx, "wrong-key"); err == nil {
		t.Error("expected failure with the wrong XOR key, got a token")
	}
}

// TestTurnstileRejectsNonVM checks a non-bytecode dx is reported clearly.
func TestTurnstileRejectsNonVM(t *testing.T) {
	dx := base64.StdEncoding.EncodeToString([]byte(xorStr("not json at all", "p")))
	if _, err := SolveTurnstile(dx, "p"); err == nil {
		t.Error("expected an error for a non-JSON payload")
	}
}

// TestXorBytesIsByteWise pins the XOR to raw bytes. Operating on runes instead
// would replace invalid UTF-8 with U+FFFD and corrupt the bytecode — the bug
// that made the first live attempt fail to decode.
func TestXorBytesIsByteWise(t *testing.T) {
	// 0x80 is invalid as a standalone UTF-8 byte; a rune-wise XOR would mangle it.
	in := []byte{0x80, 0x81, 0x82}
	out := xorBytes(in, "k")
	if len(out) != len(in) {
		t.Fatalf("xorBytes changed the length: %d -> %d", len(in), len(out))
	}
	for i := range in {
		if out[i] != in[i]^'k' {
			t.Errorf("byte %d: got %02x, want %02x", i, out[i], in[i]^'k')
		}
	}
}

// TestCapturedDxDecodesXor verifies the one thing checkable without the missing
// `p`: that the real captured dx is base64 of high-entropy bytes, not plaintext
// JSON. Our capture truncated dx at the 20k body cap, so this only asserts the
// encoding shape. The sample is trimmed to a whole base64 quantum (multiple of
// 4 chars) since the truncation does not respect one.
func TestCapturedDxDecodesXor(t *testing.T) {
	const dxHead = "PBp5bWF4c3lMcRxvbAxfaTdrAXRlenh6bnNJdFJlR39dHFVuMnhHY1xXHW5jWlZ1YG5oZQt9UhZ9ZVR7e21PYmMbXFR7ZVYIH3IzW118XltBcV9jaV"
	raw, err := base64.StdEncoding.DecodeString(dxHead[:len(dxHead)/4*4])
	if err != nil {
		t.Fatalf("captured dx is not base64: %v", err)
	}
	// It must NOT already be JSON — if it were, the XOR step would be wrong.
	if len(raw) > 0 && raw[0] == '[' {
		t.Error("captured dx decodes straight to JSON; the XOR step would be unnecessary")
	}
	t.Logf("captured dx: %d bytes, first: %q", len(raw), truncate(string(raw), 40))
}
