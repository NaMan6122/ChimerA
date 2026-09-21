package deepseekweb

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chimera/chimera/internal/providers/webapi"
)

// TestLiveFlowDump is a manual probe, not a CI test: it runs a real turn against
// chat.deepseek.com and logs the raw response shapes, so the client's parsing can
// be written against the wire format rather than against documentation.
//
//	go test ./internal/providers/webapi/deepseek/ -run TestLiveFlowDump -v
//
// It is skipped unless DEEPSEEK_LIVE=1 and a session exists at auth_data/deepseek.json.
func TestLiveFlowDump(t *testing.T) {
	if os.Getenv("DEEPSEEK_LIVE") != "1" {
		t.Skip("set DEEPSEEK_LIVE=1 to run the live probe")
	}
	sess, err := webapi.LoadSession("../../../../auth_data/deepseek.json")
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	c := NewClient(sess, 60*time.Second)

	prompt := os.Getenv("DEEPSEEK_PROMPT")
	if prompt == "" {
		prompt = "Reply with exactly: PONG"
	}
	dumpPath := os.Getenv("DEEPSEEK_DUMP")
	if dumpPath == "" {
		dumpPath = "/tmp/ds_sse_raw.txt"
	}
	thinking := os.Getenv("DEEPSEEK_THINKING") == "1"

	// ── 1. create chat session ──────────────────────────────────────────
	req, _ := http.NewRequest("POST", c.BaseURL+"/api/v0/chat_session/create", strings.NewReader("{}"))
	resp, err := c.do(req)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	t.Logf("CREATE SESSION %d ct=%s\n%s", resp.StatusCode, resp.Header.Get("Content-Type"), string(body))

	// Parse the id the way the server actually nests it, for use below.
	var env struct {
		Code int `json:"code"`
		Data struct {
			BizData struct {
				ID         string `json:"id"`
				ChatSessID string `json:"chat_session_id"`
				ChatSess   struct {
					ID string `json:"id"`
				} `json:"chat_session"`
			} `json:"biz_data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("create session not JSON: %v", err)
	}
	biz := env.Data.BizData
	sessionID := firstNonEmpty(biz.ID, biz.ChatSessID, biz.ChatSess.ID)
	if sessionID == "" {
		t.Fatalf("could not find a session id in %s", string(body))
	}
	t.Logf("parsed session id = %s", sessionID)

	// ── 2. pow challenge + solve ────────────────────────────────────────
	ch, err := c.CreatePowChallenge(CompletionPath)
	if err != nil {
		t.Fatalf("pow challenge: %v", err)
	}
	header, err := Solve(*ch)
	if err != nil {
		t.Fatalf("solve: %v", err)
	}
	t.Logf("solved pow for salt=%s expire_at=%d difficulty=%d", ch.Salt, ch.ExpireAt, ch.Difficulty)

	// ── 3. completion (dump the SSE head) ───────────────────────────────
	payloadBytes, _ := json.Marshal(map[string]any{
		"chat_session_id": sessionID, "parent_message_id": nil,
		"model_type": modelType(thinking), "prompt": prompt,
		"ref_file_ids": []any{}, "thinking_enabled": thinking,
		"search_enabled": false, "action": nil, "preempt": false,
	})
	req2, _ := http.NewRequest("POST", c.BaseURL+CompletionPath, strings.NewReader(string(payloadBytes)))
	req2.Header.Set("x-ds-pow-response", header)
	resp2, err := c.do(req2)
	if err != nil {
		t.Fatalf("completion: %v", err)
	}
	defer resp2.Body.Close()
	t.Logf("COMPLETION %d ct=%s", resp2.StatusCode, resp2.Header.Get("Content-Type"))

	// Write the raw stream to a file so the parser can be written against the
	// complete protocol rather than a truncated sample.
	out, err := os.Create(dumpPath)
	if err != nil {
		t.Fatalf("create dump: %v", err)
	}
	defer out.Close()
	sc := bufio.NewScanner(resp2.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 4<<20)
	for sc.Scan() {
		fmt.Fprintln(out, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Logf("scan ended: %v", err)
	}
	t.Logf("raw SSE written to %s", dumpPath)
}
