// Command qwenweb-spike talks to chat.qwen.ai's web API without Chromium.
//
// Spike for specs/011-provider-transports.md. It reimplements the client-side
// anti-bot material (ssxmod cookies, bx-ua, bx-umidtoken) in pure Go stdlib,
// creates a chat and streams one turn, reporting latency. Protocol reference:
// qwen-reverse 0.1.6 (MIT), https://github.com/whatever/qwen-reverse.
//
// Usage:
//
//	go run ./scripts/qwenweb-spike -list-models          # guest, no session
//	go run ./scripts/qwenweb-spike -n 3 -chars 30000     # latency run
//	go run ./scripts/qwenweb-spike -session logs/qwen-session.json
package main

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	qwenBase       = "https://chat.qwen.ai"
	defaultModel   = "qwen3.8-max"
	clientVersion  = "0.2.84"
	ua             = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"
	customB64Chars = "DGi0YA7BemWnQjCl4_bR3f8SKIF9tUz/xhr2oEOgPpac=61ZqwTudLkM5vHyNXsVJ"
)

// ── bit writer for the LZW output (6-bit codes, MSB-first byte packing) ────

type bitWriter struct {
	bits     int
	value    int
	position int
	out      []byte
}

func (b *bitWriter) emit() {
	b.out = append(b.out, customB64Chars[b.value])
	b.value = 0
	b.position = 0
}

// writeCode writes n bits of code, least-significant bit first.
func (b *bitWriter) writeCode(code, n int) {
	for i := 0; i < n; i++ {
		b.value = (b.value << 1) | (code & 1)
		if b.position == b.bits-1 {
			b.emit()
		} else {
			b.position++
		}
		code >>= 1
	}
}

// writeZeros writes n zero bits.
func (b *bitWriter) writeZeros(n int) {
	for i := 0; i < n; i++ {
		b.value <<= 1
		if b.position == b.bits-1 {
			b.emit()
		} else {
			b.position++
		}
	}
}

// flush pads with zero bits up to the next character boundary.
func (b *bitWriter) flush() {
	for {
		b.value <<= 1
		if b.position == b.bits-1 {
			b.emit()
			return
		}
		b.position++
	}
}

// lzwCompress is a port of qwen-reverse cookies.py lzw_compress (MIT).
func lzwCompress(data string, bits int, charFunc func(int) byte) string {
	dictionary := map[string]int{}
	dictToCreate := map[string]bool{}
	c, wc, w := "", "", ""
	enlargeIn := 2
	dictSize := 3
	numBits := 2
	var bw bitWriter
	bw.bits = bits
	bw.out = make([]byte, 0, len(data))

	for i := 0; i < len(data); i++ {
		c = string(data[i])
		if _, ok := dictionary[c]; !ok {
			dictionary[c] = dictSize
			dictSize++
			dictToCreate[c] = true
		}
		wc = w + c
		if _, ok := dictionary[wc]; ok {
			w = wc
			continue
		}
		if dictToCreate[w] {
			if int(w[0]) < 256 {
				bw.writeZeros(numBits)
				charCode := int(w[0])
				for j := 0; j < 8; j++ {
					bw.writeCode(charCode&1, 1)
					charCode >>= 1
				}
			} else {
				bw.writeCode(1, numBits)
				charCode := int(w[0])
				for j := 0; j < 16; j++ {
					bw.writeCode(charCode&1, 1)
					charCode >>= 1
				}
			}
			enlargeIn--
			if enlargeIn == 0 {
				enlargeIn = 1 << numBits
				numBits++
			}
			delete(dictToCreate, w)
		} else {
			charCode := dictionary[w]
			bw.writeCode(charCode, numBits)
		}
		// The reference decrements once more on the common path (twice total
		// on the literal path) — mirrored exactly so the encoding matches.
		enlargeIn--
		if enlargeIn == 0 {
			enlargeIn = 1 << numBits
			numBits++
		}
		dictionary[wc] = dictSize
		dictSize++
		w = c
	}

	if w != "" {
		if dictToCreate[w] {
			if int(w[0]) < 256 {
				bw.writeZeros(numBits)
				charCode := int(w[0])
				for j := 0; j < 8; j++ {
					bw.writeCode(charCode&1, 1)
					charCode >>= 1
				}
			} else {
				bw.writeCode(1, numBits)
				charCode := int(w[0])
				for j := 0; j < 16; j++ {
					bw.writeCode(charCode&1, 1)
					charCode >>= 1
				}
			}
			enlargeIn--
			if enlargeIn == 0 {
				enlargeIn = 1 << numBits
				numBits++
			}
			delete(dictToCreate, w)
		} else {
			charCode := dictionary[w]
			bw.writeCode(charCode, numBits)
		}
		enlargeIn--
		if enlargeIn == 0 {
			enlargeIn = 1 << numBits
			numBits++
		}
	}
	bw.writeCode(2, numBits)
	bw.flush()
	return string(bw.out)
}

func customEncode(data string, urlSafe bool) string {
	compressed := lzwCompress(data, 6, func(i int) byte { return customB64Chars[i] })
	if urlSafe {
		return compressed
	}
	switch len(compressed) % 4 {
	case 1:
		return compressed + "==="
	case 2:
		return compressed + "=="
	case 3:
		return compressed + "="
	}
	return compressed
}

// ── fingerprint ────────────────────────────────────────────────────────────

func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

func generateFingerprint() string {
	now := time.Now().UnixMilli()
	h := func() uint32 { return mrand.Uint32() }
	fields := []string{
		randHex(20), "websdk-2.3.15d", "1765348410850", "91", "1|15",
		"en-US", "480", "16705151|12791",
		"1470|956|283|797|158|0|1470|956|1470|798|0|0", "5", "MacIntel", "10",
		"ANGLE (Apple, ANGLE Metal Renderer: Apple M4, Unspecified Version)|Google Inc. (Apple)",
		"30|30", "0", "28",
		fmt.Sprintf("5|%d", h()),
		fmt.Sprint(h()), fmt.Sprint(h()),
		"1", "0", "1", "0", "P", "0", "0", "0", "416",
		"Google Inc.", "8", "-1|0|0|0|0", fmt.Sprint(h()),
		"11", fmt.Sprint(now), fmt.Sprint(h()), "0", fmt.Sprint(10 + mrand.IntN(91)),
	}
	return strings.Join(fields, "^")
}

// generateCookies ports qwen-reverse cookies.py generate_cookies (MIT).
func generateCookies(fp string) (string, string) {
	processed := strings.Split(fp, "^")
	if p := strings.Split(processed[16], "|"); len(p) == 2 {
		processed[16] = fmt.Sprintf("%s|%d", p[0], mrand.Uint32())
	}
	for _, idx := range []int{17, 18, 31, 34} {
		processed[idx] = fmt.Sprint(mrand.Uint32())
	}
	processed[36] = fmt.Sprint(10 + mrand.IntN(91))
	processed[33] = fmt.Sprint(time.Now().UnixMilli())

	itnaData := strings.Join(processed, "^")
	itna := "1-" + customEncode(itnaData, true)

	f := func(i int) string { return processed[i] }
	itna2Data := strings.Join([]string{
		f(0), f(1), f(23), "0", "", "0", "", "", "0", "0", "0",
		f(32), f(33), "0", "0", "0", "0", "0",
	}, "^")
	itna2 := "1-" + customEncode(itna2Data, true)
	return itna, itna2
}

// ── bx-ua (AES-CBC over a fingerprint payload) ─────────────────────────────

type bxDevice struct {
	DeviceID string `json:"deviceId"`
	SDKVer   string `json:"sdkVer"`
	Lang     string `json:"lang"`
	TZ       string `json:"tz"`
	Platform string `json:"platform"`
	Renderer string `json:"renderer"`
	Mode     string `json:"mode"`
	Vendor   string `json:"vendor"`
}

type bxPayload struct {
	V   string   `json:"v"`
	TS  int64    `json:"ts"`
	FP  string   `json:"fp"`
	D   bxDevice `json:"d"`
	Rnd int      `json:"rnd"`
	Seq int      `json:"seq"`
	CS  string   `json:"cs"`
}

func generateBXUA(fp string) string {
	ts := time.Now().UnixMilli()
	fields := strings.Split(fp, "^")
	rnd := 1000 + mrand.IntN(9000)
	cs := md5.Sum([]byte(fmt.Sprintf("%s%d%d", fp, ts, rnd)))
	payload := bxPayload{
		V: "231", TS: ts, FP: fp,
		D: bxDevice{
			DeviceID: fields[0], SDKVer: fields[1], Lang: fields[5], TZ: fields[6],
			Platform: fields[10], Renderer: fields[12], Mode: fields[23], Vendor: fields[28],
		},
		Rnd: rnd, Seq: 1, CS: hex.EncodeToString(cs[:])[:8],
	}
	raw, _ := json.Marshal(payload)

	seed := sha256.Sum256([]byte(fp))
	key, iv := seed[:16], seed[16:32]
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	padLen := aes.BlockSize - len(raw)%aes.BlockSize
	padded := append(raw, bytes.Repeat([]byte{byte(padLen)}, padLen)...)
	enc := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(enc, padded)
	return "231!" + base64.StdEncoding.EncodeToString(enc)
}

// ── client ─────────────────────────────────────────────────────────────────

type client struct {
	http    *http.Client
	token   string
	cookies map[string]string
	mid     string
}

func newClient(timeout time.Duration, token string, cookies map[string]string) *client {
	return &client{
		http:    &http.Client{Timeout: timeout},
		token:   token,
		cookies: cookies,
	}
}

var midRe = regexp.MustCompile(`(?:umx\.wu|__fycb)\('([^']+)'\)`)

func (c *client) fetchMidtoken() error {
	if c.mid != "" {
		return nil
	}
	resp, err := c.http.Get("https://sg-wum.alibaba.com/w/wu.json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	m := midRe.FindSubmatch(body)
	if m == nil {
		return fmt.Errorf("midtoken not found in wu.json (%d bytes)", len(body))
	}
	c.mid = string(m[1])
	return nil
}

func uuid4() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (c *client) headers(fp string) http.Header {
	itna, itna2 := generateCookies(fp)
	jar := map[string]string{"ssxmod_itna": itna, "ssxmod_itna2": itna2}
	for k, v := range c.cookies {
		jar[k] = v // imported session material wins over generated stamps
	}
	names := make([]string, 0, len(jar))
	for k := range jar {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, k := range names {
		parts = append(parts, k+"="+jar[k])
	}

	h := http.Header{}
	h.Set("User-Agent", ua)
	h.Set("Accept", "*/*")
	h.Set("Accept-Language", "en-US,en;q=0.5")
	h.Set("Origin", qwenBase)
	h.Set("Referer", qwenBase+"/")
	h.Set("Content-Type", "application/json")
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "same-origin")
	h.Set("Connection", "keep-alive")
	h.Set("X-Requested-With", "XMLHttpRequest")
	h.Set("source", "web")
	h.Set("version", clientVersion)
	h.Set("X-Accel-Buffering", "no")
	h.Set("Cookie", strings.Join(parts, "; "))
	h.Set("bx-ua", generateBXUA(fp))
	h.Set("bx-v", "2.5.37")
	h.Set("x-request-id", uuid4())
	if c.mid != "" {
		h.Set("bx-umidtoken", c.mid)
	}
	if c.token != "" {
		h.Set("Authorization", "Bearer "+c.token)
	}
	return h
}

func (c *client) do(req *http.Request) (*http.Response, error) {
	if err := c.fetchMidtoken(); err != nil {
		return nil, fmt.Errorf("midtoken: %w", err)
	}
	req.Header = c.headers(generateFingerprint())
	return c.http.Do(req)
}

func (c *client) chatMode() string {
	if c.token != "" {
		return "normal"
	}
	return "guest"
}

func (c *client) listModels() ([]string, error) {
	req, _ := http.NewRequest("GET", qwenBase+"/api/models", nil)
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet(body))
	}
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("non-JSON (%s): %s", resp.Header.Get("Content-Type"), snippet(body))
	}
	ids := make([]string, 0, len(doc.Data))
	for _, d := range doc.Data {
		ids = append(ids, d.ID)
	}
	sort.Strings(ids)
	return ids, nil
}

func (c *client) newChat(model string) (string, error) {
	payload := map[string]any{
		"title":      "New Chat",
		"models":     []string{model},
		"chat_mode":  c.chatMode(),
		"chat_type":  "t2t",
		"timestamp":  time.Now().UnixMilli(),
		"project_id": "",
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", qwenBase+"/api/v2/chats/new", bytes.NewReader(body))
	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if len(raw) > 0 && raw[0] == '<' {
		return "", fmt.Errorf("WAF blocked chat creation (HTML, %d bytes)", len(raw))
	}
	var doc struct {
		Success bool `json:"success"`
		Data    struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("non-JSON: %s", snippet(raw))
	}
	if !doc.Success || doc.Data.ID == "" {
		return "", fmt.Errorf("failed to create chat: %s", snippet(raw))
	}
	return doc.Data.ID, nil
}

type streamResult struct {
	TTFTReasoning time.Duration
	TTFTAnswer    time.Duration
	ResponseID    string
	Content       string
	Reasoning     string
	Usage         map[string]any
}

type delta struct {
	Phase     string `json:"phase"`
	Content   string `json:"content"`
	Reasoning string `json:"reasoning"`
	Status    string `json:"status"`
}

type chunk struct {
	Choices []struct {
		Delta delta `json:"delta"`
	} `json:"choices"`
	ResponseID string         `json:"response_id"`
	Usage      map[string]any `json:"usage"`
	Ret        []string       `json:"ret"`
	Error      *struct {
		Code    string `json:"code"`
		Details string `json:"details"`
	} `json:"error"`
}

func (c *client) send(model, chatID, parentID, prompt string, thinking bool, debug bool) (*streamResult, error) {
	featureConfig := map[string]any{
		"thinking_enabled": thinking,
		"output_schema":    "phase",
		"thinking_format":  "summary",
		"thinking_budget":  81920,
	}
	if thinking {
		featureConfig["auto_thinking"] = true
		featureConfig["thinking_mode"] = "Auto"
		featureConfig["research_mode"] = "normal"
		featureConfig["auto_search"] = false
	}
	now := time.Now().UnixMilli()
	payload := map[string]any{
		"stream":             true,
		"version":            "2.1",
		"incremental_output": true,
		"chat_id":            chatID,
		"chat_mode":          c.chatMode(),
		"model":              model,
		"parent_id":          parentID,
		"messages": []map[string]any{{
			"fid":            uuid4(),
			"parentId":       parentID,
			"childrenIds":    []string{},
			"role":           "user",
			"content":        prompt,
			"user_action":    "chat",
			"files":          []any{},
			"timestamp":      now,
			"models":         []string{model},
			"chat_type":      "t2t",
			"feature_config": featureConfig,
			"extra":          map[string]any{"meta": map[string]string{"subChatType": "t2t"}},
			"sub_chat_type":  "t2t",
		}},
		"timestamp": now,
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", qwenBase+"/api/v2/chat/completions?chat_id="+chatID, bytes.NewReader(body))
	start := time.Now()
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet(raw))
	}
	// WAF challenges arrive as a plain JSON body (`{"ret":["FAIL_SYS_..."],...}`)
	// instead of an SSE stream.
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var ch chunk
		if err := json.Unmarshal(raw, &ch); err == nil && len(ch.Ret) > 0 {
			return nil, fmt.Errorf("WAF challenge: %s", strings.Join(ch.Ret, " "))
		}
		return nil, fmt.Errorf("unexpected content-type %q: %s", ct, snippet(raw))
	}

	res := &streamResult{}
	var content, reasoning strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if debug {
			fmt.Fprintln(os.Stderr, "SSE:", line)
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var ch chunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			continue
		}
		if ch.Error != nil {
			return nil, fmt.Errorf("upstream error %s: %s", ch.Error.Code, ch.Error.Details)
		}
		if ch.ResponseID != "" {
			res.ResponseID = ch.ResponseID
		}
		if ch.Usage != nil {
			res.Usage = ch.Usage
		}
		if len(ch.Choices) == 0 {
			continue
		}
		d := ch.Choices[0].Delta
		switch {
		case d.Reasoning != "" || d.Phase == "think":
			if res.TTFTReasoning == 0 {
				res.TTFTReasoning = time.Since(start)
			}
			if d.Reasoning != "" {
				reasoning.WriteString(d.Reasoning)
			} else {
				reasoning.WriteString(d.Content)
			}
		case d.Content != "" && d.Phase == "answer":
			if res.TTFTAnswer == 0 {
				res.TTFTAnswer = time.Since(start)
			}
			content.WriteString(d.Content)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	res.Content = content.String()
	res.Reasoning = reasoning.String()
	return res, nil
}

func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 160 {
		return s[:160]
	}
	return s
}

// ── main ───────────────────────────────────────────────────────────────────

type spinResult struct {
	Index      int     `json:"index"`
	CreateS    float64 `json:"create_s"`
	TTFTRs     float64 `json:"ttft_reasoning_s"`
	TTFTAs     float64 `json:"ttft_answer_s"`
	TotalS     float64 `json:"total_s"`
	Chars      int     `json:"answer_chars"`
	ReasoningC int     `json:"reasoning_chars"`
	Err        string  `json:"error,omitempty"`
	Usage      any     `json:"usage,omitempty"`
}

func pct(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	k := int(p/100*float64(len(s)-1) + 0.5)
	if k < 0 {
		k = 0
	}
	if k >= len(s) {
		k = len(s) - 1
	}
	return s[k]
}

func main() {
	var (
		model      = flag.String("model", defaultModel, "qwen model id")
		n          = flag.Int("n", 1, "number of independent spins")
		prompt     = flag.String("prompt", "Reply with exactly the word PONG and nothing else.", "prompt text")
		chars      = flag.Int("chars", 0, "if >0, synthesize a prompt of this many chars")
		session    = flag.String("session", "", "JSON file with {\"access_token\":\"...\"} (empty = guest)")
		timeout    = flag.Duration("timeout", 180*time.Second, "per-request timeout")
		thinking   = flag.Bool("thinking", true, "enable model thinking")
		listModels = flag.Bool("list-models", false, "list available models and exit")
		debug      = flag.Bool("debug", false, "dump raw SSE lines to stderr")
		jsonOut    = flag.String("json", "", "write raw results to this path")
	)
	flag.Parse()

	token := ""
	cookies := map[string]string{}
	if *session != "" {
		raw, err := os.ReadFile(*session)
		if err != nil {
			fmt.Fprintln(os.Stderr, "session:", err)
			os.Exit(1)
		}
		var doc struct {
			AccessToken string            `json:"access_token"`
			Cookies     map[string]string `json:"cookies"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil || doc.AccessToken == "" {
			fmt.Fprintln(os.Stderr, "session file must contain access_token (export with cdp-export.mjs)")
			os.Exit(1)
		}
		token = doc.AccessToken
		for k, v := range doc.Cookies {
			cookies[k] = v
		}
	}

	c := newClient(*timeout, token, cookies)
	mode := "guest"
	if token != "" {
		mode = fmt.Sprintf("logged-in (%d imported cookies)", len(cookies))
	}

	if *listModels {
		ids, err := c.listModels()
		if err != nil {
			fmt.Fprintln(os.Stderr, "list models:", err)
			os.Exit(1)
		}
		fmt.Printf("models (%s):\n", mode)
		for _, id := range ids {
			fmt.Println(" ", id)
		}
		return
	}

	if *chars > 0 {
		unit := "Tool get_weather: returns conditions for a city. Params: city (string). "
		reps := *chars/len(unit) + 1
		*prompt = strings.Repeat(unit, reps)[:*chars] + "\n" + *prompt
	}

	fmt.Printf("qwenweb-spike — mode=%s model=%s n=%d prompt=%d chars thinking=%v\n",
		mode, *model, *n, len(*prompt), *thinking)

	results := make([]spinResult, 0, *n)
	var totals, ttfts []float64
	for i := 0; i < *n; i++ {
		t0 := time.Now()
		chatID, err := c.newChat(*model)
		createS := time.Since(t0).Seconds()
		if err != nil {
			results = append(results, spinResult{Index: i, CreateS: createS, Err: err.Error()})
			fmt.Printf("  spin %d: create=%.2fs ERROR %v\n", i, createS, err)
			continue
		}
		res, err := c.send(*model, chatID, "", *prompt, *thinking, *debug)
		total := time.Since(t0)
		if err != nil {
			results = append(results, spinResult{Index: i, CreateS: createS, TotalS: total.Seconds(), Err: err.Error()})
			fmt.Printf("  spin %d: create=%.2fs total=%.2fs ERROR %v\n", i, createS, total.Seconds(), err)
			continue
		}
		r := spinResult{
			Index:      i,
			CreateS:    createS,
			TTFTRs:     res.TTFTReasoning.Seconds(),
			TTFTAs:     res.TTFTAnswer.Seconds(),
			TotalS:     total.Seconds(),
			Chars:      len(res.Content),
			ReasoningC: len(res.Reasoning),
			Usage:      res.Usage,
		}
		results = append(results, r)
		totals = append(totals, r.TotalS)
		if r.TTFTAs > 0 {
			ttfts = append(ttfts, r.TTFTAs)
		}
		fmt.Printf("  spin %d: create=%.2fs ttft_reasoning=%.2fs ttft_answer=%.2fs total=%.2fs answer=%q\n",
			i, r.CreateS, r.TTFTRs, r.TTFTAs, r.TotalS, truncate(res.Content, 60))
	}

	if len(totals) > 0 {
		fmt.Printf("summary: ok=%d/%d total p50=%.2fs p95=%.2fs min=%.2fs max=%.2fs",
			len(totals), *n, pct(totals, 50), pct(totals, 95),
			pct(totals, 0), pct(totals, 100))
		if len(ttfts) > 0 {
			fmt.Printf("  ttft_answer p50=%.2fs", pct(ttfts, 50))
		}
		fmt.Println()
	}

	if *jsonOut != "" {
		doc := map[string]any{"args": os.Args[1:], "results": results}
		raw, _ := json.MarshalIndent(doc, "", "  ")
		if err := os.WriteFile(*jsonOut, raw, 0600); err != nil {
			fmt.Fprintln(os.Stderr, "write json:", err)
		} else {
			fmt.Println("raw results ->", *jsonOut)
		}
	}
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
