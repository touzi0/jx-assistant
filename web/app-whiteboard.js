/* app-whiteboard.js —— 画板：跟 AI 交流时随手画个示意图。
 *
 * 入口：对话输入区加号菜单「画板」（主对话区 / 右栏子对话 / 首页都可用，与工作模式无关）。
 * 形态：居中弹窗（.modal），白底画布 + 工具条：画笔 / 文字 / 橡皮，黑红蓝绿黄与
 * 自选色，三档线宽，撤销 / 清空。
 * 产出：「插入到输入框」把画布导成 PNG（逻辑尺寸的 2 倍，2000×1240）走 stageFiles
 * 进附件区——与粘贴图片同一条通路，发送时随消息带给 AI。插入后画布清空；点关闭
 * 则保留内容，下次打开接着画（误关不丢）。
 * 数据：画布内容存成操作列表（笔画＝点序列、文字＝位置＋文本、橡皮＝白色粗线、
 * 清空＝标记），撤销 / 清空 / 适配尺寸都按列表整体重画——不用位图快照，内存小、
 * 重画无损。逻辑坐标系固定 1000×620，显示尺寸按弹窗可用宽高适配（wbFit），指针
 * 坐标按显示比例换算回逻辑坐标：窗口大小只影响显示，不影响已画内容与导出清晰度。
 * 手机端返回键：开窗压一层历史栈（{jxWhiteboard:1}），返回手势＝关窗（与设置页
 * 同一套哨兵做法，见 app-settings.js closeSettingsUI）。 */

const WB = {
  W: 1000, H: 620, SCALE: 2, // 逻辑尺寸 × 2 = 画布像素（导出分辨率）
  FONT: '"PingFang SC", "Microsoft YaHei", system-ui, sans-serif',
  ops: [],                   // 操作列表：{type:'stroke',pts,color,size} / {type:'text',x,y,text,color,size} / {type:'clear'}
  tool: 'pen', color: '#1f2328', size: 6,
  open: false, view: null,   // view：打开它的输入区（插入目标）
  drawing: null,             // 进行中的笔画
};
let wbHistPushed = 0;        // 窄屏实际压成功的历史层数（关窗按它退栈，防越界）

const wbEl = {
  get modal() { return $('whiteboard'); },
  get canvas() { return $('wb-canvas'); },
  get text() { return $('wb-text'); },
  get ctx() { return this.canvas.getContext('2d'); },
};

// 线宽 → 文字字号（逻辑 px）：细/中/粗三档各配一个能看清的字号
const wbTextSize = () => ({ 3: 20, 6: 26, 12: 34 }[WB.size] || 26);

/* ---------- 开合 ---------- */

function openWhiteboard(view) {
  WB.view = view || popView || mainView;
  WB.open = true;
  narrowClosePanels(); // 手机端：进弹窗即收起侧栏浮层
  if (isNarrow() && wbHistPushed === 0) {
    try { history.pushState({ jxWhiteboard: 1 }, ''); wbHistPushed++; } catch (e) { /* 远程外壳等不许 pushState：降级为纯 UI */ }
  }
  wbEl.modal.classList.remove('hidden');
  wbFit();
  wbRedraw();
}

function closeWhiteboard(fromPop) {
  if (!WB.open) return;
  wbCommitText(); // 正在打字的内容先落笔，关窗不丢
  WB.open = false;
  WB.drawing = null;
  wbEl.modal.classList.add('hidden');
  const n = wbHistPushed;
  wbHistPushed = 0;
  if (n > 0 && !fromPop) {
    backGuardSkipOnce = true; // 程序性退栈：返回键守卫跳过这次 popstate（左栏别跟着弹出来）
    history.go(-n);
  }
}

// 返回手势：落点不是自己压的那层（d=0）＝用户按了返回 → 关窗；栈已自己退穿，不再退
window.addEventListener('popstate', (e) => {
  const d = (e.state && e.state.jxWhiteboard) || 0;
  if (!WB.open) { wbHistPushed = 0; return; }
  if (d === 0) closeWhiteboard(true);
});

// 画布显示尺寸：按卡片可用宽度铺开，高度不够（矮窗口 / 手机横屏）时按高度收
function wbFit() {
  const card = wbEl.modal.querySelector('.wb-card');
  const cs = getComputedStyle(card);
  const availW = card.clientWidth - parseFloat(cs.paddingLeft) - parseFloat(cs.paddingRight);
  const chrome = wbEl.modal.querySelector('.wb-head').offsetHeight +
    wbEl.modal.querySelector('.wb-foot').offsetHeight +
    parseFloat(cs.paddingTop) + parseFloat(cs.paddingBottom) + 20; // 上下两段 10px 间距
  const availH = Math.max(200, window.innerHeight * 0.88 - chrome); // .modal-card 上限 88vh
  let w = Math.max(260, Math.min(availW, WB.W));
  let h = w * WB.H / WB.W;
  if (h > availH) { h = availH; w = h * WB.W / WB.H; }
  wbEl.canvas.style.width = Math.round(w) + 'px';
  wbEl.canvas.style.height = Math.round(h) + 'px';
}

/* ---------- 绘制 ---------- */

// 整幅重画：白底 + 按操作列表依序回放（撤销 / 清空 / 开窗都走这里）
function wbRedraw() {
  const ctx = wbEl.ctx;
  ctx.setTransform(WB.SCALE, 0, 0, WB.SCALE, 0, 0);
  ctx.fillStyle = '#fff';
  ctx.fillRect(0, 0, WB.W, WB.H);
  for (const op of WB.ops) wbPaintOp(ctx, op);
}

function wbPaintOp(ctx, op) {
  if (op.type === 'clear') {
    ctx.fillStyle = '#fff';
    ctx.fillRect(0, 0, WB.W, WB.H);
    return;
  }
  if (op.type === 'text') {
    ctx.fillStyle = op.color;
    ctx.font = op.size + 'px ' + WB.FONT;
    ctx.textBaseline = 'top';
    String(op.text).split('\n').forEach((ln, i) => ctx.fillText(ln, op.x, op.y + i * op.size * 1.3));
    return;
  }
  if (op.pts.length < 2) { // 单点笔画＝圆点（只填充，不再描边——描边会把点撑成两倍大）
    ctx.fillStyle = op.color;
    ctx.beginPath();
    ctx.arc(op.pts[0].x, op.pts[0].y, op.size / 2, 0, Math.PI * 2);
    ctx.fill();
    return;
  }
  wbStrokeStyle(ctx, op);
  wbStrokePath(ctx, op.pts);
  ctx.stroke();
}

function wbStrokeStyle(ctx, op) {
  ctx.strokeStyle = op.color;
  ctx.lineWidth = op.size;
  ctx.lineCap = 'round';
  ctx.lineJoin = 'round';
}

// 平滑笔画路径（≥2 点）：相邻两点的中点为端点、原点为控制点的二次贝塞尔串起来。
// 首段从起点直线到第一段中点、末段从最后一段中点直线到终点——与拖动中的逐段增量
// 画法逐段对齐（松手时不跳线）。
function wbStrokePath(ctx, pts) {
  ctx.beginPath();
  const mid = (a, b) => ({ x: (a.x + b.x) / 2, y: (a.y + b.y) / 2 });
  ctx.moveTo(pts[0].x, pts[0].y);
  const m0 = mid(pts[0], pts[1]);
  ctx.lineTo(m0.x, m0.y);
  for (let i = 1; i < pts.length - 1; i++) {
    const m = mid(pts[i], pts[i + 1]);
    ctx.quadraticCurveTo(pts[i].x, pts[i].y, m.x, m.y);
  }
  const last = pts[pts.length - 1];
  ctx.lineTo(last.x, last.y);
}

// 拖动中逐段增量：只画新到的那一段（不整幅重画，长笔画也不卡）
function wbSegLive(ctx, pts) {
  const n = pts.length;
  wbStrokeStyle(ctx, WB.drawing);
  ctx.beginPath();
  if (n === 2) {
    ctx.moveTo(pts[0].x, pts[0].y);
    ctx.lineTo((pts[0].x + pts[1].x) / 2, (pts[0].y + pts[1].y) / 2);
  } else {
    const a = pts[n - 3], b = pts[n - 2], c = pts[n - 1];
    ctx.moveTo((a.x + b.x) / 2, (a.y + b.y) / 2);
    ctx.quadraticCurveTo(b.x, b.y, (b.x + c.x) / 2, (b.y + c.y) / 2);
  }
  ctx.stroke();
}

// 指针 → 逻辑坐标（画布显示尺寸与逻辑尺寸的比例换算；略微夹取防拖出画布太远）
function wbPoint(e) {
  const r = wbEl.canvas.getBoundingClientRect();
  const k = WB.W / r.width;
  const clamp = (v, lo, hi) => Math.max(lo, Math.min(hi, v));
  return { x: clamp((e.clientX - r.left) * k, -30, WB.W + 30), y: clamp((e.clientY - r.top) * k, -30, WB.H + 30) };
}

/* ---------- 指针：画笔 / 橡皮 ---------- */

wbEl.canvas.addEventListener('pointerdown', (e) => {
  if (!WB.open || (e.pointerType === 'mouse' && e.button !== 0)) return;
  wbCommitText(); // 正在打字的先落笔
  const p = wbPoint(e);
  // 必须拦下默认动作：文字工具刚把输入框聚焦，随后的 mousedown 默认动作（焦点落到
  // 点击处）会立刻把它失焦收走——点一下什么都看不见（真机点击才暴露，合成事件没有默认动作）
  e.preventDefault();
  if (WB.tool === 'text') { wbStartText(p); return; }
  try { wbEl.canvas.setPointerCapture(e.pointerId); } catch (err) { /* 个别环境不支持捕获：拖出画布即断笔，可接受 */ }
  WB.drawing = {
    tool: WB.tool,
    color: WB.tool === 'eraser' ? '#ffffff' : WB.color,
    size: WB.tool === 'eraser' ? WB.size * 3 : WB.size,
    pts: [p],
  };
  wbPaintOp(wbEl.ctx, { type: 'stroke', ...WB.drawing }); // 落笔即见圆点
});

wbEl.canvas.addEventListener('pointermove', (e) => {
  if (!WB.drawing) return;
  const p = wbPoint(e);
  const pts = WB.drawing.pts, last = pts[pts.length - 1];
  if (Math.abs(p.x - last.x) < 0.5 && Math.abs(p.y - last.y) < 0.5) return; // 原地抖动不记点
  pts.push(p);
  wbSegLive(wbEl.ctx, pts);
});

function wbEndStroke() {
  if (!WB.drawing) return;
  const d = WB.drawing;
  WB.drawing = null;
  const pts = d.pts;
  if (pts.length >= 2) { // 补上最后半段（中点 → 终点），与整幅重画的路径完全一致
    const a = pts[pts.length - 2], b = pts[pts.length - 1];
    wbStrokeStyle(wbEl.ctx, d);
    wbEl.ctx.beginPath();
    wbEl.ctx.moveTo((a.x + b.x) / 2, (a.y + b.y) / 2);
    wbEl.ctx.lineTo(b.x, b.y);
    wbEl.ctx.stroke();
  }
  WB.ops.push({ type: 'stroke', pts, color: d.color, size: d.size });
}

wbEl.canvas.addEventListener('pointerup', wbEndStroke);
wbEl.canvas.addEventListener('pointercancel', wbEndStroke);
wbEl.canvas.addEventListener('lostpointercapture', wbEndStroke);

/* ---------- 文字工具：点一下画布 → 就地输入框 → Enter 落笔 ---------- */

function wbDispScale() { return wbEl.canvas.getBoundingClientRect().width / WB.W; }

function wbStartText(p) {
  const ta = wbEl.text;
  const k = wbDispScale();
  const size = wbTextSize();
  ta.value = '';
  ta.dataset.x = p.x;
  ta.dataset.y = p.y;
  ta.dataset.color = WB.color;
  ta.dataset.size = size;
  ta.style.left = Math.min(p.x * k, wbEl.canvas.getBoundingClientRect().width * 0.85) + 'px';
  ta.style.top = Math.min(p.y * k, wbEl.canvas.getBoundingClientRect().height - size * k * 1.4) + 'px';
  ta.style.fontSize = (size * k) + 'px';
  ta.style.fontFamily = WB.FONT; // 与落笔到画布的字体一致，输入时所见即所得
  ta.dataset.opened = Date.now(); // 失焦时据此辨认「刚打开就被抢焦」
  ta.classList.remove('hidden');
  wbAutoSize();
  ta.focus();
}

// 输入框跟着内容长：宽按最长一行量，高按行数（overflow:hidden 的 textarea 不会自己长）。
// measureText 不受变换矩阵影响，用当前逻辑坐标系直接量显示字号即可
function wbAutoSize() {
  const ta = wbEl.text;
  const size = parseFloat(ta.dataset.size) * wbDispScale();
  const ctx = wbEl.ctx;
  ctx.font = size + 'px ' + WB.FONT;
  let w = 0;
  for (const ln of ta.value.split('\n')) w = Math.max(w, ctx.measureText(ln || '输入文字').width);
  ta.style.width = Math.max(140, Math.ceil(w) + 14) + 'px';
  ta.style.height = Math.ceil(ta.value.split('\n').length * size * 1.3 + 6) + 'px';
}

wbEl.text.addEventListener('input', wbAutoSize);
wbEl.text.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') {
    e.preventDefault();
    e.stopPropagation();
    wbEl.text.value = '';
    wbEl.text.classList.add('hidden');
    return;
  }
  // Enter 落笔、Shift+Enter 换行；输入法组合中的 Enter 是选字，不落笔
  if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
    e.preventDefault();
    wbCommitText();
  }
});
wbEl.text.addEventListener('blur', () => {
  // 刚打开 150ms 内的失焦只可能是这次点击自己的焦点默认动作在「回抢」（pointerdown
  // 已 preventDefault，这里兜底 WebView2 等环境的差异）：重新聚焦，不当成用户点去了别处
  if (Date.now() - (+wbEl.text.dataset.opened || 0) < 150) { wbEl.text.focus(); return; }
  wbCommitText();
});

function wbCommitText() {
  const ta = wbEl.text;
  if (ta.classList.contains('hidden')) return;
  const txt = ta.value.replace(/\s+$/, '');
  ta.classList.add('hidden');
  if (!txt) return;
  WB.ops.push({ type: 'text', x: +ta.dataset.x, y: +ta.dataset.y, text: txt, color: ta.dataset.color, size: +ta.dataset.size });
  wbRedraw();
}

/* ---------- 工具条 ---------- */

function wbSyncTools() {
  document.querySelectorAll('#whiteboard .wb-tool[data-tool]').forEach((b) => b.classList.toggle('active', b.dataset.tool === WB.tool));
  const presets = [...document.querySelectorAll('#wb-colors .wb-color[data-color]')];
  const isPreset = presets.some((b) => b.dataset.color === WB.color);
  presets.forEach((b) => b.classList.toggle('active', b.dataset.color === WB.color));
  document.querySelector('#wb-colors .wb-custom').classList.toggle('active', !isPreset); // 取色器选的色：自选色圆亮
  document.querySelectorAll('#whiteboard .wb-size').forEach((b) => b.classList.toggle('active', +b.dataset.size === WB.size));
  wbEl.canvas.className = WB.tool === 'pen' ? '' : 'tool-' + WB.tool;
  $('wb-undo').disabled = !WB.ops.length;
}

document.querySelectorAll('#whiteboard .wb-tool[data-tool]').forEach((b) => {
  b.onclick = () => { WB.tool = b.dataset.tool; wbSyncTools(); };
});
document.querySelectorAll('#wb-colors .wb-color[data-color]').forEach((b) => {
  b.onclick = () => { WB.color = b.dataset.color; wbSyncTools(); };
});
// 自选色：取色器一改即生效；自选色圆跟着选中的颜色走
$('wb-custom').addEventListener('input', (e) => {
  WB.color = e.target.value;
  document.querySelector('.wb-custom').style.setProperty('--c', WB.color);
  wbSyncTools();
});
document.querySelectorAll('#whiteboard .wb-size').forEach((b) => {
  b.onclick = () => { WB.size = +b.dataset.size; wbSyncTools(); };
});
$('wb-undo').onclick = wbUndo;
$('wb-clear').onclick = () => {
  if (!WB.ops.length) return;
  WB.ops.push({ type: 'clear' }); // 当作一步操作记下：撤销能整幅找回
  wbRedraw();
  wbSyncTools();
};
function wbUndo() {
  if (!WB.ops.length) return;
  WB.ops.pop();
  wbRedraw();
  wbSyncTools();
}

/* ---------- 弹窗级按键 / 点击 ---------- */

// 捕获阶段先于 document 上其他 Esc 处理器：画板开着时 Esc 归它（正在打字则让给输入框自己）；
// Ctrl/Cmd+Z 撤销（打字中同样让给输入框的文本撤销）
document.addEventListener('keydown', (e) => {
  if (!WB.open || !wbEl.text.classList.contains('hidden')) return;
  if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); closeWhiteboard(); }
  else if ((e.ctrlKey || e.metaKey) && !e.shiftKey && (e.key === 'z' || e.key === 'Z')) { e.preventDefault(); wbUndo(); }
}, true);

wbEl.modal.addEventListener('click', (e) => { if (e.target === wbEl.modal) closeWhiteboard(); });
$('wb-close').onclick = () => closeWhiteboard();
$('wb-cancel').onclick = () => closeWhiteboard();
wbSyncTools(); // 初始态：撤销置灰

/* ---------- 插入到输入框：导 PNG 走附件区（与粘贴图片同一条通路） ---------- */

$('wb-insert').onclick = () => {
  if (!WB.ops.length) { toast('先在画板上画点什么'); return; }
  wbEl.canvas.toBlob((blob) => {
    if (!blob) { toast('画板导出失败，请重试'); return; }
    const d = new Date(), p = (n) => String(n).padStart(2, '0');
    const name = '画板-' + d.getFullYear() + p(d.getMonth() + 1) + p(d.getDate()) + '-' + p(d.getHours()) + p(d.getMinutes()) + p(d.getSeconds()) + '.png';
    const added = stageFiles([new File([blob], name, { type: 'image/png' })], WB.view);
    if (!added.length) return; // 超出附件上限等：stageFiles 已在输入框上方提示
    WB.ops = []; // 已进输入框：画布清空，下次从白纸开始
    wbRedraw();
    wbSyncTools();
    closeWhiteboard();
    toast('已加入输入框，发送时随消息带给 AI');
  }, 'image/png');
};

// 窗口尺寸变了只重适配显示（逻辑坐标与内容不动）；正在打字的位置会错位，先落笔
window.addEventListener('resize', () => {
  if (!WB.open) return;
  wbCommitText();
  wbFit();
});
