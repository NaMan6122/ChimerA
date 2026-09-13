// Export the logged-in chat.qwen.ai session from the running Chromium over CDP.
// Secrets go to a 0600 file; stdout prints key names and lengths only.
import { writeFileSync } from "node:fs";

const PORT = process.env.CDP_PORT || "63509";
const OUT = process.env.OUT || "logs/qwen-session.json";

const targets = await (await fetch(`http://127.0.0.1:${PORT}/json/list`)).json();
const page = targets.find((t) => t.type === "page" && t.url.includes("chat.qwen.ai"));
if (!page) {
  console.error("no chat.qwen.ai page found");
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

ws.onmessage = (ev) => {
  const msg = JSON.parse(ev.data);
  if (msg.id && pending.has(msg.id)) {
    const { resolve, reject } = pending.get(msg.id);
    pending.delete(msg.id);
    msg.error ? reject(new Error(JSON.stringify(msg.error))) : resolve(msg.result);
  }
};

ws.onopen = async () => {
  try {
    await send("Network.enable");
    const { cookies } = await send("Network.getCookies", {
      urls: ["https://chat.qwen.ai"],
    });
    const evalExpr = `JSON.stringify({
      ls: Object.fromEntries(Object.entries(localStorage)),
      ss: Object.fromEntries(Object.entries(sessionStorage))
    })`;
    const { result } = await send("Runtime.evaluate", {
      expression: evalExpr,
      returnByValue: true,
    });
    const storage = JSON.parse(result.value);

    const cookieJar = {};
    for (const c of cookies) cookieJar[c.name] = c.value;

    // A qwen access token is a JWT; find it in storage or cookies.
    let accessToken = null;
    let tokenKey = null;
    const scan = (obj, where) => {
      for (const [k, v] of Object.entries(obj || {})) {
        if (typeof v === "string" && /^eyJ[A-Za-z0-9_-]+\.eyJ/.test(v)) {
          accessToken = v;
          tokenKey = `${where}.${k}`;
        }
      }
    };
    scan(storage.ls, "localStorage");
    scan(storage.ss, "sessionStorage");
    scan(cookieJar, "cookie");

    writeFileSync(
      OUT,
      JSON.stringify(
        { exported_at: new Date().toISOString(), cookies: cookieJar, storage, access_token: accessToken },
        null,
        2
      ),
      { mode: 0o600 }
    );

    console.log(`wrote ${OUT}`);
    console.log(`cookies (${Object.keys(cookieJar).length}):`);
    for (const [k, v] of Object.entries(cookieJar)) console.log(`  ${k} (${v.length})`);
    console.log(`localStorage (${Object.keys(storage.ls).length}) keys:`);
    for (const [k, v] of Object.entries(storage.ls)) console.log(`  ${k} (${String(v).length})`);
    console.log(`sessionStorage (${Object.keys(storage.ss).length}) keys:`);
    for (const [k, v] of Object.entries(storage.ss)) console.log(`  ${k} (${String(v).length})`);
    console.log(`access_token: ${tokenKey ? tokenKey + " (" + accessToken.length + " chars)" : "NOT FOUND"}`);
  } catch (e) {
    console.error("export failed:", e.message);
    process.exitCode = 1;
  } finally {
    ws.close();
  }
};
