// Capture a logged-in provider's live web-API traffic over CDP, and export the
// session material the browserless transport needs (spec 012 §Risks).
//
// Answers the two questions the spec cannot: which anti-bot material is actually
// on the wire, and whether a CDN challenge sits in front of the API.
//
// Usage:
//   1. Launch Chromium with remote debugging (the `chimera auth login` browser
//      or any Chromium started with --remote-debugging-port=<CDP_PORT>).
//   2. Log in, then run:
//        node scripts/webapi-spike/cdp-capture.mjs deepseek
//        node scripts/webapi-spike/cdp-capture.mjs chatgpt
//   3. While it waits, send ONE message in that browser tab.
//
// Secrets go to a 0600 file; stdout prints key names, lengths and shapes only.
import { existsSync, readFileSync, writeFileSync } from "node:fs";

const PROVIDERS = {
  deepseek: {
    pageMatch: "chat.deepseek.com",
    cookieURL: "https://chat.deepseek.com",
    // Paths whose request headers/bodies we want on the record.
    paths: [
      "/api/v0/chat/completion",
      "/api/v0/chat/create_pow_challenge",
      "/api/v0/chat_session/create",
    ],
    // localStorage key holding the bearer token.
    tokenExpr: `localStorage.getItem('userToken')||''`,
    // Header(s) carrying the solved proof-of-work.
    powHeaders: ["x-ds-pow-response"],
    // Client-identification headers the transport must replicate.
    echoHeaders: [
      "x-client-platform",
      "x-client-version",
      "x-client-locale",
      "x-app-version",
      "authorization",
    ],
  },
  chatgpt: {
    pageMatch: "chatgpt.com",
    cookieURL: "https://chatgpt.com",
    paths: [
      "/backend-api/conversation",
      "/backend-api/sentinel/chat-requirements",
      "/backend-api/sentinel/req",
      "/backend-api/f/conversation",
      // Mints the bearer from the session cookie; its response shape is needed
      // to reconcile the token refresh path.
      "/api/auth/session",
    ],
    // ChatGPT's bearer is minted from the cookie by /api/auth/session.
    tokenExpr: `fetch('/api/auth/session').then(r=>r.json()).then(j=>(j&&j.accessToken)||'').catch(()=>'')`,
    awaitToken: true,
    powHeaders: [
      "openai-sentinel-proof-token",
      "openai-sentinel-chat-requirements-token",
      "openai-sentinel-turnstile-token",
    ],
    echoHeaders: ["oai-device-id", "oai-client-version", "oai-language", "x-conduit-token"],
    // The real turn is the frontend alias, not /backend-api/conversation, and it
    // does NOT carry a sentinel token — so completion cannot be detected from a
    // header. Treat the endpoint itself as the signal.
    isTurn: (u) =>
      u.includes("/backend-api/f/conversation") || /\/backend-api\/conversation$/.test(u),
  },
};

const provider = process.argv[2];
const conf = PROVIDERS[provider];
if (!conf) {
  console.error(`usage: node cdp-capture.mjs <${Object.keys(PROVIDERS).join("|")}>`);
  process.exit(2);
}

// Find the CDP port. A `chimera auth login` window is launched by rod, which
// picks its own port and records it in the profile's DevToolsActivePort file —
// so attaching to the *same* window needs no extra configuration.
function resolvePort() {
  if (process.env.CDP_PORT) return process.env.CDP_PORT;
  for (const f of [`browser_data/${provider}/DevToolsActivePort`, "browser_data/pool/DevToolsActivePort"]) {
    if (existsSync(f)) {
      const line = readFileSync(f, "utf8").split("\n")[0].trim();
      if (line) {
        console.log(`using CDP port ${line} (from ${f})`);
        return line;
      }
    }
  }
  return "63509";
}

const PORT = resolvePort();
const WAIT_SECONDS = Number(process.env.WAIT_SECONDS || 180);
const SESSION_OUT = process.env.SESSION_OUT || `logs/${provider}-session.json`;

const b64json = (s) => {
  try {
    return JSON.parse(Buffer.from(s, "base64").toString("utf8"));
  } catch {
    return null;
  }
};

// Summarize a body's shape without dumping its contents to the terminal. Values
// live only in the 0600 capture file.
const bodyShape = (body) => {
  if (!body) return "(none)";
  const t = String(body).trim();
  if (t.startsWith("<")) return `HTML ${t.length}b (challenge?)`;
  if (t.startsWith("{") || t.startsWith("[")) {
    try {
      const j = JSON.parse(t);
      const keys = Array.isArray(j) ? [`[${j.length}]`] : Object.keys(j);
      return `JSON keys=[${keys.slice(0, 24).join(",")}]`;
    } catch {
      /* fall through to the generic form */
    }
  }
  if (/^(event:|data:)/m.test(t)) {
    const lines = t.split("\n").filter((l) => l.startsWith("data:"));
    return `SSE ${lines.length} data line(s), ${t.length}b`;
  }
  return `${t.length}b`;
};

// Report a header's shape without leaking its value.
const shape = (name, value) => {
  if (value == null) return `${name}: (absent)`;
  const base = `${name}: len=${value.length}`;
  const dec = b64json(value.replace(/^gAAAAAB/, ""));
  if (dec && typeof dec === "object") {
    return `${base} base64-json keys=[${Object.keys(dec).join(",")}]`;
  }
  if (/^eyJ/.test(value)) return `${base} (JWT)`;
  return base;
};

const targets = await (await fetch(`http://127.0.0.1:${PORT}/json/list`)).json();
const page = targets.find((t) => t.type === "page" && t.url.includes(conf.pageMatch));
if (!page) {
  console.error(`no ${conf.pageMatch} page found on port ${PORT}`);
  process.exit(1);
}

const ws = new WebSocket(page.webSocketDebuggerUrl);
let seq = 0;
const pending = new Map();
const send = (method, params = {}) =>
  new Promise((resolve, reject) => {
    const id = ++seq;
    pending.set(id, { resolve, reject });
    ws.send(JSON.stringify({ id, method, params }));
  });

const captures = [];
const byRequest = new Map();

const matches = (url) => conf.paths.some((p) => url.includes(p));

// Write the capture file. Called after every body lands as well as at the end:
// a handshake-heavy provider can take many turns to satisfy a header-based stop
// condition, and holding everything in memory until the deadline means a kill or
// a timeout loses the whole round. Writing incrementally makes that impossible.
//
// exportedAt is declared here, above its first reader: flushCapture runs from the
// CDP event handler, which can fire before later top-level statements execute.
const exportedAt = new Date().toISOString();
const OUT = process.env.OUT || `logs/${provider}-capture.json`;
let wroteOnce = false;
const flushCapture = () => {
  try {
    writeFileSync(OUT, JSON.stringify({ exported_at: exportedAt, captures }, null, 2), {
      mode: 0o600,
    });
    wroteOnce = true;
  } catch (e) {
    console.error("capture flush failed:", e.message);
  }
};

// Bodies are truncated so a long completion does not bloat the capture file.
const MAX_BODY = 20000;

const onEvent = async (msg) => {
  if (msg.method === "Network.requestWillBeSent") {
    const { requestId, request } = msg.params;
    if (!matches(request.url)) return;
    const rec = {
      url: request.url,
      method: request.method,
      headers: request.headers,
      postData: request.postData ?? null,
      status: null,
      respHeaders: null,
      respBody: null,
    };
    captures.push(rec);
    byRequest.set(requestId, rec);
    console.log(`[req]  ${request.method} ${request.url}`);
  } else if (msg.method === "Network.responseReceived") {
    const rec = byRequest.get(msg.params.requestId);
    if (!rec) return;
    rec.status = msg.params.response.status;
    rec.respHeaders = msg.params.response.headers;
    rec.mimeType = msg.params.response.mimeType;
    console.log(`[resp] ${msg.params.response.status} ${rec.url}`);
  } else if (msg.method === "Network.loadingFinished") {
    // Response bodies are the point of this capture for a handshake-heavy
    // provider: ChatGPT's Sentinel reply carries the proof seed and the
    // turnstile flag, which are unguessable from the request side alone.
    const rec = byRequest.get(msg.params.requestId);
    if (!rec || rec.respBody !== null) return;
    try {
      const { body, base64Encoded } = await send("Network.getResponseBody", {
        requestId: msg.params.requestId,
      });
      rec.respBody = base64Encoded
        ? `<base64 ${body.length} chars>`
        : body.slice(0, MAX_BODY);
      if (!base64Encoded && body.length > MAX_BODY) rec.respBodyTruncated = true;
      flushCapture();
    } catch (e) {
      // A streamed or already-evicted response can refuse; record why rather
      // than silently leaving a hole.
      rec.respBody = `<unavailable: ${e.message}>`;
      flushCapture();
    }
  }
};

ws.onmessage = (ev) => {
  const msg = JSON.parse(ev.data);
  if (msg.id && pending.has(msg.id)) {
    const { resolve, reject } = pending.get(msg.id);
    pending.delete(msg.id);
    msg.error ? reject(new Error(JSON.stringify(msg.error))) : resolve(msg.result);
  } else if (msg.method) {
    onEvent(msg);
  }
};

ws.onopen = async () => {
  try {
    await send("Network.enable");
    await send("Runtime.enable");

    console.log(`\nListening for ${conf.pageMatch} API traffic on port ${PORT}.`);
    console.log(`Send ONE message in that browser tab within ${WAIT_SECONDS}s...\n`);

    const deadline = Date.now() + WAIT_SECONDS * 1000;
    while (Date.now() < deadline) {
      await new Promise((r) => setTimeout(r, 1000));
      // Stop early once a SUCCESSFUL turn is on the record, with its body.
      // Two conditions are accepted: a 2xx response whose request carried a
      // proof header (qwen, deepseek), or a 2xx on the provider's turn endpoint
      // (chatgpt, whose frontend no longer puts a sentinel token on the turn —
      // requiring one there meant the capture never stopped).
      const done = captures.some((c) => {
        if (!(c.status >= 200 && c.status < 300)) return false;
        if (conf.isTurn && conf.isTurn(c.url)) return true;
        return conf.powHeaders.some((h) => c.headers[h.toLowerCase()]);
      });
      if (done) break;
    }

    // ── Session material (mirrors scripts/qwenweb-spike/cdp-export.mjs) ──
    const { cookies } = await send("Network.getCookies", { urls: [conf.cookieURL] });
    const evalExpr = `JSON.stringify({ls:Object.fromEntries(Object.entries(localStorage)),ss:Object.fromEntries(Object.entries(sessionStorage))})`;
    const { result } = await send("Runtime.evaluate", {
      expression: evalExpr,
      returnByValue: true,
    });
    const storage = JSON.parse(result.value);

    const tokenRes = await send("Runtime.evaluate", {
      expression: conf.tokenExpr,
      returnByValue: true,
      awaitPromise: !!conf.awaitToken,
    });
    const accessToken = tokenRes.result?.value || null;

    const cookieJar = {};
    for (const c of cookies) cookieJar[c.name] = c.value;

    writeFileSync(
      SESSION_OUT,
      JSON.stringify(
        { exported_at: exportedAt, access_token: accessToken, cookies: cookieJar },
        null,
        2
      ),
      { mode: 0o600 }
    );
    flushCapture();

    // ── Summary: shapes, never values ──
    console.log(`\n=== capture: ${captures.length} request(s) -> ${OUT} ===`);
    for (const c of captures) {
      console.log(`\n${c.method} ${c.url} -> ${c.status ?? "no response"}`);
      const interesting = [...conf.powHeaders, ...conf.echoHeaders, "cookie", "user-agent", "origin", "referer"];
      for (const h of interesting) {
        if (c.headers[h.toLowerCase()] != null) console.log("  " + shape(h, c.headers[h.toLowerCase()]));
      }
      if (c.postData) {
        console.log(`  req body: ${bodyShape(c.postData)}`);
      }
      if (c.respBody) {
        console.log(`  resp body: ${bodyShape(c.respBody)}`);
      }
    }
    const ct = captures.flatMap((c) => Object.entries(c.respHeaders || {})).find(([k]) => k.toLowerCase() === "content-type");
    console.log(`\ncontent-type seen: ${ct ? ct[1] : "none"}`);
    console.log(`\n=== session -> ${SESSION_OUT} ===`);
    console.log(`cookies (${Object.keys(cookieJar).length}):`);
    for (const [k, v] of Object.entries(cookieJar)) console.log(`  ${k} (${v.length})`);
    console.log(`localStorage (${Object.keys(storage.ls).length}):`);
    for (const [k, v] of Object.entries(storage.ls)) console.log(`  ${k} (${String(v).length})`);
    console.log(`access_token: ${accessToken ? accessToken.length + " chars" : "NOT FOUND"}`);
  } catch (e) {
    console.error("capture failed:", e.message);
    process.exitCode = 1;
  } finally {
    ws.close();
  }
};

// Flush on the way out so an operator killing the wait still keeps the traffic.
for (const sig of ["SIGINT", "SIGTERM"]) {
  process.on(sig, () => {
    flushCapture();
    if (wroteOnce) console.error(`\ncaptured ${captures.length} request(s) -> ${OUT}`);
    process.exit(0);
  });
}
