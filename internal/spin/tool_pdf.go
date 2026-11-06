package spin

import (
	"context"
	"fmt"
	"math"
	"os/exec"
	"strings"
	"time"
)

// pdfToolTimeout bounds one pdftotext run. Same limit run_command gets: a hung
// subprocess must not hang the round.
const pdfToolTimeout = 60 * time.Second

// readPDF extracts text from a PDF under the workspace root by shelling out to
// pdftotext (poppler). Shelling out is deliberate: a Go PDF parser would be a
// dependency, and internal/spin imports nothing outside the stdlib.
//
// Arguments:
//
//	{"path": "report.pdf", "pages": {"from": 2, "to": 5}, "max_chars": 4000}
//
// Failures come back as tool-result text, never as aborts: a missing binary, an
// encrypted PDF or a page range past the last page are all information the model
// can act on.
func (e *Executor) readPDF(args map[string]any) string {
	abs, err := e.Resolve(str(args, "path"))
	if err != nil {
		return "error: " + err.Error()
	}
	from, to, err := pdfPageRange(args)
	if err != nil {
		return "error: " + err.Error()
	}

	bin, err := exec.LookPath("pdftotext")
	if err != nil {
		return "error: pdftotext (poppler) is not installed or not on PATH — " +
			"install it with `brew install poppler` (macOS) or `apt install poppler-utils` " +
			"(Debian/Ubuntu), then retry read_pdf"
	}

	argv := []string{"-layout"}
	if from > 0 {
		argv = append(argv, "-f", fmt.Sprintf("%d", from))
	}
	if to > 0 {
		argv = append(argv, "-l", fmt.Sprintf("%d", to))
	}
	argv = append(argv, abs, "-") // text to stdout

	ctx, cancel := context.WithTimeout(context.Background(), pdfToolTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, argv...).Output()
	if err != nil {
		return pdfExecError(err, out)
	}

	text := string(out)
	if strings.TrimSpace(text) == "" {
		return "error: " + str(args, "path") +
			" has no extractable text layer (likely a scanned/image-only PDF); " +
			"OCR is out of scope for read_pdf"
	}

	budget := MaxToolOutput
	if mc := num(args, "max_chars"); mc > 0 && mc < budget {
		budget = mc // max_chars may lower the budget, never raise it
	}
	if len(text) > budget {
		return text[:budget] + fmt.Sprintf(
			"\n… [truncated at %d chars; re-call read_pdf with narrower pages {\"from\":N,\"to\":M} to read the rest]", budget)
	}
	return text
}

// pdfPageRange validates the optional pages object. Only {"from":N,"to":M} with
// positive 1-indexed integers is accepted; a mistyped range is surfaced so the
// model can fix the call instead of the harness guessing.
func pdfPageRange(args map[string]any) (from, to int, err error) {
	raw, present := args["pages"]
	if !present || raw == nil {
		return 0, 0, nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return 0, 0, fmt.Errorf(`pages must be an object {"from":N,"to":M} (1-indexed page numbers), not %T`, raw)
	}
	if from, err = pdfPageNum(obj, "from"); err != nil {
		return 0, 0, err
	}
	if to, err = pdfPageNum(obj, "to"); err != nil {
		return 0, 0, err
	}
	if from == 0 && to == 0 {
		return 0, 0, fmt.Errorf(`pages must include "from" and/or "to" (1-indexed page numbers)`)
	}
	if from > 0 && to > 0 && from > to {
		return 0, 0, fmt.Errorf("pages.from (%d) is greater than pages.to (%d)", from, to)
	}
	return from, to, nil
}

// pdfPageNum reads one member of the pages object; 0 means "not set".
func pdfPageNum(obj map[string]any, key string) (int, error) {
	v, present := obj[key]
	if !present || v == nil {
		return 0, nil
	}
	f, ok := v.(float64)
	if !ok || f < 1 || f != math.Trunc(f) {
		return 0, fmt.Errorf("pages.%s must be a positive integer (1-indexed page number)", key)
	}
	return int(f), nil
}

// pdfExecError turns a pdftotext failure into text the model can act on.
// Encrypted PDFs (password-protected or copy-restricted) and scanned PDFs share
// one answer: OCR is out of scope.
func pdfExecError(err error, out []byte) string {
	msg := strings.TrimSpace(string(out))
	if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
		msg = strings.TrimSpace(string(ee.Stderr))
	}
	if msg == "" {
		msg = err.Error()
	}
	low := strings.ToLower(msg)
	if strings.Contains(low, "password") ||
		strings.Contains(low, "encrypt") ||
		strings.Contains(low, "copying of text") {
		return "error: the PDF is encrypted or copy-restricted (pdftotext: " + msg + "); " +
			"OCR is out of scope for read_pdf — supply a decrypted copy"
	}
	return "error: pdftotext failed: " + msg
}
