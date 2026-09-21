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
      "/backend-api/f/conversation",
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
const OUT = process.env.OUT || `logs/${provider}-capture.json`;
const SESSION_OUT = process.env.SESSION_OUT || `logs/${provider}-session.json`;

const b64json = (s) => {
  try {
    return JSON.parse(Buffer.from(s, "base64").toString("utf8"));
  } catch {
    return null;
  }
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

const onEvent = (msg) => {
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
    };
    captures.push(rec);
    byRequest.set(requestId, rec);
    console.log(`[req]  ${request.method} ${request.url}`);
  } else if (msg.method === "Network.responseReceived") {
    const rec = byRequest.get(msg.params.requestId);
    if (!rec) return;
    rec.status = msg.params.response.status;
    rec.respHeaders = msg.params.response.headers;
    console.log(`[resp] ${msg.params.response.status} ${rec.url}`);
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

const exportedAt = new Date().toISOString();

ws.onopen = async () => {
  try {
    await send("Network.enable");
    await send("Runtime.enable");

    console.log(`\nListening for ${conf.pageMatch} API traffic on port ${PORT}.`);
    console.log(`Send ONE message in that browser tab within ${WAIT_SECONDS}s...\n`);

    const deadline = Date.now() + WAIT_SECONDS * 1000;
    while (Date.now() < deadline) {
      await new Promise((r) => setTimeout(r, 1000));
      // Stop early once a SUCCESSFUL turn carrying a PoW header is on the record.
      // Requiring 2xx matters: a pre-login attempt can also carry a pow header
      // and come back 401, and stopping on that would end the capture before the
      // user has even logged in.
      const done = captures.some(
        (c) =>
          c.status >= 200 &&
          c.status < 300 &&
          conf.powHeaders.some((h) => c.headers[h.toLowerCase()])
      );
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
    writeFileSync(OUT, JSON.stringify({ exported_at: exportedAt, captures }, null, 2), {
      mode: 0o600,
    });

    // ── Summary: shapes, never values ──
    console.log(`\n=== capture: ${captures.length} request(s) -> ${OUT} ===`);
    for (const c of captures) {
      console.log(`\n${c.method} ${c.url} -> ${c.status ?? "no response"}`);
      const interesting = [...conf.powHeaders, ...conf.echoHeaders, "cookie", "user-agent", "origin", "referer"];
      for (const h of interesting) {
        if (c.headers[h.toLowerCase()] != null) console.log("  " + shape(h, c.headers[h.toLowerCase()]));
      }
      if (c.postData) {
        const body = JSON.parse(c.postData);
        console.log(`  body keys: [${Object.keys(body).join(",")}]`);
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
