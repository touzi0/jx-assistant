// 对话区 Markdown 渲染性能对比：旧自研正则版（冻结快照，函数名加 L 后缀）vs
// marked v12 词法+白名单渲染。
// 场景：长文档（几千字、含表格与代码块）、44KB 超长文档（超 32KB 流式降频档）、
// 流式增量（前缀重渲染）、长对话（多条小消息）。
// 用法：node gui-test/md-perf-test.js
'use strict';
const { readFileSync } = require('fs');
const pathMod = require('path');
const vm = require('vm');

/* ---------- 旧实现快照（换 marked 前的 esc/inlineFmt/renderBlocks/md，逻辑逐字冻结，仅改名避免冲突） ---------- */
function escL(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}
function inlineFmtL(s) {
  const codes = [];
  let t = s.replace(/`([^`\n]+)`/g, (_, c) => { codes.push(c); return '\x00' + (codes.length - 1) + '\x00'; });
  const linkRe = /\[([^\]\n]+)\]\((https?:\/\/[^\s)]+)\)|(https?:\/\/[A-Za-z0-9\-._~:\/?#\[\]@!$&'*+,;=%]+)/g;
  let out = '', last = 0, m;
  while ((m = linkRe.exec(t))) {
    out += escL(t.slice(last, m.index));
    const href = m[1] !== undefined ? m[2] : m[0];
    const text = m[1] !== undefined ? m[1] : m[0];
    out += '<a class="ext-link" href="' + escL(href) + '" rel="noopener">' + escL(text) + '</a>';
    last = m.index + m[0].length;
  }
  out += escL(t.slice(last));
  out = out
    .replace(/\x00(\d+)\x00/g, (_, i) => { const c = codes[+i]; return '<code>' + escL(c) + '</code>'; })
    .replace(/\*\*([^*\n]+)\*\*/g, '<b>$1</b>');
  return out;
}
function renderBlocksL(seg) {
  const lines = seg.split(/\r?\n/); const out = []; let i = 0;
  const isTableSep = (l) => /^\s*\|[\s:|-]+\|\s*$/.test(l);
  const isUl = (l) => /^\s*[-*]\s+\S/.test(l);
  const isOl = (l) => /^\s*\d+[.、]\s+\S/.test(l);
  const parseRow = (l) => l.trim().replace(/^\||\|$/g, '').split('|').map((c) => c.trim());
  while (i < lines.length) {
    const line = lines[i]; if (!line.trim()) { i++; continue; }
    const h = line.match(/^(#{1,6})\s+(.*)$/);
    if (h) { const lv = Math.min(h[1].length + 1, 5); out.push('<h' + lv + '>' + inlineFmtL(h[2]) + '</h' + lv + '>'); i++; continue; }
    if (/^\s*\|.+\|\s*$/.test(line) && i + 1 < lines.length && isTableSep(lines[i + 1])) {
      const head = parseRow(line); i += 2; const rows = [];
      while (i < lines.length && /^\s*\|.+\|\s*$/.test(lines[i])) rows.push(parseRow(lines[i++]));
      let t = '<table><thead><tr>' + head.map((c) => '<th>' + inlineFmtL(c) + '</th>').join('') + '</tr></thead><tbody>';
      for (const r of rows) t += '<tr>' + r.map((c) => '<td>' + inlineFmtL(c) + '</td>').join('') + '</tr>';
      out.push(t + '</tbody></table>'); continue;
    }
    if (isUl(line) || isOl(line)) {
      const ol = isOl(line) && !isUl(line); const items = [];
      while (i < lines.length && (ol ? isOl(lines[i]) : isUl(lines[i]))) { items.push(lines[i].replace(ol ? /^\s*\d+[.、]\s+/ : /^\s*[-*]\s+/, '')); i++; }
      out.push((ol ? '<ol>' : '<ul>') + items.map((it) => '<li>' + inlineFmtL(it) + '</li>').join('') + (ol ? '</ol>' : '</ul>')); continue;
    }
    const para = [];
    while (i < lines.length && lines[i].trim() && !/^#{1,6}\s/.test(lines[i]) && !/^\s*\|/.test(lines[i]) && !isUl(lines[i]) && !isOl(lines[i])) { para.push(lines[i]); i++; }
    if (para.length) out.push('<p>' + inlineFmtL(para.join('\n')).replace(/\n/g, '<br>') + '</p>');
    else { out.push('<p>' + inlineFmtL(line) + '</p>'); i++; }
  }
  return out.join('');
}
function mdL(text) {
  const parts = String(text ?? '').split('```'); let html = '';
  parts.forEach((part, i) => {
    if (i % 2 === 1) { const code = part.replace(/^[a-zA-Z0-9_#+-]*\r?\n/, ''); html += '<pre><code>' + escL(code) + '</code></pre>'; }
    else if (part.trim()) { html += renderBlocksL(part); }
  });
  return html;
}

/* ---------- 新实现：从 web/app-core.js 抽取 + marked.lib.js ---------- */
const root = pathMod.join(__dirname, '..');
const src = readFileSync(pathMod.join(root, 'web', 'app-core.js'), 'utf8');
const grab = (re) => { const m = src.match(re); if (!m) throw new Error('not found: ' + re); return m[0]; };
const escFn = grab(/function esc\(s\) \{[\s\S]*?\n\}/);
const mdSec = grab(/\/\* ---------- Markdown 渲染[\s\S]*?\n\}(?=\n\nfunction relTime)/);
const sb = {};
vm.createContext(sb);
vm.runInContext(readFileSync(pathMod.join(root, 'web', 'marked.lib.js'), 'utf8'), sb);
vm.runInContext(escFn + '\n' + mdSec + '\nthis.mdN = md;', sb);
const mdNew = sb.mdN, mdOld = mdL;

/* ---------- 样例数据 ---------- */
function buildDoc(repeats) {
  const block = [
    '## 性能测试章节标题',
    '',
    '这是一段普通正文，包含 **粗体**、行内代码 `config.json`、文件路径 web/index.html 与链接 https://example.com/a?b=1 的混排。',
    '中文标点吞字测试：见 https://x.com/a。然后继续下一句。',
    '',
    '| 列一 | 列二 | 列三 |',
    '|:-----|:----:|-----:|',
    '| 左对齐 | 居中 | 右对齐 |',
    '| 11 | 22 | 33 |',
    '| 数据行 A | 数据行 B | 数据行 C |',
    '',
    '- 列表项一（含 `path/to/file.go` 路径）',
    '- 列表项二',
    '  - 嵌套项',
    '- 列表项三',
    '',
    '```go',
    'func main() {',
    '    fmt.Println("hello 性能")',
    '    // 注释行 > 引用样式 --- 分隔线',
    '}',
    '```',
    '',
    '> 引用块一行',
    '> 引用块二行',
    '',
    '1、 第一步',
    '2、 第二步',
    '',
  ].join('\n');
  return Array.from({ length: repeats }, () => block).join('\n');
}
const docLong = buildDoc(25);        // ≈ 8.5KB
const docHuge = buildDoc(130);       // ≈ 44KB（超过 32KB 流式降频档）
const docChat = Array.from({ length: 300 }, (_, i) =>
  '消息 ' + i + '：正文一段 **粗体** `code`，路径 src/main' + i + '.go，链接 https://x.com/' + i + '。中文。').join('\n\n');

/* ---------- 计时 ---------- */
function bench(fn, arg, iters, warmup) {
  for (let i = 0; i < warmup; i++) fn(arg);
  const t0 = process.hrtime.bigint();
  for (let i = 0; i < iters; i++) fn(arg);
  const t1 = process.hrtime.bigint();
  return Number(t1 - t0) / 1e6 / iters; // ms/次
}
function report(name, doc, iters) {
  const o = bench(mdOld, doc, iters, 3);
  const n = bench(mdNew, doc, iters, 3);
  console.log(`${name.padEnd(22, ' ')} 旧 ${o.toFixed(2)}ms   新 ${n.toFixed(2)}ms   比值 ${(n / o).toFixed(2)}x`);
  return { o, n };
}
console.log('文档长度：long=' + docLong.length + 'B, huge=' + docHuge.length + 'B, chat=' + docChat.length + 'B\n');
report('长文档 ~8.5KB', docLong, 200);
report('超长文档 ~44KB', docHuge, 40);

// 流式增量：把 long 文档按 150 帧切块，逐步渲染前缀（模拟逐 token 到达时的全文重渲染）
function streamTotal(md2, doc) {
  const step = Math.ceil(doc.length / 150);
  const t0 = process.hrtime.bigint();
  let last = null;
  for (let end = step; end <= doc.length + step; end += step) {
    last = md2(doc.slice(0, Math.min(end, doc.length)));
  }
  const t1 = process.hrtime.bigint();
  return { ms: Number(t1 - t0) / 1e6, len: last.length };
}
{
  const so = streamTotal(mdOld, docLong), sn = streamTotal(mdNew, docLong);
  console.log(`流式 150 帧前缀重渲染    旧 ${so.ms.toFixed(1)}ms   新 ${sn.ms.toFixed(1)}ms   （每帧均值 旧 ${(so.ms / 150).toFixed(2)}ms / 新 ${(sn.ms / 150).toFixed(2)}ms）`);
}
// 长对话：300 条小消息逐条渲染
{
  const msgs = docChat.split('\n\n');
  const t0 = process.hrtime.bigint();
  for (const m of msgs) mdOld(m);
  const t1 = process.hrtime.bigint();
  for (const m of msgs) mdNew(m);
  const t2 = process.hrtime.bigint();
  console.log(`长对话 300 条消息       旧 ${(Number(t1 - t0) / 1e6).toFixed(1)}ms   新 ${(Number(t2 - t1) / 1e6).toFixed(1)}ms`);
}
// 输出长度对比（新增语法产出更多标签，长度差是预期内的）
console.log('\n长文档输出长度：旧 ' + mdOld(docLong).length + 'B，新 ' + mdNew(docLong).length + 'B');
