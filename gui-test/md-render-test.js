// 对话区 Markdown 渲染测试（marked v12 词法 + 白名单渲染器，web/app-core.js）。
// 从 app-core.js 原样抽取渲染器全套，与 web/marked.lib.js 一起在 vm 里跑：
//  1) 旧 linkify 用例回归（file-link / ext-link 识别，行为与换渲染器前一致）
//  2) GFM 新语法（斜体/删除线/引用块/任务列表/表格对齐/图片/嵌套列表/分隔线/自动链接）
//  3) XSS：白名单标签断言（输出里不可能出现白名单外的标签）+ 链接协议白名单
//  4) 视觉契约（标题 h2 起步、单换行=br、裸 URL 不吞中文、代码块原样）
// 用法：node gui-test/md-render-test.js
'use strict';
const { readFileSync } = require('fs');
const pathMod = require('path');
const vm = require('vm');

const root = pathMod.join(__dirname, '..');
const src = readFileSync(pathMod.join(root, 'web', 'app-core.js'), 'utf8');
const grab = (re) => { const m = src.match(re); if (!m) throw new Error('not found: ' + re); return m[0]; };
const escFn = grab(/function esc\(s\) \{[\s\S]*?\n\}/);
const mdSec = grab(/\/\* ---------- Markdown 渲染[\s\S]*?\n\}(?=\n\nfunction relTime)/);
const sandbox = {};
vm.createContext(sandbox);
vm.runInContext(readFileSync(pathMod.join(root, 'web', 'marked.lib.js'), 'utf8'), sandbox);
vm.runInContext(escFn + '\n' + mdSec + '\nthis.md = md;', sandbox);
const md = sandbox.md;

let pass = 0, fail = 0;
const ok = (cond, label, extra) => {
  if (cond) { pass++; }
  else { fail++; console.log('FAIL', label, extra !== undefined ? ':: ' + JSON.stringify(extra) : ''); }
};
const S = String.raw;
const pathsOf = (html) => [...html.matchAll(/data-path="([^"]*)"/g)].map((m) => m[1]);
const urlsOf = (html) => (html.match(/class="ext-link"/g) || []).length;

/* ---------- 1) 旧 linkify 用例（换渲染器后行为不变） ---------- */
const linkCases = [
  ['产物在 web/index.html，请查看。', 'web/index.html', 0],
  [S`绝对路径 D:\智能体工作区\软著版数字助理\AGENT.md 已更新`, S`D:\智能体工作区\软著版数字助理\AGENT.md`, 0],
  ['./mock-演示.txt 已生成。', './mock-演示.txt', 0],
  ['参考 https://example.com/a?b=1 文档。', null, 1],
  ['写进了 `config.json` 里。', 'config.json', 0],
  ['改好了 `D:\\x\\y\\说明 v2.md`，看下', 'D:\\x\\y\\说明 v2.md', 0],
  ['`My File.txt` 也在。', 'My File.txt', 0],
  ['看 https://example.com/x.go 与 src/main.go', 'src/main.go', 1],
  ['demo/子目录/页面.html 可用', 'demo/子目录/页面.html', 0],
  ['Node.js 和 Go1.26.5 都行', null, 0],
  ['a/b/c 目录结构', null, 0],
  ['v1.2 版本', null, 0],
  ['第 3.5 节说明 e.g. 内容', null, 0],
  [S`正文里的 D:\x\说明 v2.md（含空格）不识别`, null, 0],
  ['`go build -o jx-assistant.exe .` 是命令', null, 0],
  // 裸 URL 在中文正文里截断（marked 会吞中文，渲染器要切回来）
  ['见 https://x.com/a。然后继续', null, 1],
  ['先 https://x.com/a，再 https://y.com/b；结束', null, 2],
];
for (const [input, wantPath, wantUrl] of linkCases) {
  const html = md(input);
  const paths = pathsOf(html);
  ok(paths.length === (wantPath ? 1 : 0) && (!wantPath || paths[0] === wantPath) && urlsOf(html) === wantUrl,
    'linkify: ' + input, { paths, urls: urlsOf(html), html });
}

/* ---------- 2) GFM 新语法 ---------- */
ok(md('*斜体* 与 _斜体2_').includes('<em>斜体</em>') && md('_斜体2_').includes('<em>斜体2</em>'), '斜体');
ok(md('~~删除线~~').includes('<del>删除线</del>'), '删除线');
ok(md('> 引用\n>> 嵌套').startsWith('<blockquote><p>引用</p><blockquote>'), '引用块嵌套');
ok(md('a\n\n---\n\nb').includes('<hr>'), '分隔线');
const task = md('- [ ] 待办\n- [x] 完成');
ok(task.includes('<input type="checkbox" disabled> 待办') && task.includes('<input type="checkbox" disabled checked> 完成'), '任务列表', task);
const tbl = md('| L | C | R | N |\n|:--|:-:|--:|---|\n| 1 | 2 | 3 | 4 |');
ok(tbl.includes('<th>L</th><th style="text-align:center">C</th><th style="text-align:right">R</th><th>N</th>'), '表格三向对齐', tbl);
ok(/<td style="text-align:center">2<\/td>/.test(tbl) && /<td style="text-align:right">3<\/td>/.test(tbl), '表格数据行对齐');
ok(md('![x](https://a.b/c.png)').includes('<img src="https://a.b/c.png"'), '远程图片');
ok(md('- a\n  - b\n    - c\n- d') === '<ul><li>a<ul><li>b<ul><li>c</li></ul></li></ul></li><li>d</li></ul>', '嵌套列表', md('- a\n  - b\n    - c\n- d'));
ok(md('1、 第一项\n2、 第二项') === '<ol><li>第一项</li><li>第二项</li></ol>', '中文顿号有序列表', md('1、 第一项\n2、 第二项'));
ok(md('见 www.example.com/x 后').includes('href="http://www.example.com/x"'), 'www 自动链接');
ok(md('3. 三\n4. 四').includes('<ol start="3">'), '有序列表 start');
ok(md('- a\n\n- b').includes('<p>'), '松散列表包 p');
const pipe = md('| a\\|b | c |\n|---|---|\n| 1 | 2 |');
ok(pipe.includes('<th>a|b</th>'), '表格竖线转义', pipe);
ok(md('> 引用里的 **粗体**').includes('<strong>粗体</strong>'), '引用内行内格式');
ok(md('    indented code') === '<pre><code>indented code</code></pre>', '缩进代码块');

/* ---------- 3) XSS：白名单标签 + 协议白名单 ---------- */
const ALLOWED = new Set(['p', 'h2', 'h3', 'h4', 'h5', 'strong', 'em', 'del', 'br', 'a', 'code',
  'pre', 'blockquote', 'hr', 'table', 'thead', 'tbody', 'tr', 'th', 'td', 'ul', 'ol', 'li', 'input', 'img']);
const xssCases = [
  '<script>alert(1)</script>',
  '<script src="https://evil/x.js"></script>',
  '<img src=x onerror=alert(1)>',
  '<iframe src="https://evil"></iframe>',
  '<style>body{background:red}</style>',
  '<div onclick="alert(1)">x</div>',
  '[x](javascript:alert(1))',
  '[x](data:text/html;base64,PHNjcmlwdD4=)',
  '![x](data:image/svg+xml,<svg onload=alert(1)>)',
  '![y](javascript:alert(2))',
  '![z](https://a.b/c"onerror=alert(3))',
  '<a href="https://ok.com" onclick="alert(4)">link</a>',
  '```\n<script>alert(5)</script>\n```',
  'auto@link <a@b.com> 与 [t](vbscript:msgbox)',
];
for (const c of xssCases) {
  const out = md(c);
  // 输出中出现的每个标签都必须在白名单内（实体 &lt;script&gt; 不算标签）
  for (const m of out.matchAll(/<\/?([a-zA-Z][a-zA-Z0-9-]*)/g)) {
    ok(ALLOWED.has(m[1].toLowerCase()), 'XSS 白名单标签: ' + JSON.stringify(c), { tag: m[1], out });
  }
  // 输出的 href/src 必须是 http(s)
  for (const m of out.matchAll(/(?:href|src)="([^"]*)"/g)) {
    ok(/^https?:\/\//i.test(m[1]), 'XSS 协议白名单: ' + JSON.stringify(c), { attr: m[1], out });
  }
  // 事件属性不可能出现在生成的标签里（文本里的 onerror= 是转义后的字面量，允许）
  for (const m of out.matchAll(/<(?:a|img|input)[^>]*>/g)) {
    ok(!/\son\w+\s*=/i.test(m[0]) && !/javascript:/i.test(m[0]), 'XSS 事件属性: ' + JSON.stringify(c), { tag: m[0] });
  }
}
ok(md('[x](javascript:alert(1))') === '<p>[x](javascript:alert(1))</p>', 'js 链接按原文显示', md('[x](javascript:alert(1))'));
ok(!/<iframe/.test(md('<iframe src="https://evil"></iframe>')), 'iframe 挡住');
ok(!/<style/.test(md('<style>x</style>')), 'style 挡住');

/* ---------- 4) 视觉契约 ---------- */
ok(md('# 一级') === '<h2>一级</h2>', '标题 h2 起步', md('# 一级'));
ok(md('###### 六级') === '<h5>六级</h5>', '六级封顶 h5', md('###### 六级'));
ok(md('第一行\n第二行').includes('<br>'), '段落单换行=br');
const a1 = md('[文字](https://x.com/a)');
ok(a1.includes('class="ext-link"') && a1.includes('rel="noopener"'), 'ext-link + noopener', a1);
const u1 = md('看 https://example.com/a*b 与 https://y.com/c*d');
ok(urlsOf(u1) === 2 && !/<em>/.test(u1), 'URL 内星号不出斜体', u1);
ok(!/<em>/.test(md('foo_bar_baz')), 'intraword 下划线不斜体');
ok(md('`**不粗** ~不删~`') === '<p><code>**不粗** ~不删~</code></p>', '行内代码原样', md('`**不粗** ~不删~`'));
const cb = md('```md\n> 引用\n---\n- [ ] 任务\n```');
ok(cb === '<pre><code>&gt; 引用\n---\n- [ ] 任务</code></pre>', '代码块含 > 和 --- 原样', cb);
ok(md('**加粗** 与 ~~删除~~ 混排') === '<p><strong>加粗</strong> 与 <del>删除</del> 混排</p>', '粗体删除线混排');
ok(md('') === '' && md(null) === '', '空输入');
const half = md('| a | b\n|---|---|\n| 1');           // 流式半成品表格
ok(typeof half === 'string' && half.length > 0, '流式半成品不抛错', half);
ok(typeof md('```go\nfmt.Println("x")') === 'string', '未闭合代码块不抛错');

/* ---------- 5) 用户消息胶囊红线（app-chat.js 的 mdBody=false 分支不走 md()） ----------
 * 这里从 app-chat.js 抽出胶囊正则原样跑：该分支的输入语法必须保持「引用N/文件名」
 * 灰胶囊行为；同时提醒：用户消息渲染代码（appendQuotedBody/appendQuoteBodyText）
 * 不在本渲染器改动范围内。 */
const chatSrc = readFileSync(pathMod.join(root, 'web', 'app-chat.js'), 'utf8');
const rxGrab = chatSrc.match(/const rx = \/“\(引用\\d\+\|\[\^”\\n\]\*\\.\[A-Za-z\]\{1,8\}\)”\/g;/);
ok(!!rxGrab, '用户消息胶囊正则仍在 app-chat.js（appendQuoteBodyText 未被改动）');
if (rxGrab) {
  const rx2 = /“(引用\d+|[^”\n]*\.[A-Za-z]{1,8})”/g;
  const hits = [...正文样例(rx2)];
  ok(hits.length === 2, '「引用1」「报告final.docx」两处命中胶囊形状', hits);
}
function 正文样例(rx) {
  const src = '正文“引用1”与“报告final.docx”结尾';
  rx.lastIndex = 0;
  const out = [];
  let m;
  while ((m = rx.exec(src))) out.push(m[1]);
  return out;
}

console.log(`md-render: ${pass} pass, ${fail} fail`);
if (fail) process.exit(2);
