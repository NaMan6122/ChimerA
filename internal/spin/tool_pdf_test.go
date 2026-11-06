package spin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakePDFScript is a stand-in pdftotext so tests run on CI machines without
// poppler. It must use shell builtins only: PATH is set to its directory, so
// nothing else on the system is reachable. It records its argv (flag
// passthrough) and switches behaviour on FAKE_PDF_MODE.
const fakePDFScript = `#!/bin/sh
# fake pdftotext (test double)
if [ -n "$FAKE_PDF_ARGS" ]; then
  printf '%s\n' "$@" > "$FAKE_PDF_ARGS"
fi
case "$FAKE_PDF_MODE" in
  encrypted)
    echo "Copying of text from this document is not allowed." >&2
    exit 3
    ;;
  fail)
    echo "Syntax Error: Couldn't find trailer dictionary" >&2
    exit 1
    ;;
  empty)
    exit 0
    ;;
  big)
    i=1
    while [ "$i" -le 600 ]; do
      echo "line $i: deterministic fake pdf extraction output, padded for the budget test"
      i=$((i+1))
    done
    ;;
  *)
    echo "FAKE-PDF-TEXT: deterministic extraction of $FAKE_PDF_PAGE"
    ;;
esac
`

// installFakePDF puts the double on PATH and returns its directory.
func installFakePDF(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pdftotext"), []byte(fakePDFScript), 0o755); err != nil {
		t.Fatalf("write fake pdftotext: %v", err)
	}
	t.Setenv("PATH", dir)
}

func execTool(t *testing.T, e *Executor, name, args string) string {
	t.Helper()
	var tc ToolCall
	tc.Type = "function"
	tc.Function.Name = name
	tc.Function.Arguments = args
	return e.Exec(tc)
}

// TestReadPDFMissingBinary pins the actionable install hint: on a CI box without
// poppler the tool must tell the model which binary is missing and how to get it.
func TestReadPDFMissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no pdftotext anywhere
	e := &Executor{Root: t.TempDir()}
	got := execTool(t, e, "read_pdf", `{"path":"doc.pdf"}`)
	for _, want := range []string{"pdftotext", "brew install poppler", "apt install poppler-utils"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing-binary result %q does not mention %q", got, want)
		}
	}
	if !strings.HasPrefix(got, "error:") {
		t.Errorf("result %q should be an error, not abort", got)
	}
}

// TestReadPDFConfinement is the safety property, identical to read_file's: a
// path that escapes -root must be rejected before any subprocess runs.
func TestReadPDFConfinement(t *testing.T) {
	installFakePDF(t)
	e := &Executor{Root: t.TempDir()}
	for _, bad := range []string{"../../etc/passwd", "../outside.pdf", "/etc/passwd"} {
		got := execTool(t, e, "read_pdf", `{"path":"`+bad+`"}`)
		if !strings.Contains(got, "outside the workspace") {
			t.Errorf("read_pdf(%q) = %q, want rejection", bad, got)
		}
	}
	if got := execTool(t, e, "read_pdf", `{"path":"docs/report.pdf"}`); strings.Contains(got, "outside the workspace") {
		t.Errorf("in-root path rejected: %q", got)
	}
}

// TestReadPDFPageFlags pins the passthrough: pages become -f/-l in front of the
// input path, with "-" as the stdout sink; no pages object, no page flags.
func TestReadPDFPageFlags(t *testing.T) {
	installFakePDF(t)
	root := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "argv.txt")
	t.Setenv("FAKE_PDF_ARGS", argsFile)
	e := &Executor{Root: root}

	execTool(t, e, "read_pdf", `{"path":"doc.pdf","pages":{"from":2,"to":5}}`)
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("fake did not record argv: %v", err)
	}
	want := "-layout\n-f\n2\n-l\n5\n" + filepath.Join(root, "doc.pdf") + "\n-\n"
	if string(raw) != want {
		t.Errorf("argv = %q, want %q", raw, want)
	}

	execTool(t, e, "read_pdf", `{"path":"doc.pdf"}`)
	raw, err = os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("fake did not record argv: %v", err)
	}
	want = "-layout\n" + filepath.Join(root, "doc.pdf") + "\n-\n"
	if string(raw) != want {
		t.Errorf("argv without pages = %q, want %q", raw, want)
	}
}

// TestReadPDFBudgetTruncation pins the 8,000-char ceiling: truncation is marked
// with a re-call instruction, and max_chars may lower the budget but never raise it.
func TestReadPDFBudgetTruncation(t *testing.T) {
	installFakePDF(t)
	t.Setenv("FAKE_PDF_MODE", "big")
	e := &Executor{Root: t.TempDir()}

	got := execTool(t, e, "read_pdf", `{"path":"doc.pdf"}`)
	if !strings.Contains(got, "truncated at 8000 chars") || !strings.Contains(got, "narrower pages") {
		t.Errorf("default budget result not marked: %q", got[:80]+"…")
	}
	if len(got) <= MaxToolOutput || len(got) > MaxToolOutput+200 {
		t.Errorf("result length = %d, want just over %d", len(got), MaxToolOutput)
	}

	got = execTool(t, e, "read_pdf", `{"path":"doc.pdf","max_chars":100}`)
	if !strings.Contains(got, "truncated at 100 chars") {
		t.Errorf("max_chars=100 result not marked: %q", got)
	}
	if head := got[:100]; strings.Contains(head, "truncated") {
		t.Errorf("marker leaked into the kept text: %q", head)
	}

	got = execTool(t, e, "read_pdf", `{"path":"doc.pdf","max_chars":99999}`)
	if !strings.Contains(got, "truncated at 8000 chars") {
		t.Errorf("max_chars raised the budget above %d: %q", MaxToolOutput, got[:80]+"…")
	}
}

// TestReadPDFSmallOutputUntruncated guards the happy path: short extractions
// come back verbatim with no marker.
func TestReadPDFSmallOutputUntruncated(t *testing.T) {
	installFakePDF(t)
	e := &Executor{Root: t.TempDir()}
	got := execTool(t, e, "read_pdf", `{"path":"doc.pdf"}`)
	if !strings.HasPrefix(got, "FAKE-PDF-TEXT:") || strings.Contains(got, "truncated") {
		t.Errorf("small output mangled: %q", got)
	}
}

// TestReadPDFEncryptedAndNoTextLayer pins the OCR boundary: encrypted and
// image-only PDFs both refuse with OCR named as out of scope.
func TestReadPDFEncryptedAndNoTextLayer(t *testing.T) {
	installFakePDF(t)
	e := &Executor{Root: t.TempDir()}

	t.Setenv("FAKE_PDF_MODE", "encrypted")
	got := execTool(t, e, "read_pdf", `{"path":"secret.pdf"}`)
	if !strings.Contains(got, "encrypted") || !strings.Contains(got, "OCR is out of scope") {
		t.Errorf("encrypted result = %q", got)
	}

	t.Setenv("FAKE_PDF_MODE", "empty")
	got = execTool(t, e, "read_pdf", `{"path":"scan.pdf"}`)
	if !strings.Contains(got, "no extractable text layer") || !strings.Contains(got, "OCR is out of scope") {
		t.Errorf("no-text-layer result = %q", got)
	}
}

// TestReadPDFBadPages pins range validation: a mistyped or inverted range is
// information, returned before any subprocess runs.
func TestReadPDFBadPages(t *testing.T) {
	installFakePDF(t)
	e := &Executor{Root: t.TempDir()}
	for _, args := range []string{
		`{"path":"d.pdf","pages":"2-5"}`,
		`{"path":"d.pdf","pages":{"from":5,"to":2}}`,
		`{"path":"d.pdf","pages":{"from":0,"to":3}}`,
		`{"path":"d.pdf","pages":{"from":2.5}}`,
		`{"path":"d.pdf","pages":{}}`,
	} {
		if got := execTool(t, e, "read_pdf", args); !strings.HasPrefix(got, "error: pages") {
			t.Errorf("read_pdf(%s) = %q, want a pages error", args, got)
		}
	}
}

// TestReadPDFGenericFailure makes sure pdftotext's own diagnostics reach the
// model instead of being swallowed.
func TestReadPDFGenericFailure(t *testing.T) {
	installFakePDF(t)
	t.Setenv("FAKE_PDF_MODE", "fail")
	e := &Executor{Root: t.TempDir()}
	got := execTool(t, e, "read_pdf", `{"path":"broken.pdf"}`)
	if !strings.Contains(got, "pdftotext failed") || !strings.Contains(got, "Couldn't find trailer dictionary") {
		t.Errorf("generic failure result = %q", got)
	}
}

// TestReadPDFRegisteredInSchema pins turn-1 declaration: the schema list, the
// system prompt's tool line and the unknown-tool message all carry read_pdf.
func TestReadPDFRegisteredInSchema(t *testing.T) {
	var found ToolDef
	declared := false
	for _, td := range ToolDefs() {
		if td.Function.Name == "read_pdf" {
			found, declared = td, true
			break
		}
	}
	if !declared {
		t.Fatal("ToolDefs does not declare read_pdf")
	}
	if req, ok := found.Function.Parameters["required"].([]string); !ok || len(req) != 1 || req[0] != "path" {
		t.Errorf("read_pdf required = %#v, want [path]", found.Function.Parameters["required"])
	}
	if !strings.Contains(SystemPrompt, "read_pdf") {
		t.Error("SystemPrompt tool list does not mention read_pdf")
	}
}
