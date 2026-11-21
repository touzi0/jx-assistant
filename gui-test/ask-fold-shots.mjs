// 在 1440×900 的 headless 标签里打开同一个隔离实例（8853），打桩三张卡，
// 截三张图：展开态 / 点开计划全文 / 折叠成一行。只为了留证据图。
import { newTab, read, sleep, shot, closeTab } from './cdp.mjs';
import { mkdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = fileURLToPath(new URL('..', import.meta.url));
const SHOTS = join(ROOT, '.jxai', 'tmp', 'askfold', 'shots');
mkdirSync(SHOTS, { recursive: true });
const BASE = 'http://127.0.0.1:8853';

const c = await newTab(BASE + '/index.html', 1440, 900);
try {
  await sleep(1500);
  const r = await read(c, `(async () => {
    const PLAN = ['# 把日志与快照目录整理干净', '',
      '## 现状', '',
      '- 快照堆在 data/snapshots/2026/11/18/askfold/shots/repro 下面，按会话分目录；',
      '- 日志在 logs/2026/11/18/run/main/stdout/stderr 里滚；',
      '- 归档目标 a/b/c/d/e/f/g/h/i/j/k/l/m/n/o/p/q/r/s/t/u/v/w/x/y/z（先建目录再说）；',
      '- 临时目录 tmp/upload/parts/2026/11/18/chunk/0 里还有残片。', '',
      '## 步骤', '', '1. 先停服务、确认没有在写的分片；', '2. 归档 7 天前的快照到 archive/snapshots/2026/11/old；',
      '3. 清掉 tmp/upload/parts 下的残片，重建分片索引；',
      '4. 重启后核对 data/store.go 与 web/app-chat.js 的行为没变。'].join('\\n');
    const json = (b) => Promise.resolve(new Response(JSON.stringify(b), { status: 200, headers: { 'Content-Type': 'application/json' } }));
    const real = window.fetch.bind(window);
    window.__t0 = performance.now();
    const fx = {
      approvals: [{ id: 'ap-demo', sessionId: 'x', tool: 'run_command', summary: 'powershell -Command "Get-ChildItem D:\\\\logs | Remove-Item -Recurse"', reason: '清理临时日志' }],
      questions: [{ id: 'q-plan', kind: 'plan', question: '请审批以下执行计划', plan: PLAN },
                  { id: 'q-ask', kind: 'ask', question: '要保留哪几份历史日志？', options: ['只留最近 3 天', '留最近 7 天'], deadlineMs: Date.now() + 300000 }],
    };
    window.fetch = (u, o) => {
      const s = String(u);
      if (/\\/api\\/(approvals|questions)\\/[^/]+$/.test(s)) return json({ ok: 1 });
      if (s.indexOf('/api/approvals') >= 0) return json({ approvals: fx.approvals });
      if (s.indexOf('/api/questions') >= 0) return json({ questions: fx.questions });
      return real(u, o);
    };
    await real('/api/sessions', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ title: '折叠 / 计划全文' }) });
    const d = await (await real('/api/sessions')).json();
    const sid = (d.sessions || [])[0].id;
    fx.approvals[0].sessionId = sid; fx.questions.forEach((q) => q.sessionId = sid);
    openSession(sid);
    await new Promise((r2) => setTimeout(r2, 1200));
    if (typeof closeWizard === 'function') closeWizard();
    await new Promise((r2) => setTimeout(r2, 300));
    const t0 = performance.now();
    await loadApprovals(); await loadQuestions();
    const ms = Math.round(performance.now() - t0);
    await new Promise((r2) => setTimeout(r2, 900));
    return { sid, loadMs: ms, cards: [...document.querySelectorAll('#ask-bar .ap-card')].map((x) => x.className) };
  })()`);
  console.log('页面状态:', JSON.stringify(r));
  await shot(c, join(SHOTS, 'plan-fold-1-expanded.png'));

  const open = await read(c, `(() => { const t0 = performance.now();
    document.querySelector('#ask-bar .q-card.plan').click();
    return { syncMs: Math.round(performance.now() - t0) }; })()`);
  await sleep(1200);
  const st = await read(c, `(() => ({ panel: !document.getElementById('rpanel').classList.contains('hidden'),
    act: document.getElementById('plan-act').classList.contains('hidden') ? 'hidden' : 'shown',
    textLen: (document.getElementById('plan-doc') || {}).textContent.length,
    fileLinks: [...document.querySelectorAll('#plan-doc a.file-link')].map((a) => a.dataset.path),
    cancel: document.getElementById('plan-cancel').textContent.trim(),
    approve: document.getElementById('plan-approve').textContent.trim() }))()`);
  console.log('点计划卡:', JSON.stringify(open), ' 右栏:', JSON.stringify(st));
  await shot(c, join(SHOTS, 'plan-fold-2-planview.png'));

  // 折叠三张卡 → 一行
  await read(c, `(() => { document.querySelectorAll('#ask-bar .ap-fold-btn').forEach((b) => b.click()); return true; })()`);
  await sleep(500);
  const folded = await read(c, `(() => ({ cards: [...document.querySelectorAll('#ask-bar .ap-card')].map((c2) => ({
    folded: c2.classList.contains('folded'), h: Math.round(c2.getBoundingClientRect().height),
    text: (c2.querySelector('.ap-fold') || {}).textContent || '' })) }))()`);
  console.log('折叠后:', JSON.stringify(folded));
  await shot(c, join(SHOTS, 'plan-fold-3-folded.png'));
} finally {
  await closeTab(c);
}
