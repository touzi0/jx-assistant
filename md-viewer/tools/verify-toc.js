// Verify the MD viewer TOC behavior in a real browser (system Edge).
// Usage: node tools/verify-toc.js
//
// 1) builds a temp HTML exactly the way src/MDViewer.cs fills the template
//    (__TITLE__/__META__/__KEEP_RUNNING__/__B64__), with the sections of
//    build/目录测试.md lengthened so the content actually overflows;
// 2) drives it with playwright-core + Edge and asserts:
//      A. clicking a trailing TOC entry scrolls main to the heading,
//         the heading is visible below the header, and the clicked entry
//         is the highlighted (.cur) one;
//      B. same for the very last entry (bottom-of-scroll edge case);
//      C. scrolling the content updates the highlight (scrollspy),
//         including pinning to the last entry at the bottom;
// 4) writes screenshots to build/verify-toc-*.png and exits non-zero on failure.
'use strict';
const fs = require('fs');
const os = require('os');
const path = require('path');
const { pathToFileURL } = require('url');

const root = path.join(__dirname, '..');
const tplPath = path.join(root, 'viewer-template.html');
const mdPath = path.join(root, 'build', '目录测试.md');
const shotDir = path.join(root, 'build');

function requirePw() {
  const candidates = [
    path.join(root, '..', '坚祥数字助理2', 'node_modules', 'playwright-core'),
    path.join(root, '..', '坚祥数字助理2', 'node_modules', '.pnpm', 'playwright-core@1.61.1', 'node_modules', 'playwright-core'),
    'playwright-core',
  ];
  for (const c of candidates) {
    try { return require(c); } catch (e) { /* next */ }
  }
  throw new Error('playwright-core not found (tried: ' + candidates.join(' ; ') + ')');
}

function buildTempHtml() {
  let md = fs.readFileSync(mdPath, 'utf8');
  // lengthen every placeholder paragraph so the doc overflows the viewport
  const filler = Array.from({ length: 30 }, (_, i) => `段落内容 ${i + 1}，用于撑高文档验证滚动。`).join('\n\n');
  md = md.replace(/^段落内容。$/gm, filler);
  const tpl = fs.readFileSync(tplPath, 'utf8');
  // 模板只带 /*__MARKED__*/ 占位符（marked 本体全仓唯一一份在 web/marked.lib.js，
  // 主程序 mdview.go 启动时注入；这里按同一规则注入后才能驱动真实页面）
  const markedJs = fs.readFileSync(path.join(root, '..', 'web', 'marked.lib.js'), 'utf8');
  if (!tpl.includes('/*__MARKED__*/')) throw new Error('template missing /*__MARKED__*/ slot');
  if (/<\/script/.test(markedJs)) throw new Error('marked lib contains </script');
  const name = '目录测试.md';
  const b64 = Buffer.from(md, 'utf8').toString('base64');
  const html = tpl
    .replace('/*__MARKED__*/', () => markedJs)
    .replace('__TITLE__', name)
    .replace('__META__', name + ' · ' + md.length + ' 字符')
    .replace('__KEEP_RUNNING__', 'false')
    .replace('__B64__', b64);
  const outDir = path.join(os.tmpdir(), 'mdviewer');
  fs.mkdirSync(outDir, { recursive: true });
  const outPath = path.join(outDir, 'verify-目录测试.html');
  fs.writeFileSync(outPath, html, 'utf8');
  return outPath;
}

const results = [];
function check(id, cond, detail) {
  results.push({ id, ok: !!cond, detail: detail || '' });
  console.log((cond ? 'PASS' : 'FAIL') + '  ' + id + (detail ? '  (' + detail + ')' : ''));
}

(async () => {
  const pw = requirePw();
  const edgePath = 'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';
  if (!fs.existsSync(edgePath)) throw new Error('Edge not found at ' + edgePath);
  const htmlPath = buildTempHtml();
  console.log('temp html ->', htmlPath);

  const browser = await pw.chromium.launch({ executablePath: edgePath, headless: true });
  try {
    const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
    page.on('pageerror', (e) => console.log('pageerror:', e.message));
    await page.goto(pathToFileURL(htmlPath).href);
    await page.waitForSelector('#toc a.ti', { timeout: 5000 });

    const cnt = await page.locator('#toc a.ti').count();
    check('toc-entries', cnt >= 8, cnt + ' entries');

    const stopScroll = async () => {
      let prev = -1;
      for (let i = 0; i < 40; i++) {
        const s = await page.evaluate(() => document.querySelector('main').scrollTop);
        if (s === prev) return s;
        prev = s;
        await page.waitForTimeout(120);
      }
      return prev;
    };
    const state = () => page.evaluate(() => {
      const main = document.querySelector('main');
      const cur = document.querySelector('#toc .ti.cur');
      const heads = [...document.querySelectorAll('#content h1,h2,h3,h4,h5,h6')];
      return {
        scrollTop: main.scrollTop,
        scrollHeight: main.scrollHeight,
        clientHeight: main.clientHeight,
        curText: cur ? cur.textContent : null,
        headTops: heads.map((h) => h.getBoundingClientRect().top),
        headTexts: heads.map((h) => h.textContent.trim()),
      };
    });
    const HEADER_BOTTOM = 52; // header height per CSS var --header-h

    // ---- A: click a trailing (second-to-last) entry ----
    await page.screenshot({ path: path.join(shotDir, 'verify-toc-1-initial.png') });
    const idxA = cnt - 2;
    const entryA = page.locator('#toc a.ti').nth(idxA);
    const textA = (await entryA.textContent()).trim();
    const before = (await state()).scrollTop;
    await entryA.click();
    await stopScroll();
    let s = await state();
    const topA = s.headTops[idxA];
    check('A1-scroll-moved', s.scrollTop > before + 100, `scrollTop ${before} -> ${s.scrollTop}`);
    check('A2-heading-visible-below-header', topA >= HEADER_BOTTOM - 2 && topA < HEADER_BOTTOM + 80, `heading top ${topA.toFixed(1)}px (header ${HEADER_BOTTOM}px)`);
    check('A3-highlight-matches-click', s.curText === textA, `cur="${s.curText}" clicked="${textA}"`);
    await page.screenshot({ path: path.join(shotDir, 'verify-toc-2-after-click.png') });

    // ---- B: click the very last entry (bottom edge case) ----
    const beforeB = (await state()).scrollTop;
    const entryB = page.locator('#toc a.ti').nth(cnt - 1);
    const textB = (await entryB.textContent()).trim();
    await entryB.click();
    await stopScroll();
    s = await state();
    const topB = s.headTops[cnt - 1];
    check('B1-last-entry-visible', topB >= HEADER_BOTTOM - 2 && topB < 900, `heading top ${topB.toFixed(1)}px`);
    check('B2-highlight-matches-click', s.curText === textB, `cur="${s.curText}" clicked="${textB}"`);
    check('B3-scrolled-toward-entry', s.scrollTop > beforeB, `scrollTop ${beforeB.toFixed(0)} -> ${s.scrollTop.toFixed(0)}`);

    // ---- C: manual scrolling updates the highlight (scrollspy) ----
    await page.evaluate(() => { document.querySelector('main').scrollTop = 0; });
    await stopScroll();
    s = await state();
    check('C1-top-highlight-first', s.curText === s.headTexts[0], `cur="${s.curText}"`);
    await page.mouse.move(800, 400);
    await page.mouse.wheel(0, 700);
    await page.waitForTimeout(250);
    await page.mouse.wheel(0, 700);
    await stopScroll();
    s = await state();
    check('C2-highlight-follows-scroll', s.curText !== s.headTexts[0], `after 1400px wheel cur="${s.curText}"`);
    await page.screenshot({ path: path.join(shotDir, 'verify-toc-3-scrollspy.png') });
    await page.evaluate(() => { document.querySelector('main').scrollTop = 1e9; });
    await stopScroll();
    const bottom = await page.evaluate(() => ({
      pinned: document.querySelector('#toc .ti.cur') === document.querySelector('#toc a.ti:last-of-type'),
      curText: (document.querySelector('#toc .ti.cur') || {}).textContent || null,
    }));
    check('C3-bottom-pins-last', bottom.pinned, `at bottom cur="${bottom.curText}"`);
  } finally {
    await browser.close();
  }

  const failed = results.filter((r) => !r.ok);
  console.log('\n' + (failed.length ? `FAILED ${failed.length}/${results.length}` : `ALL PASS (${results.length} checks)`));
  process.exit(failed.length ? 1 : 0);
})().catch((e) => { console.error('ERROR', e); process.exit(2); });
