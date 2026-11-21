// 计划框交互验证（CDP 真实点击 + 打桩待办数据，隔离实例）。
// 覆盖这次改动：
//   A. 计划审批卡：不再有「查看计划」按钮；动作是 修改 / 取消计划 / 批准并开始执行；
//      整张卡标记为可点（点标题区 → 打开右栏「计划」），点输入框不误触。
//   B. 「取消计划」把 __ignore__ 发给后端，且卡片不做乐观移除（仍留在原处）。
//   C. 右栏默认选中：上次看过/选过旧版（V1）时，**新提交的待审批计划（V2）不被旧版遮住**。
//   D. 对话流里的计划卡（plancard）：没有按钮、只有一行提示、整张卡可点，
//      点击带上这条提交的序号（用于把右栏定位到这一份）。
// 时间限制（计划无时限、提交即落库、重启后仍在等）由 Go 测试覆盖：
//   go test -run Plan .
// 运行：仓库根目录 node gui-test/plan-frame-verify.mjs
import { newTab, read, click, sleep, closeTab } from './cdp.mjs';
import { spawn, execSync } from 'node:child_process';
import { writeFileSync, mkdirSync, accessSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const BASE = 'http://127.0.0.1:8837';
const CDP_HTTP = 'http://127.0.0.1:9223';
const ROOT = fileURLToPath(new URL('..', import.meta.url));
const TMP = join(tmpdir(), 'jx-planframe-' + Date.now().toString(36));
const results = [];
function check(name, pass, detail) {
  results.push({ name, pass, detail });
  console.log((pass ? 'PASS ' : 'FAIL ') + name + (detail === undefined ? '' : '  | ' + JSON.stringify(detail)));
}

async function ensureEdge() {
  try { await fetch(CDP_HTTP + '/json/version'); return; } catch (e) { /* spawn */ }
  const cands = [
    'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
    'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
  ];
  const edge = cands.find((p) => { try { accessSync(p); return true; } catch (e2) { return false; } });
  if (!edge) throw new Error('没找到 msedge.exe');
  spawn(edge, ['--headless=new', '--remote-debugging-port=9223',
    '--user-data-dir=' + join(TMP, 'edge'), 'about:blank'], { stdio: 'ignore' });
  for (let i = 0; i < 30; i++) {
    await sleep(500);
    try { await fetch(CDP_HTTP + '/json/version'); return; } catch (e2) { /* wait */ }
  }
  throw new Error('Edge CDP 没起来');
}

let serverProc = null;
async function ensureServer() {
  try { const r = await fetch(BASE + '/api/status'); if (r.ok) return; } catch (e) { /* spawn */ }
  mkdirSync(join(TMP, 'ws'), { recursive: true });
  const exe = join(TMP, 'server.exe');
  execSync(`go build -o "${exe}" .`, { cwd: ROOT, stdio: 'inherit' });
  writeFileSync(join(TMP, 'config.json'), JSON.stringify({
    port: 8837, bindHost: '127.0.0.1', baseURL: 'mock', apiKey: '', model: 'mock-model',
    workspace: join(TMP, 'ws'), updateURL: '',
  }));
  serverProc = spawn(exe, ['-config', 'config.json', '-noopen'], { cwd: TMP, stdio: 'ignore' });
  for (let i = 0; i < 40; i++) {
    await sleep(500);
    try { const r = await fetch(BASE + '/api/status'); if (r.ok) return; } catch (e) { /* wait */ }
  }
  throw new Error('server 没起来');
}

const PLAN_V2 = '# V2 新计划\n\n这是第二版：先改审批卡，再改右栏选中。';
const PLAN_STREAM = '# 计划 X\n\n这是对话流计划卡要展示的说明段落。';

async function centerOf(c, sel) {
  return await read(c, `(() => {
    const el = document.querySelector(${JSON.stringify(sel)});
    if (!el) return null;
    el.scrollIntoView({ block: 'center' });
    const b = el.getBoundingClientRect();
    return { x: b.left + b.width / 2, y: b.top + b.height / 2 };
  })()`);
}

await ensureEdge();
await ensureServer();

const c = await newTab(BASE + '/index.html', 1440, 900);
try {
  await sleep(1500);

  // 两个真实会话（隔离库里新建），打开第一个
  for (const t of ['计划框验证 A', '计划框验证 B']) {
    await read(c, `(async () => { await fetch('/api/sessions', { method: 'POST',
      headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ title: ${JSON.stringify(t)} }) }); return true; })()`);
    await sleep(200);
  }
  const ids = await read(c, `(async () => { const d = await (await fetch('/api/sessions')).json();
    return (d.sessions || []).map((s) => s.id); })()`);
  if (!ids || ids.length < 2) throw new Error('隔离实例里没建出两个会话');
  const own = ids[0];
  await read(c, `openSession(${JSON.stringify(own)})`);
  await sleep(1500);

  // 打桩待办：一份待审批计划（V2）
  await read(c, `(() => {
    const questions = [
      { id: 'q-v2', sessionId: ${JSON.stringify(own)}, sessionTitle: '', kind: 'plan',
        question: '请审批以下执行计划', plan: ${JSON.stringify(PLAN_V2)} },
    ];
    const json = (body) => Promise.resolve(new Response(JSON.stringify(body),
      { status: 200, headers: { 'Content-Type': 'application/json' } }));
    const real = window.fetch.bind(window);
    window.__realFetch = real;
    window.fetch = (u, o) => {
      const s = String(u);
      if (s.indexOf('/api/approvals') >= 0) return json({ approvals: [] });
      if (s.indexOf('/api/questions') >= 0) return json({ questions });
      return real(u, o);
    };
    return true;
  })()`);

  // 先摆好「上次看过、并在下拉里选过 V1」的右栏状态，再让 V2 到达
  await read(c, `(() => {
    planDocs = [{ seq: 10, title: 'V1 旧计划', markdown: '# V1 旧计划\\n\\n第一版', createdAt: '2026-10-01 10:00:00' }];
    planSel = 'h:10'; planPinned = 'h:10'; planWantSeq = 0; planWantQid = '';
    return true;
  })()`);
  await read(c, `pushQuestion({ questionId: 'q-v2', sessionId: ${JSON.stringify(own)}, kind: 'plan',
    summary: '请审批以下执行计划', plan: ${JSON.stringify(PLAN_V2)} })`);
  await sleep(400);
  const pick = await read(c, `(() => ({ title: planCur && planCur.title, key: planSel,
    options: [...document.querySelectorAll('#plan-pick option')].map((o) => o.textContent) }))()`);
  check('C. 新提交的待审批计划（V2）不被上次看过的旧版遮住', pick.title === 'V2 新计划', pick);

  // A. 审批卡结构与整卡可点
  await read(c, 'if (rpActive === "plan") toggleRpanel()');
  await sleep(700);
  const card = await read(c, `(() => {
    const el = document.querySelector('#ask-bar .q-card.plan');
    if (!el) return null;
    return {
      btns: [...el.querySelectorAll('button')].map((b) => b.textContent.trim()),
      acts: [...el.querySelectorAll('.ap-actions button')].map((b) => b.textContent.trim()),
      clickable: el.classList.contains('clickable'),
      hint: (el.querySelector('.q-plan-hint') || {}).textContent || '',
    };
  })()`);
  check('A1. 审批卡动作是「取消计划 / 批准并开始执行」（修改在输入行）',
    card && card.acts.includes('取消计划') && card.acts.includes('批准并开始执行') && card.btns.includes('修改'), card && card.btns);
  check('A2. 不再有「查看计划」按钮',
    card && !card.btns.some((b) => b.indexOf('查看计划') >= 0), card && card.btns);
  check('A3. 整张卡标记为可点，并写明「除输入框与按钮外都能点」',
    card && card.clickable === true && card.hint.indexOf('都可以点') >= 0, card && { clickable: card.clickable, hint: card.hint });

  const tp = await centerOf(c, '#ask-bar .q-card.plan .q-plan-title');
  await click(c, tp.x, tp.y);
  await sleep(700);
  const op = await read(c, `(() => ({ open: !document.getElementById('rpanel').classList.contains('hidden'),
    active: rpActive, title: planCur && planCur.title }))()`);
  check('A4. 点卡片标题区 → 打开右栏「计划」并显示这一份', op.open && op.active === 'plan' && op.title === 'V2 新计划', op);

  await read(c, 'toggleRpanel()');
  await sleep(700);
  const ip = await centerOf(c, '#ask-bar .q-card.plan .q-free input');
  await click(c, ip.x, ip.y);
  await sleep(400);
  const hidden = await read(c, `document.getElementById('rpanel').classList.contains('hidden')`);
  check('A5. 点输入框不会误触打开右栏', hidden === true, { rpanelHidden: hidden });

  // B. 取消计划：发 __ignore__、卡片留在原处
  await read(c, `(() => {
    window.__ignored = [];
    const real = window.__realFetch;
    window.fetch = (u, o) => {
      const s = String(u);
      if (/\\/api\\/questions\\/[^/]+$/.test(s) && o && o.method === 'POST') {
        window.__ignored.push({ url: s, body: String(o.body) });
        return Promise.resolve(new Response(JSON.stringify({ ok: '1' }),
          { status: 200, headers: { 'Content-Type': 'application/json' } }));
      }
      return real(u, o);
    };
    return true;
  })()`);
  const ig = await centerOf(c, '#ask-bar .q-card.plan .ap-actions button.ghost');
  await click(c, ig.x, ig.y);
  await sleep(600);
  const igr = await read(c, `(() => ({
    calls: window.__ignored || [],
    stillPending: state.questions.some((q) => q.id === 'q-v2'),
    cardThere: !!document.querySelector('#ask-bar .q-card.plan'),
  }))()`);
  check('B1. 「取消计划」把 __ignore__ 发给后端',
    igr.calls.length === 1 && igr.calls[0].body.indexOf('__ignore__') >= 0, igr.calls);
  check('B2. 「取消计划」后卡片不消失（计划仍留着待处置）', igr.stillPending === true && igr.cardThere === true,
    { stillPending: igr.stillPending, cardThere: igr.cardThere });

  // D. 对话流计划卡：无按钮、整张卡可点、点开带定位
  const built = await read(c, `(() => {
    const tc = { id: 'call-1', function: { name: 'exit_plan_mode', arguments: JSON.stringify({ plan: ${JSON.stringify(PLAN_STREAM)} }) } };
    const card = planCardRow(tc, 42, { sessionId: state.cur.id });
    document.querySelector('#messages .msg-col').appendChild(card);
    return { cls: card.className, seq: card.dataset.seq, text: card.textContent,
      btns: [...card.querySelectorAll('button')].map((b) => b.textContent.trim()) };
  })()`);
  check('D1. 对话流计划卡没有按钮，只有一行「点击这张卡看计划全文」',
    built.btns.length === 0 && built.text.indexOf('点击这张卡看计划全文') >= 0, built);
  check('D2. 对话流计划卡带上这条提交的序号（定位用）', built.seq === '42', { seq: built.seq });

  await read(c, 'if (rpActive === "plan") toggleRpanel()');
  await sleep(600);
  const pc = await centerOf(c, '#messages .plan-card');
  await click(c, pc.x, pc.y);
  await sleep(700);
  const pst = await read(c, `(() => ({ open: !document.getElementById('rpanel').classList.contains('hidden'),
    active: rpActive, wantSeq: planWantSeq }))()`);
  check('D3. 点对话流计划卡 → 打开右栏，并把定位序号（42）交给计划视图',
    pst.open && pst.active === 'plan' && pst.wantSeq === 42, pst);

  // D4. 计划卡上的「待审批」徽章随待办消失而撤掉（refreshPlanCards）
  const badge1 = await read(c, `(() => { refreshPlanCards();
    return !!document.querySelector('#messages .plan-card .pc-badge'); })()`);
  await read(c, `(() => { state.questions = []; renderPanels(); return true; })()`);
  await sleep(200);
  const badge2 = await read(c, `!!document.querySelector('#messages .plan-card .pc-badge')`);
  check('D4. 待审批徽章随待办变化翻新（有 → 无）', badge1 === true && badge2 === false,
    { before: badge1, after: badge2 });

  const failed = results.filter((r) => !r.pass);
  console.log('\n' + (failed.length ? ('FAILED ' + failed.length + '/' + results.length) : ('ALL PASS ' + results.length)));
  for (const f of failed) console.log('  ✗ ' + f.name);
  process.exitCode = failed.length ? 1 : 0;
} finally {
  await closeTab(c);
  if (serverProc) { try { serverProc.kill(); } catch (e) { /* 忽略 */ } }
}
