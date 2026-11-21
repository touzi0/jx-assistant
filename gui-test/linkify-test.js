// 链接化单测：从 app-core.js 抽取渲染器全套 + marked.lib.js，走 md() 全链路
// 验证路径/URL 识别（换 marked 渲染器后行为与旧 inlineFmt 版一致；用例清单原样保留）
// 用法：node gui-test/linkify-test.js
const { readFileSync } = require('fs');
const pathMod = require('path');
const vm = require('vm');
const src = readFileSync(pathMod.join(__dirname, '..', 'web', 'app-core.js'), 'utf8');
const grab = (re) => { const m = src.match(re); if (!m) throw new Error('not found: ' + re); return m[0]; };
const escFn = grab(/function esc\(s\) \{[\s\S]*?\n\}/);
const mdSec = grab(/\/\* ---------- Markdown 渲染[\s\S]*?\n\}(?=\n\nfunction relTime)/);
const sandbox = {};
vm.createContext(sandbox);
vm.runInContext(readFileSync(pathMod.join(__dirname, '..', 'web', 'marked.lib.js'), 'utf8'), sandbox);
vm.runInContext(escFn + '\n' + mdSec + '\nthis.md = md;', sandbox);
const fmt = (s) => sandbox.md(s);

const S = String.raw;
let pass = 0, fail = 0;
const cases = [
  // [输入, 期望 file-link 的 data-path（或 null）, 期望 URL 链接数]
  ['产物在 web/index.html，请查看。', 'web/index.html', 0],
  [S`绝对路径 D:\智能体工作区\软著版数字助理\AGENT.md 已更新`, S`D:\智能体工作区\软著版数字助理\AGENT.md`, 0],
  ['./mock-演示.txt 已生成。', './mock-演示.txt', 0],
  ['参考 https://example.com/a?b=1 文档。', null, 1],
  ['写进了 `config.json` 里。', 'config.json', 0],
  ['改好了 `D:\\x\\y\\说明 v2.md`，看下', 'D:\\x\\y\\说明 v2.md', 0],   // 反引号内含空格路径
  ['`My File.txt` 也在。', 'My File.txt', 0],
  ['看 https://example.com/x.go 与 src/main.go', 'src/main.go', 1],
  ['demo/子目录/页面.html 可用', 'demo/子目录/页面.html', 0],
  // 不应出 file-link
  ['Node.js 和 Go1.26.5 都行', null, 0],
  ['a/b/c 目录结构', null, 0],
  ['v1.2 版本', null, 0],
  ['第 3.5 节说明 e.g. 内容', null, 0],
  [S`正文里的 D:\x\说明 v2.md（含空格）不识别`, null, 0],                 // 正文含空格路径：不判定
  ['`go build -o jx-assistant.exe .` 是命令', null, 0],                 // 命令不是路径
];
for (const [input, wantPath, wantUrl] of cases) {
  const html = fmt(input);
  const paths = [...html.matchAll(/data-path="([^"]*)"/g)].map((m) => m[1]);
  const urlCount = (html.match(/class="ext-link"/g) || []).length;
  let ok;
  if (wantPath) ok = paths.length === 1 && paths[0] === wantPath && urlCount === wantUrl;
  else ok = paths.length === 0 && urlCount === wantUrl;
  if (ok) pass++;
  else { fail++; console.log('FAIL', JSON.stringify(input), '->', JSON.stringify({ paths, urlCount })); }
}
// XSS：路径中的引号/尖括号不能逃出属性；输出的标签必须在渲染器白名单内
const xss = fmt(S`x/<img src=x onerror=alert(1)>.js "a".md 与 <script>alert(2)</script>`);
const xssOk = !/<img/.test(xss.replace(/&lt;img/g, '')) && !/<script/.test(xss.replace(/&lt;script/g, ''));
console.log(`linkify: ${pass + (xssOk ? 1 : 0)} pass, ${fail + (xssOk ? 0 : 1)} fail（含 XSS 一例）`);
if (fail || !xssOk) process.exit(2);
