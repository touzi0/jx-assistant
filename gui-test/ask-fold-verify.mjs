// 询问/审批卡「折叠成一行 + 取消计划文案」验证（CDP 真实点击，两个隔离实例 + 打桩待办）。
//
//   A. 三类卡（计划审批 / 权限审批 / 提问）都有右上角折叠按钮；
//   B. 折叠后只占一行、正文输出区（#messages）可见高度明显变大；
//   C. 折叠态在轮询刷新、签名变化重建卡片、切会话再回来之后都不弹回展开；
//   D. 折叠再展开，输入框里已打的字还在；
//   E. 折叠只影响视觉：倒计时仍走、提交仍发同样的提交值；
//   F. 展开态与改动前（HEAD 版前端）逐个盒模型一致——只多一枚小按钮，不改现状；
//   G. 计划卡按钮文字已从「忽略计划」改为「取消计划」，提交值仍是 __ignore__。
//
// 隔离：临时配置 + 独立 dataDir + 独立端口（8846/8847），不碰用户在跑的实例（8788）。
// 运行：仓库根目录 node gui-test/ask-fold-verify.mjs
import { newTab, read, click, sleep, shot, center, closeTab } from './cdp.mjs';
import { spawn, execSync } from 'node:child_process';
import { writeFileSync, mkdirSync, cpSync, existsSync, accessSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = fileURLToPath(new URL('..', import.meta.url));
const TMP = join(ROOT, '.jxai', 'tmp', 'askfold');
const SHOTS = join(TMP, 'shots');
const CDP_HTTP = 'http://127.0.0.1:9223';
const PORT_BEFORE = 8846;
const PORT_AFTER = 8847;
const results = [];
mkdirSync(SHOTS, { recursive: true });

function check(name, pass, detail) {
  results.push({ name, pass, detail });
  console.log((pass ? 'PASS ' : 'FAIL ') + name + (detail === undefined ? '' : '  | ' + JSON.stringify(detail).slice(0, 600)));
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
  for (let i = 0; i < 40; i++) {
    await sleep(500);
    try { await fetch(CDP_HTTP + '/json/version'); return; } catch (e2) { /* wait */ }
  }
  throw new Error('Edge CDP 没起来');
}

// 隔离实例：独立 config（mock 模型）+ 独立 dataDir/workspace + 独立端口
async function startServer(tag, port, webdir) {
  mkdirSync(join(TMP, tag), { recursive: true });
  const cfg = {
    port, bindHost: '127.0.0.1', baseURL: 'mock', apiKey: '', model: 'mock-model',
    workspace: join(TMP, tag, 'ws'), dataDir: join(TMP, tag, 'data'),
    skillsDir: join(TMP, tag, 'skills'), allowShell: false, marketURL: '', searchURL: '',
  };
  writeFileSync(join(TMP, tag, 'config.json'), JSON.stringify(cfg));
  const proc = spawn(join(TMP, 'server.exe'), ['-config', 'config.json', '-noopen', '-webdir', webdir],
    { cwd: join(TMP, tag), stdio: 'ignore' });
  const BASE = 'http://127.0.0.1:' + port;
  for (let i = 0; i < 60; i++) {
    await sleep(400);
    try { const r = await fetch(BASE + '/api/status'); if (r.ok) return { proc, BASE }; } catch (e) { /* wait */ }
  }
  throw new Error('server(' + tag + ') 没起来');
}

// 改动前的前端：整份 web/ 拷一份，再把本次改过的三个文件换回 HEAD 版
function prepareBeforeWeb() {
  const dir = join(TMP, 'web-before');
  cpSync(join(ROOT, 'web'), dir, { recursive: true });
  for (const f of ['app-chat.js', 'style.css', 'index.html']) {
    const buf = execSync('git show HEAD:web/' + f, { cwd: ROOT, maxBuffer: 64 * 1024 * 1024 });
    writeFileSync(join(dir, f), buf);
  }
  return dir;
}

function buildServerIfNeeded() {
  const exe = join(TMP, 'server.exe');
  mkdirSync(join(TMP, 'gocache'), { recursive: true });
  mkdirSync(join(TMP, 'gotmp'), { recursive: true });
  const env = { ...process.env,
    GOCACHE: join(TMP, 'gocache'), GOTMPDIR: join(TMP, 'gotmp') };
  execSync('go build -o "' + exe + '" .', { cwd: ROOT, stdio: 'inherit', env });
  return exe;
}

const PLAN_MD = '# 把日志清理一下\n\n先把 7 天前的日志归档到 D:\\logs\\archive，再删掉临时目录，最后重启服务验证。';

// 打桩：/api/approvals 与 /api/questions 一律返回这三条待办（不碰后端接口语义）
function stubExpr(sid) {
  const fx = {
    approvals: [{
      id: 'ap-demo', sessionId: sid, sessionTitle: '', tool: 'run_command',
      summary: 'powershell -Command "Get-ChildItem D:\\logs | Remove-Item -Recurse"',
      reason: '要清理临时日志目录（隔离实例里只是打桩，不会真执行）', createdAt: '',
    }],
    questions: [
      { id: 'q-demo-plan', sessionId: sid, sessionTitle: '', kind: 'plan',
        question: '请审批以下执行计划', plan: PLAN_MD },
      { id: 'q-demo-ask', sessionId: sid, sessionTitle: '', kind: 'ask',
        question: '要保留哪几份历史日志？', options: ['只留最近 3 天', '留最近 7 天', '全部保留'],
        deadlineMs: Date.now() + 125000, timerHeld: false, noTimeout: false },
    ],
  };
  return `(() => {
    window.__fx = ${JSON.stringify(fx)};
    window.__posts = [];
    const json = (b) => Promise.resolve(new Response(JSON.stringify(b),
      { status: 200, headers: { 'Content-Type': 'application/json' } }));
    const real = window.fetch.bind(window);
    window.__realFetch = real;
    window.__stub = () => {
      window.fetch = (u, o) => {
        const s = String(u);
        if (/\\/api\\/(approvals|questions)\\/[^/]+$/.test(s)) {
          window.__posts.push({ url: s, body: (o && o.body) ? String(o.body) : '' });
          return json({ ok: '1' });
        }
        if (s.indexOf('/api/approvals') >= 0) return json({ approvals: window.__fx.approvals });
        if (s.indexOf('/api/questions') >= 0) return json({ questions: window.__fx.questions });
        return real(u, o);
      };
    };
    window.__stub();
    return true;
  })()`;
}

const MEASURE = `(() => {
  const bar = document.getElementById('ask-bar');
  if (!bar) return null;
  const box = (el) => { const b = el.getBoundingClientRect();
    return { y: Math.round(b.y - bar.getBoundingClientRect().y), w: Math.round(b.width), h: Math.round(b.height) }; };
  const cards = [...bar.querySelectorAll('.ap-card')];
  const msgRect = document.getElementById('messages').getBoundingClientRect();
  const barRect = bar.getBoundingClientRect();
  return {
    // 输入区是绝对定位浮在对话区之上的：正文「看得见」的高度 = 输入区（含询问带）顶到消息区顶
    visH: Math.round(barRect.top - msgRect.top),
    msgH: document.getElementById('messages').clientHeight,
    barH: Math.round(barRect.height),
    cards: cards.map((c) => {
      const fold = c.querySelector('.ap-fold');
      const head = c.querySelector('.ap-head');
      const body = c.querySelector('.ap-body');
      const free = c.querySelector('.q-free');
      const acts = c.querySelector('.ap-actions');
      return {
        cls: c.className, h: Math.round(c.getBoundingClientRect().height),
        head: head ? box(head) : null, body: body ? box(body) : null,
        free: free ? box(free) : null, acts: acts ? box(acts) : null,
        hasFoldBtn: !!c.querySelector('.ap-fold-btn'),
        count: (c.querySelector('.ap-head .q-count') || {}).textContent || '',
        folded: c.classList.contains('folded'),
        foldDisplay: fold ? getComputedStyle(fold).display : 'none',
        foldH: fold ? Math.round(fold.getBoundingClientRect().height) : 0,
        foldText: fold ? fold.textContent.trim() : '',
        foldCount: (fold && fold.querySelector('.q-count')) ? fold.querySelector('.q-count').textContent.trim() : '',
      };
    }),
  };
})()`;

// 打开某实例的页面 → 建两个会话 → 打桩待办 → 渲染
async function openAndLoad(BASE) {
  const c = await newTab(BASE + '/index.html', 1440, 900);
  await sleep(1500);
  for (const t of ['折叠验证 A', '折叠验证 B']) {
    await read(c, `(async () => { await fetch('/api/sessions', { method: 'POST',
      headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ title: ${JSON.stringify(t)} }) }); return true; })()`);
    await sleep(200);
  }
  const ids = await read(c, `(async () => { const d = await (await fetch('/api/sessions')).json();
    return (d.sessions || []).map((s) => s.id); })()`);
  if (!ids || ids.length < 2) throw new Error('隔离实例里没建出两个会话');
  const own = ids[0];
  await read(c, `openSession(${JSON.stringify(own)})`);
  await sleep(1200);
  await read(c, 'if (typeof closeWizard === "function") closeWizard();'); // 隔离实例首启会弹接入向导，先关掉
  await sleep(300);
  await read(c, stubExpr(own));
  await read(c, '(async () => { await loadApprovals(); await loadQuestions(); return true; })()');
  await sleep(500);
  await read(c, 'if (rpActive) toggleRpanel()'); // 计划自动铺开的右栏收起，量宽度用
  await sleep(600);
  return { c, own, other: ids[1] };
}

await ensureEdge();
buildServerIfNeeded();
const beforeWeb = prepareBeforeWeb();

const before = await startServer('before', PORT_BEFORE, beforeWeb);
let tabB = null;
let tabA = null;
let after = null;
try {
  // ---------- 改动前（HEAD 版前端）：基线几何 + 截图 ----------
  tabB = await openAndLoad(before.BASE);
  const mB = await read(tabB.c, MEASURE);
  check('0. 改动前：三张卡都渲染出来了（计划 / 权限 / 提问）', mB && mB.cards.length === 3, mB && mB.cards.map((x) => x.cls));
  await shot(tabB.c, join(SHOTS, '0-before-expanded.png'));
  await closeTab(tabB.c);
  tabB = null;
  before.proc.kill();

  // ---------- 改动后：同一个桩，先量展开态 ----------
  after = await startServer('after', PORT_AFTER, join(ROOT, 'web'));
  tabA = await openAndLoad(after.BASE);
  const mA = await read(tabA.c, MEASURE);
  await shot(tabA.c, join(SHOTS, '1-after-expanded.png'));

  check('A. 三类卡都有右上角折叠按钮，默认都不折叠',
    mA && mA.cards.length === 3 && mA.cards.every((x) => x.hasFoldBtn && !x.folded),
    mA && mA.cards.map((x) => ({ cls: x.cls, btn: x.hasFoldBtn, folded: x.folded })));
  const same = (a, b) => !!a && !!b && a.w === b.w && a.h === b.h;
  const geom = (m) => m && m.cards.map((x) => ({ h: x.h,
    head: !!x.head && x.head.h, body: !!x.body && x.body.h, free: !!x.free && x.free.h, acts: !!x.acts && x.acts.h }));
  const gB = geom(mB);
  const gA = geom(mA);
  check('F1. 展开态：卡片总高与各区块高与改动前一致（只多一枚小按钮，不改现状）',
    JSON.stringify(gB) === JSON.stringify(gA), { before: gB, after: gA });
  check('F2. 展开态：正文看得见的高度与改动前一致（输入区是浮层，比的是没被遮挡的部分）',
    mB && mA && mB.visH === mA.visH, { before: mB && mB.visH, after: mA && mA.visH });

  // ---------- 折叠三张卡 ----------
  for (let i = 0; i < 3; i++) {
    const p = await center(tabA.c, `.ap-card:nth-of-type(${i + 1}) .ap-fold-btn`);
    await click(tabA.c, p.x, p.y);
  }
  await sleep(400);
  const mF = await read(tabA.c, MEASURE);
  await shot(tabA.c, join(SHOTS, '2-after-folded.png'));
  check('B1. 折叠后三张卡都只剩一行（高度 ≤ 36px，折叠行可见、卡片其余内容不显示）',
    mF && mF.cards.every((x) => x.folded && x.h <= 36 && x.foldH > 0 && x.foldH <= 30),
    mF && mF.cards.map((x) => ({ h: x.h, foldH: x.foldH, txt: x.foldText })));
  check('B2. 折叠行写清「类型 + 摘要 + 点击展开」',
    mF && mF.cards[0].foldText.indexOf('需要批准') >= 0 && mF.cards[1].foldText.indexOf('计划待审批') >= 0
      && mF.cards[2].foldText.indexOf('助手提问') >= 0 && mF.cards.every((x) => x.foldText.indexOf('点击展开') >= 0),
    mF && mF.cards.map((x) => x.foldText));
  check('B3. 折叠后正文看得见的高度明显变大（≥ 100px）',
    mA && mF && mF.visH - mA.visH >= 100, { expanded: mA && mA.visH, folded: mF && mF.visH, 腾出: mA && mF && mF.visH - mA.visH });
  check('B4. 提问卡倒计时在折叠前就在走、折叠后也照样显示在折叠行里（折叠不影响计时）',
    mA && /\d+:\d\d/.test(mA.cards[2].count) && mF && /\d+:\d\d|∞/.test(mF.cards[2].foldCount),
    { 折叠前: mA && mA.cards[2].count, 折叠后: mF && mF.cards[2].foldCount });

  // ---------- 轮询刷新不弹回 ----------
  await read(tabA.c, '(async () => { await loadApprovals(); await loadQuestions(); return true; })()');
  await sleep(300);
  await read(tabA.c, '(async () => { await loadApprovals(); await loadQuestions(); return true; })()');
  await sleep(300);
  const mPoll = await read(tabA.c, MEASURE);
  check('C1. 轮询/事件刷新后仍是折叠态（不弹回展开）',
    mPoll && mPoll.cards.every((x) => x.folded && x.h <= 36), mPoll && mPoll.cards.map((x) => x.folded));

  // ---------- 签名变化（内容变了要重建节点）也不弹回 ----------
  await read(tabA.c, `(() => {
    window.__fx.questions[1].question = '要保留哪几份历史日志？（已刷新）';
    return true; })()`);
  await read(tabA.c, '(async () => { await loadQuestions(); return true; })()');
  await sleep(400);
  const mSig = await read(tabA.c, MEASURE);
  const rebuilt = await read(tabA.c, `(() => {
    const el = document.querySelectorAll('#ask-bar .ap-card')[2];
    return { folded: el.classList.contains('folded'), text: (el.querySelector('.ap-body') || {}).textContent || '',
      fresh: el.dataset.fresh || '' }; })()`);
  check('C2. 卡片内容变化被重建后，折叠状态按 id 回放（仍折叠，不弹回）',
    mSig && mSig.cards.every((x) => x.folded) && rebuilt.folded === true && rebuilt.text.indexOf('已刷新') >= 0, rebuilt);

  // ---------- 切会话再回来 ----------
  await read(tabA.c, `openSession(${JSON.stringify(tabA.other)})`);
  await sleep(900);
  const otherBar = await read(tabA.c, `(() => { const b = document.getElementById('ask-bar');
    return { hidden: b.classList.contains('hidden'), n: b.querySelectorAll('.ap-card').length }; })()`);
  await read(tabA.c, `openSession(${JSON.stringify(tabA.own)})`);
  await sleep(900);
  const mBack = await read(tabA.c, MEASURE);
  check('C3. 切到别的对话再切回来，折叠状态仍在',
    otherBar.hidden === true && mBack && mBack.cards.length === 3 && mBack.cards.every((x) => x.folded && x.h <= 36),
    { otherBar, cards: mBack && mBack.cards.map((x) => x.folded) });

  // ---------- 全部展开：几何回到改动前的样子（这一项要在后面点掉卡片之前做） ----------
  await read(tabA.c, `(() => { document.querySelectorAll('#ask-bar .ap-fold').forEach((b) => b.click()); return true; })()`);
  await sleep(400);
  const mU = await read(tabA.c, MEASURE);
  const gU = geom(mU);
  check('4. 再全部展开：几何回到改动前的样子（看得见的高度也回到基线，按钮不顶高标题行）',
    JSON.stringify(gU) === JSON.stringify(gB) && mU && mB && mU.visH === mB.visH,
    { before: gB, after: gU, visH: [mB && mB.visH, mU && mU.visH] });
  await shot(tabA.c, join(SHOTS, '3-after-unfolded.png'));

  // ---------- 输入框内容：折叠 → 展开还在；折叠状态下重建也在 ----------
  const inp = await center(tabA.c, '#ask-bar .q-card.plan .q-free input');
  await click(tabA.c, inp.x, inp.y);
  await sleep(150);
  await read(tabA.c, `(() => { const i = document.querySelector('#ask-bar .q-card.plan .q-free input');
    i.value = '先只清 7 天前的'; i.dispatchEvent(new Event('input', { bubbles: true })); return i.value; })()`);
  const f4 = await center(tabA.c, '#ask-bar .q-card.plan .ap-fold-btn');
  await click(tabA.c, f4.x, f4.y); // 折叠
  await sleep(200);
  const foldedNow = await read(tabA.c, `document.querySelector('#ask-bar .q-card.plan').classList.contains('folded')`);
  const f3 = await center(tabA.c, '#ask-bar .q-card.plan .ap-fold');
  await click(tabA.c, f3.x, f3.y); // 展开（点折叠行）
  await sleep(200);
  const v1 = await read(tabA.c, `document.querySelector('#ask-bar .q-card.plan .q-free input').value`);
  const f4b = await center(tabA.c, '#ask-bar .q-card.plan .ap-fold-btn');
  await click(tabA.c, f4b.x, f4b.y); // 再折叠
  await sleep(200);
  await read(tabA.c, '(async () => { await loadQuestions(); return true; })()'); // 轮询刷新（节点会按签名重建）
  await sleep(300);
  const stillFolded = await read(tabA.c, `document.querySelector('#ask-bar .q-card.plan').classList.contains('folded')`);
  const f5 = await center(tabA.c, '#ask-bar .q-card.plan .ap-fold');
  await click(tabA.c, f5.x, f5.y); // 再展开
  await sleep(200);
  const v2 = await read(tabA.c, `document.querySelector('#ask-bar .q-card.plan .q-free input').value`);
  check('D. 折叠再展开后输入框里已打的字还在（折叠前后、跨一次刷新重建都在）',
    foldedNow === true && stillFolded === true && v1 === '先只清 7 天前的' && v2 === '先只清 7 天前的',
    { 折叠成功: foldedNow, 刷新后仍折叠: stillFolded, 展开后: v1, 折叠并刷新后再展开: v2 });

  // ---------- 文案与提交值 ----------
  const copy = await read(tabA.c, `(() => {
    const plan = document.querySelector('#ask-bar .q-card.plan');
    return { acts: [...plan.querySelectorAll('.ap-actions button')].map((b) => b.textContent.trim()),
      svc: (document.getElementById('plan-cancel') || {}).textContent || '',
      svcBound: typeof (document.getElementById('plan-cancel') || {}).onclick,
      old: !!document.getElementById('plan-ignore') }; })()`);
  check('G1. 计划卡按钮已是「取消计划」，不再有「忽略计划」；右栏计划页同名按钮也已改',
    copy.acts.indexOf('取消计划') >= 0 && copy.acts.indexOf('忽略计划') < 0
      && copy.svc.trim() === '取消计划' && copy.svcBound === 'function' && copy.old === false, copy);

  await read(tabA.c, 'window.__posts = []');
  const ig = await center(tabA.c, '#ask-bar .q-card.plan .ap-actions button.ghost');
  await click(tabA.c, ig.x, ig.y);
  await sleep(500);
  const igr = await read(tabA.c, `(() => ({ posts: window.__posts,
    stillPending: state.questions.some((q) => q.id === 'q-demo-plan'),
    cardThere: !!document.querySelector('#ask-bar .q-card.plan') }))()`);
  check('G2. 点「取消计划」提交值仍是 __ignore__（后端接口语义未变），计划仍留着',
    igr.posts.length === 1 && igr.posts[0].body.indexOf('__ignore__') >= 0 && igr.stillPending && igr.cardThere, igr);

  // ---------- 折叠只影响视觉：审批卡「允许」照样能提交 ----------
  await read(tabA.c, 'window.__posts = []');
  const allow = await center(tabA.c, '#ask-bar .ap-card:nth-of-type(1) .ap-actions button.ok');
  await click(tabA.c, allow.x, allow.y);
  await sleep(500);
  const apr = await read(tabA.c, 'window.__posts');
  check('E. 展开后的审批卡按钮照常提交（approved 走原接口）',
    apr && apr.length === 1 && apr[0].body.indexOf('"approved":true') >= 0, apr);

  const failed = results.filter((r) => !r.pass);
  console.log('\n' + (failed.length ? ('FAILED ' + failed.length + '/' + results.length) : ('ALL PASS ' + results.length)));
  for (const f of failed) console.log('  ✗ ' + f.name);
  process.exitCode = failed.length ? 1 : 0;
} finally {
  if (tabB) await closeTab(tabB.c);
  if (tabA) await closeTab(tabA.c);
  try { before.proc.kill(); } catch (e) { /* 已退出 */ }
  if (after) { try { after.proc.kill(); } catch (e) { /* 已退出 */ } }
}
