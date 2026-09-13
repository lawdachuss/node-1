// CDP driver: load each {filename, url} in headless Chrome, passively capture
// HTTP response bodies via the Network domain (works across page reloads),
// filter to video-api/stream-config responses, and write them to out.json.
// Usage: node anonmp4_capture.js <entries.json> <out.json>
// Requires Node >= 21 (global fetch + WebSocket) and Chrome listening on
// 127.0.0.1:9222 (--remote-debugging-port=9222 --headless=new).
const fs = require('fs');

const entries = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
const outFile = process.argv[3];
const CDP = 'http://127.0.0.1:9222';
const results = [];

function interesting(url) {
  return /video-api|tracks|\/api\//.test(url) || /\.m3u8$/.test(url);
}

async function runEntry(entry) {
  const t = await (await fetch(`${CDP}/json/new?` + encodeURIComponent('about:blank'), { method: 'PUT' })).json();
  const ws = new WebSocket(t.webSocketDebuggerUrl);
  let id = 0;
  const pending = new Map();
  const send = (method, params = {}) => new Promise((res, rej) => {
    const i = ++id;
    pending.set(i, { res, rej });
    ws.send(JSON.stringify({ id: i, method, params }));
  });
  const reqs = new Map();    // requestId -> url
  const bodies = [];         // captured {url, status, body}
  const events = [];
  let draining = false;

  ws.onmessage = async e => {
    const m = JSON.parse(e.data);
    if (m.id && pending.has(m.id)) { pending.get(m.id).res(m.result); pending.delete(m.id); return; }
    if (!m.method) return;
    events.push(m.method);
    try {
      if (m.method === 'Network.requestWillBeSent') {
        reqs.set(m.params.requestId, m.params.request.url);
      }
      if (m.method === 'Network.loadingFinished' || m.method === 'Network.responseReceived') {
        const rid = m.params.requestId;
        const u = reqs.get(rid);
        if (u && interesting(u)) {
          if (m.method === 'Network.loadingFinished') {
            try {
              const r = await send('Network.getResponseBody', { requestId: rid });
              const b = r.body || '';
              bodies.push({ url: u, status: (r.base64Encoded ? 'b64' : ''), body: String(b).slice(0, 300000) });
            } catch {}
          } else {
            bodies.push({ url: u, status: m.params.response.status, body: '<<headers-only>>' });
          }
        }
      }
    } catch {}
  };

  await new Promise(res => ws.onopen = res);
  await send('Network.enable');
  await send('Page.enable');
  await send('Page.navigate', { url: entry.url });
  await new Promise(r => setTimeout(r, 15000));

  let state = {};
  try {
    const expr = `JSON.stringify({ url: document.URL, title: document.title, ready: document.readyState })`;
    const r = await send('Runtime.evaluate', { expression: expr, returnByValue: true });
    state = r.result && r.result.result && r.result.result.value ? JSON.parse(r.result.result.value) : {};
  } catch (err) {
    if (typeof err === 'object' && err && !('message' in err)) { draining = true; }
    state = { err: String(err && err.message || err) };
  }
  try { await fetch(`${CDP}/json/close/${t.id}`); } catch {}
  try { ws.close(); } catch {}
  results.push({ filename: entry.filename, url: entry.url, state, bodies });
}

(async () => {
  for (const e of entries) {
    try { await runEntry(e); } catch (err) { results.push({ filename: e.filename, url: e.url, error: String(err && err.message || err) }); }
    fs.writeFileSync(outFile, JSON.stringify(results, null, 1));
  }
  setTimeout(() => process.exit(0), 300);
  setTimeout(() => process.exit(0), 400000).unref();
})().catch(e => { console.error(e); process.exit(1); });