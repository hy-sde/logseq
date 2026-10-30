// CDP IPC micro-benchmark against a Chromium/DevTools endpoint.
// Usage: node cdp-ipc.mjs <port> <calls>
// Evaluates N sequential window.apis.doAction(["getAppBaseInfo"]) round-trips in the
// first tab and prints elapsed ms + us/call as JSON.
const port = process.argv[2] || "9222";
const calls = parseInt(process.argv[3] || "200", 10);
const list = await (await fetch(`http://127.0.0.1:${port}/json/list`)).json();
const page = list.find(t => t.type === "page");
if (!page) { console.error("no page target"); process.exit(2); }
const ws = new WebSocket(page.webSocketDebuggerUrl);
await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
let id = 0;
const pending = new Map();
ws.onmessage = (ev) => { const m = JSON.parse(ev.data); if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); } };
const send = (method, params) => new Promise((res) => { const i = ++id; pending.set(i, res); ws.send(JSON.stringify({ id: i, method, params })); });
const expr = `(async () => {
  if (!window.apis || !window.apis.doAction) return "NOAPIS";
  const t0 = performance.now();
  for (let i = 0; i < ${calls}; i++) { await window.apis.doAction(["getAppBaseInfo"]); }
  return performance.now() - t0;
})()`;
const r = await send("Runtime.evaluate", { expression: expr, awaitPromise: true, returnByValue: true });
ws.close();
if (r.result?.result?.value === "NOAPIS") { console.error("apis not ready"); process.exit(3); }
const ms = r.result.result.value;
console.log(JSON.stringify({ calls, totalMs: ms, usPerCall: Math.round((ms / calls) * 1000) }));
