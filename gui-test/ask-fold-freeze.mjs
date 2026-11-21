// 「计划卡点开 → 右栏计划全文 → 前端卡死」复现/验收（隔离实例 + headless Edge）。
//
// 量四件事：
//   1) md() 单次渲染耗时（计划全文渲染就在这一句里，主线程同步卡住）；
//   2) 点计划卡后 sync 耗时 + 右栏多久真的画出正文；
//   3) 折叠 ↔ 展开来回 5 次，每次耗时、按钮是否可用；
//   4) 控制台异常 / DOM 节点 / 监听器累计（看有没有越点越多）。
//
// 运行：仓库根目录 node gui-test/ask-fold-freeze.mjs
import { newTab, read, click, sleep, shot, center, closeTab } from './cdp.mjs';
import { spawn, execSync } from 'node:child_process';
import { writeFileSync, mkdirSync, existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = fileURLToPath(new URL('..', import.meta.url));
const TMP = join(ROOT, '.jxai', 'tmp', 'askfold');
const SHOTS = join(TMP, 'shots');
const PORT = 8852;
const BASE = 'http://127.0.0.1:' + PORT;
const TAG = process.argv[2] || 'freeze';
mkdirSync(SHOTS, { recursive: true });

async function ensureEdge() {
  try { await fetch('http://127.0.0.1:9223/json/version'); return; } catch (e) {}
  spawn('C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
    ['--headless=new', '--remote-debugging-port=9223', '--user-data-dir=' + join(TMP, 'edge'), 'about:blank'],
    { stdio: 'ignore' });
  for (let i = 0; i < 40; i++) { await sleep(500); try { await fetch('http://127.0.0.1:9223/json/version'); return; } catch (e) {} }
  throw new Error('Edge CDP 没起来');
}
async function startServer() {
  const dir = join(TMP, TAG);
  mkdirSync(dir, { recursive: true });
  writeFileSync(join(dir, 'config.json'), JSON.stringify({
    port: PORT, bindHost: '127.0.0.1', baseURL: 'mock', apiKey: '', model: 'mock-model',
    workspace: join(dir, 'ws'), dataDir: join(dir, 'data'), skillsDir: join(dir, 'skills'), allowShell: false }));
  const proc = spawn(join(TMP, 'server.exe'), ['-config', 'config.json', '-noopen', '-webdir', join(ROOT, 'web')],
    { cwd: dir, stdio: 'ignore' });
  for (let i = 0; i < 60; i++) { await sleep(400); try { const r = await fetch(BASE + '/api/status'); if (r.ok) return proc; } catch (e) {} }
  throw new Error('server 没起来');
}
async function tryRead(c, expr, ms = 20000) {
  const t0 = Date.now();
  const r = await Promise.race([
    read(c, expr).then((v) => ({ ok: true, v })).catch((e) => ({ ok: false, err: String(e).slice(0, 200) })),
    sleep(ms).then(() => ({ ok: false, timeout: true })),
  ]);
  r.rt = Date.now() - t0;
  return r;
}

// 真实体量的计划：含目录路径（多段斜杠、结尾没有扩展名）——就是会把路径正则
// 拖进指数回溯的形态；仓库里 54 份 md 里最慢的一份单次渲染要 4.3 秒。
const NASTY = [
  '# 把日志与快照目录整理干净', '',
  '## 现状', '',
  '- 快照堆在 data/snapshots/2026/11/18/askfold/shots/repro 下面，按会话分目录；',
  '- 日志在 logs/2026/11/18/run/main/stdout/stderr 里滚；',
  '- 归档目标 a/b/c/d/e/f/g/h/i/j/k/l/m/n/o/p/q/r/s/t/u/v/w/x/y/z（先建目录再说）；',
  '- 临时目录 tmp/upload/parts/2026/11/18/chunk/0 里还有残片。', '',
  '## 步骤', '',
  '1. 先停服务、确认没有在写的分片；',
  '2. 归档 7 天前的快照到 archive/snapshots/2026/11/old；',
  '3. 清掉 tmp/upload/parts 下的残片，重建分片索引；',
  '4. 重启后核对 data/store.go 与 web/app-chat.js 的行为没变。', '',
  '## 风险', '',
  '- 清错目录会丢会话快照（先 dry-run 打印清单）；',
  '- 重启窗口期内的上传会失败一次（客户端重试）。', '',
  '```sh', 'go build -o jx.exe .', '```', '',
  '## 验收', '', '- 目录干净、服务正常、上传恢复。',
].join('\n');

const WORST_DOC = (() => {
  try { return readFileSync(join(ROOT, '临时文档', '对话区Markdown渲染器切换marked-验收报告.md'), 'utf8'); }
  catch (e) { return ''; }
})();

function stubExpr(sid) {
  const fx = {
    approvals: [{ id: 'ap-demo', sessionId: sid, tool: 'run_command',
      summary: 'powershell -Command "Get-ChildItem D:\\logs | Remove-Item -Recurse"', reason: '清理临时日志' }],
    questions: [
      { id: 'q-plan', sessionId: sid, kind: 'plan', question: '请审批以下执行计划', plan: NASTY },
      { id: 'q-ask', sessionId: sid, kind: 'ask', question: '要保留哪几份历史日志？',
        options: ['只留最近 3 天', '留最近 7 天'], deadlineMs: Date.now() + 125000 },
    ],
  };
  return `(() => {
    window.__fx = ${JSON.stringify(fx)};
    window.__worst = ${JSON.stringify(WORST_DOC.slice(0, 120000))};
    window.__posts = [];
    const json = (b) => Promise.resolve(new Response(JSON.stringify(b), { status: 200, headers: { 'Content-Type': 'application/json' } }));
    const real = window.fetch.bind(window);
    window.fetch = (u, o) => {
      const s = String(u);
      if (/\\/api\\/(approvals|questions)\\/[^/]+$/.test(s)) { window.__posts.push({ url: s, body: (o && o.body) || '' }); return json({ ok: '1' }); }
      if (s.indexOf('/api/approvals') >= 0) return json({ approvals: window.__fx.approvals });
      if (s.indexOf('/api/questions') >= 0) return json({ questions: window.__fx.questions });
      return real(u, o);
    };
    const ol = EventTarget.prototype.addEventListener;
    window.__lis = {};
    EventTarget.prototype.addEventListener = function (type) {
      const t = this;
      const name = (t === window) ? 'window' : (t && t.nodeType === 1)
        ? (t.tagName + (t.className ? '.' + String(t.className).split(' ')[0] : '')) : (t && t.nodeName) || 'other';
      window.__lis[name + ':' + type] = (window.__lis[name + ':' + type] || 0) + 1;
      return ol.apply(this, arguments);
    };
    return true;
  })()`;
}

await ensureEdge();
if (!existsSync(join(TMP, 'server.exe'))) {
  execSync('go build -o "' + join(TMP, 'server.exe') + '" .', { cwd: ROOT, stdio: 'inherit',
    env: { ...process.env, GOCACHE: join(TMP, 'gocache'), GOTMPDIR: join(TMP, 'gotmp') } });
}
const proc = await startServer();
const c = await newTab(BASE + '/index.html', 1440, 900);
try {
  await sleep(1500);
  const logs = [];
  await c.send('Log.enable', {});
  c.ws.addEventListener('message', (ev) => {
    const m = JSON.parse(ev.data);
    if (m.method === 'Runtime.exceptionThrown') logs.push('EXCEPTION ' + JSON.stringify(m.params.exceptionDetails).slice(0, 200));
    if (m.method === 'Log.entryAdded' && m.params.entry.level === 'error') logs.push('ERROR ' + m.params.entry.text.slice(0, 200));
  });

  await read(c, `(async () => { await fetch('/api/sessions', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{"title":"卡死复现"}' }); return true; })()`);
  await sleep(300);
  const ids = await read(c, `(async () => { const d = await (await fetch('/api/sessions')).json(); return d.sessions.map((s) => s.id); })()`);
  await read(c, `openSession(${JSON.stringify(ids[0])})`);
  await sleep(1200);
  await read(c, 'if (typeof closeWizard === "function") closeWizard();');
  await sleep(300);
  await read(c, stubExpr(ids[0]));

  // ---- 1) md() 单次渲染耗时（先关掉自动打开的右栏，用裸函数量） ----
  const mdNasty = await tryRead(c, `(() => { const t0 = performance.now(); const h = md(window.__fx.questions[0].plan);
    return { ms: Math.round(performance.now() - t0), len: h.length }; })()`, 60000);
  console.log('md(计划正文) :', JSON.stringify(mdNasty));
  const mdWorst = await tryRead(c, `(() => { const t0 = performance.now(); const h = md(window.__worst);
    return { ms: Math.round(performance.now() - t0), len: h.length }; })()`, 60000);
  console.log('md(仓库里最慢的真实文档):', JSON.stringify(mdWorst));

  // ---- 2) 正常路径：加载待办 → 点计划卡 → 右栏是否画出正文 ----
  const load = await tryRead(c, '(async () => { await loadApprovals(); await loadQuestions(); return true; })()', 60000);
  console.log('loadQuestions:', JSON.stringify(load).slice(0, 200));
  await sleep(1500);
  await read(c, 'if (typeof rpOpen === "function" && rpActive) toggleRpanel()');
  await sleep(500);

  const open = await tryRead(c, `(() => { const t0 = performance.now();
    document.querySelector('#ask-bar .q-card.plan').click();
    return { syncMs: Math.round(performance.now() - t0) }; })()`, 60000);
  console.log('点计划卡（同步部分）:', JSON.stringify(open));
  const panel = await tryRead(c, `(() => ({ panelOpen: !document.getElementById('rpanel').classList.contains('hidden'),
    active: String(rpActive), docLen: (document.getElementById('plan-doc') || {}).innerHTML.length,
    hasH2: !!document.querySelector('#plan-doc h2'), nodes: document.getElementsByTagName('*').length,
    lis: { ...window.__lis } }))()`, 20000);
  console.log('右栏状态:', JSON.stringify(panel).slice(0, 800));
  await shot(c, join(SHOTS, TAG + '-plan-open.png'));

  // ---- 3) 三个按钮是否可用 ----
  const btns = await tryRead(c, `(() => {
    const q = (s) => { const el = document.getElementById(s); return el ? { text: el.textContent.trim(), onclick: typeof el.onclick } : null; };
    return { act: document.getElementById('plan-act').classList.contains('hidden') ? 'hidden' : 'shown',
      cancel: q('plan-cancel'), approve: q('plan-approve'), reject: q('plan-reject') }; })()`, 20000);
  console.log('计划页按钮:', JSON.stringify(btns));

  // ---- 4) 折叠 ↔ 展开 5 次 × 三张卡：页面内同步计时（performance.now 包住 click） ----
  await read(c, `(() => { window.__t = (sel) => { const el = document.querySelector(sel); if (!el) return { err: 'no ' + sel };
    const t0 = performance.now(); el.click(); return { ms: +(performance.now() - t0).toFixed(1) }; }; return true; })()`);
  await read(c, 'if (rpActive) toggleRpanel()');
  await sleep(600);
  const SEL = { 计划: '#ask-bar .q-card.plan', 审批: '#ask-bar .ap-card:nth-of-type(1)', 提问: '#ask-bar .ap-card:nth-of-type(3)' };
  const worst = {};
  for (const [name, sel] of Object.entries(SEL)) {
    const rounds = [];
    for (let round = 1; round <= 5; round++) {
      const f = await tryRead(c, `window.__t(${JSON.stringify(sel + ' .ap-fold-btn')})`, 20000);
      const u = await tryRead(c, `window.__t(${JSON.stringify(sel + ' .ap-fold')})`, 20000);
      const st = await tryRead(c, `(() => ({ folded: document.querySelector(${JSON.stringify(sel)}).classList.contains('folded'),
        h: Math.round(document.querySelector(${JSON.stringify(sel)}).getBoundingClientRect().height) }))()`, 15000);
      rounds.push({ 折叠: f.v && f.v.ms, 展开: u.v && u.v.ms, 展开后: st.v });
    }
    const ms = rounds.map((r) => Math.max(r.折叠 || 0, r.展开 || 0));
    worst[name] = Math.max(...ms);
    console.log(`${name}卡 5 次折叠/展开耗时(ms):`, JSON.stringify(ms), ' 末次状态:', JSON.stringify(rounds[4]));
  }
  console.log('三类卡最大单次耗时(ms):', JSON.stringify(worst),
    ' 全部 < 300ms:', Object.values(worst).every((x) => x < 300));

  // ---- 5) 计划正文完整 + 文件链接仍在（正则改完不能丢 file-link） ----
  const doc = await tryRead(c, `(() => { const d = document.getElementById('plan-doc');
    return { text: d.textContent.length, h2: d.querySelectorAll('h2').length, li: d.querySelectorAll('li').length,
      fileLinks: [...d.querySelectorAll('a.file-link')].map((a) => a.dataset.path),
      hasBegin: d.textContent.indexOf('把日志与快照目录整理干净') >= 0 }; })()`, 20000);
  console.log('计划正文:', JSON.stringify(doc).slice(0, 700));

  // ---- 6) 三个按钮真能点：提交值核对（打桩拦 POST，不发真请求） ----
  await read(c, `(() => { const t = (sel) => { const el = document.querySelector(sel); if (!el) return 'no ' + sel;
    el.click(); return 'clicked'; }; return true; })()`);
  const b1 = await tryRead(c, `(() => { document.getElementById('plan-cancel').click(); return true; })()`, 20000);
  await sleep(400);
  const p1 = await tryRead(c, `JSON.stringify(window.__posts)`, 15000);
  console.log('点「取消计划」:', JSON.stringify(b1).slice(0, 120), ' 提交记录:', String(p1.v).slice(0, 300));
  const b2 = await tryRead(c, `(() => { const el = document.querySelector('#ask-bar .q-card.plan .ap-actions .ok');
    if (!el) return { err: 'no approve btn' }; const t0 = performance.now(); el.click();
    return { ms: +(performance.now() - t0).toFixed(1), text: el.textContent.trim() }; })()`, 20000);
  await sleep(400);
  const p2 = await tryRead(c, `JSON.stringify(window.__posts)`, 15000);
  console.log('点「批准并开始执行」:', JSON.stringify(b2).slice(0, 200), ' 提交记录:', String(p2.v).slice(0, 400));
  const end = await tryRead(c, `(() => ({ counts: { ...window.__lis }, nodes: document.getElementsByTagName('*').length }))()`, 15000);
  console.log('监听器/节点:', JSON.stringify(end).slice(0, 900));
  console.log('console 异常:', logs.length, logs.slice(0, 5));
} finally {
  await closeTab(c);
  try { proc.kill(); } catch (e) {}
}
