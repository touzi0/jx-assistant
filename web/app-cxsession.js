/* app-cxsession.js —— 本对话的组件与插件（会话级覆盖浮窗）。
 *
 * 入口：对话输入区加号菜单「本对话的组件与插件」（主对话区且有当前会话时可见）。
 * 形态：设置页同款浮窗（modal + settings-card 双栏；手机端同款两级翻页），
 * 但一级页只有「组件」「插件」两页，组件注册的独立设置页（如语音）排在它们
 * 之后（CX.registerSettings 带 pane 的动态项，组件热启停时增删）。
 * 开关语义：只写当前会话的覆盖记录（POST /api/components、/api/plugins 带
 * session），优先于全局开关、全局开关不动；与全局拨成一致时覆盖自动删除
 * （恢复跟随全局）。前端热生效复用 CX 的注入/卸载路径（CX.toggle 带 session），
 * 切换会话时 cx.js 按 session:open 重新装配。
 * 全局安装/卸载、执行型组件开关等全局性操作不在这里（归 设置 → 插件）。 */

/* ---------- 导航状态机（对齐 app-settings.js 的 setNav，作用于 #cxsession 卡） ---------- */

const cxsNav = { open: false, pane: 'cxs-components', level: 2 }; // 打开即停在「组件」页
let cxsHistDepth = 0;   // 窄屏历史栈层数（与设置页同款：0 无 / 1 一级 / 2 二级）
let cxsHistPushed = 0;  // 实际压栈成功的层数
let cxsForSid = '';     // 浮窗打开时针对的会话（切到别的会话即收起，防清单错位）

function cxsCard() { return document.getElementById('cxsession-card'); }

function cxsHistPush() {
  try {
    history.pushState({ jxCXSession: cxsHistDepth }, '');
    cxsHistPushed++;
  } catch (e) { /* 远程外壳等环境不允许 pushState：降级为纯 UI 导航 */ }
}

// 唯一 DOM 出口：高亮导航项、显隐分区页、翻页（pane-open）、返回头标题
function cxsRenderNav() {
  document.querySelectorAll('#cxs-nav .set-nav-item').forEach((b) => {
    b.classList.toggle('active', cxsNav.level === 2 && b.dataset.pane === cxsNav.pane);
  });
  document.querySelectorAll('#cxs-panes .set-pane').forEach((p) => {
    p.classList.toggle('active', p.id === 'cxs-pane-' + cxsNav.pane);
  });
  const card = cxsCard();
  if (card) card.classList.toggle('pane-open', cxsNav.level === 2);
  const cur = document.querySelector('#cxs-nav .set-nav-item[data-pane="' + cxsNav.pane + '"]');
  $('cxs-back-title').textContent = cxsNav.level === 2 && cur ? cur.textContent.trim() : '本对话的组件与插件';
  $('cxsession').classList.toggle('hidden', !cxsNav.open);
}

function cxsNavGoto(pane) {
  cxsNav.pane = pane;
  cxsNav.level = 2;
  if (isNarrow() && cxsHistDepth < 2) {
    cxsHistDepth = 2;
    cxsHistPush();
  }
  cxsRenderNav();
}

// 当前会话id（浮窗的一切操作都归它；没有会话时入口本来就不显示）
function cxsSessionId() { return (state.cur && state.cur.id) || ''; }

/* ---------- 开合 ---------- */

function openCXSessionUI() {
  const sid = cxsSessionId();
  if (!sid) { toast('还没有打开的对话'); return; }
  cxsForSid = sid;
  cxsNav.open = true;
  cxsNav.pane = 'cxs-components';
  cxsNav.level = 2; // 只有几项，直接停在「组件」页
  narrowClosePanels(); // 手机端：进浮窗即收起侧栏浮层
  if (isNarrow() && cxsHistDepth === 0) {
    cxsHistDepth = 1;
    cxsHistPush();
    cxsHistDepth = 2;
    cxsHistPush();
  }
  cxsRenderNav();
  cxsRenderComponentList();
  cxsRenderPluginList();
  cxsMountComponentSections();
  cxsMountComponentPanes();
  cxsMountPluginSections();
}

function closeCXSessionUI(fromPop) {
  cxsNav.open = false;
  cxsNav.pane = 'cxs-components';
  cxsNav.level = 2;
  cxsRenderNav();
  const n = cxsHistPushed; // 只退实际压成功的层数（栈被宿主动过时按它退，防越界）
  cxsHistDepth = 0;
  cxsHistPushed = 0;
  if (n > 0 && !fromPop) {
    backGuardSkipOnce = true;
    history.go(-n);
  }
  syncPlusChecks(mainView); // 关窗后刷新加号菜单的「已定制」✓
}

window.addEventListener('popstate', (e) => {
  const d = (e.state && e.state.jxCXSession);
  if (d == null) return; // 不是本浮窗压的栈（设置页等）：不碰
  if (!cxsNav.open) { cxsHistDepth = 0; cxsHistPushed = 0; return; }
  cxsHistDepth = d;
  cxsHistPushed = d;
  if (d >= 2) { cxsNav.level = 2; cxsRenderNav(); }
  else if (d === 1) { cxsNav.level = 1; cxsRenderNav(); } // 返回手势：二级退一级
  else closeCXSessionUI(true);
});

$('cxsession').addEventListener('click', (e) => { if (e.target === $('cxsession')) closeCXSessionUI(); });
$('cxs-cancel').onclick = () => closeCXSessionUI(); // 丢弃设置区草稿（下次打开重挂重读）；开关的即时改动保持已生效状态
// 底部「保存」＝组件设置区草稿统一落库（与设置页右下角同一个 CX.save 钩子链；
// 开关本来就即时生效不归这里管）。任一组件保存失败就不关窗——错误亮在组件
// 自己的状态行，修正后可再点保存。
$('cxs-save').onclick = async () => {
  if (!(await CX.saveSettingsSections())) return;
  closeCXSessionUI();
};
$('cxs-back').onclick = () => {
  if (cxsNav.level === 2 && isNarrow()) {
    // 手机二级 → 一级：UI 直切，历史栈有就同步退一步（与设置页同款纪律）
    cxsNav.level = 1;
    cxsHistDepth = 1;
    cxsRenderNav();
    if (cxsHistPushed > 0) {
      cxsHistPushed--;
      history.back();
    }
  } else {
    closeCXSessionUI();
  }
};
document.querySelectorAll('#cxs-nav .set-nav-item').forEach((b) => {
  b.addEventListener('click', () => cxsNavGoto(b.dataset.pane));
});

/* ---------- 组件页（会话生效清单） ---------- */

async function cxsRenderComponentList() {
  const box = $('cxs-component-list');
  if (!box) return;
  const sid = cxsSessionId();
  if (!sid) return;
  box.innerHTML = '加载中…';
  let data;
  try {
    data = await api('/api/components?s=' + encodeURIComponent(sid));
  } catch (e) {
    box.textContent = '组件清单读取失败：' + e.message;
    return;
  }
  const list = data.components || [];
  // 配置视图（总开关 + 每行「会话独立/跟随全局」）随清单一起来，同步给运行时
  const cfg = CX.applyCfg(data.cfg || { scope: false, components: {} });
  cxsSyncScopeSwitch(cfg, sid);
  box.innerHTML = '';

  for (const c of list) {
    const row = document.createElement('div');
    row.className = 'plugin-row';
    const info = document.createElement('div');
    info.className = 'pr-info';
    info.innerHTML =
      `<div class="pr-line"><b>${esc(c.name)}</b><span class="pr-ver">${esc(c.version || '')}</span>` +
      (c.override ? '<span class="pr-ovr" title="本对话覆盖了全局开关（点击恢复跟随全局）">已覆盖</span>' : '') + '</div>' +
      `<p>${esc(c.description || '')}</p>`;
    row.appendChild(info);

    // 每行两个开关：上＝本对话启用/停用（行级启停，独立生效）；
    //               下＝该组件配置「会话独立 / 跟随全局」（总开关关着时不可拨）。
    const col = document.createElement('div');
    col.className = 'pr-toggles';
    const cfe = (cfg.components || {})[c.id] || { on: !!cfg.scope, values: {} };

    const upWrap = document.createElement('div');
    upWrap.className = 'cxs-tg';
    const tg = document.createElement('label');
    tg.className = 'pr-toggle';
    tg.title = '本对话启用/停用该组件（只影响本对话）';
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.checked = !!c.enabled; // 会话生效值（覆盖优先于全局）
    cb.onchange = async () => {
      cb.disabled = true;
      const note = row.querySelector('.pr-note');
      try {
        // 会话级热开关：与设置页同一条 CX 注入/卸载路径，只多带 session
        const r = await CX.toggle(c.id, cb.checked, sid);
        if (note) note.textContent = r.hot
          ? (cb.checked ? '本对话已即时启用' : '本对话已即时停用')
          : '该组件不支持热关闭，切换会话后完全生效';
        // 覆盖徽标跟实际状态对齐：与全局一致＝已恢复跟随全局
        const line = row.querySelector('.pr-line');
        if (line) {
          const still = cb.checked !== c.globalEnabled;
          let ovr = line.querySelector('.pr-ovr');
          if (!still && ovr) ovr.remove();
          if (still && !ovr) {
            ovr = document.createElement('span');
            ovr.className = 'pr-ovr';
            ovr.title = '本对话覆盖了全局开关（点击恢复跟随全局）';
            ovr.textContent = '已覆盖';
            line.appendChild(ovr);
            bindOvrReset(ovr, c, cb);
          }
        }
      } catch (e) {
        alert('切换失败：' + e.message);
        cb.checked = !cb.checked;
      }
      cb.disabled = false;
      setTimeout(() => { if (note) note.textContent = ''; }, 3000);
    };
    tg.appendChild(cb);
    tg.appendChild(document.createElement('span'));
    upWrap.appendChild(tg);
    const capUp = document.createElement('em');
    capUp.textContent = '启用';
    upWrap.appendChild(capUp);
    col.appendChild(upWrap);

    const lowWrap = document.createElement('div');
    lowWrap.className = 'cxs-tg';
    const cfgTg = document.createElement('label');
    cfgTg.className = 'pr-toggle';
    cfgTg.title = cfg.scope
      ? '该组件的设置：本对话独立保存 / 跟随全局（拨回跟随＝删掉本对话的这份记录）'
      : '总开关当前是「跟随全局」：打开底部总开关后，每行才能切成会话独立';
    const cfgCb = document.createElement('input');
    cfgCb.type = 'checkbox';
    cfgCb.checked = cfg.scope && cfe.on !== false;
    cfgCb.disabled = !cfg.scope;
    cfgCb.onchange = async () => {
      cfgCb.disabled = true;
      const note = row.querySelector('.pr-note');
      try {
        const r = await api('/api/components/cfg/mode', {
          method: 'POST',
          body: JSON.stringify({ session: sid, id: c.id, on: cfgCb.checked }),
        });
        // 视图换了就该组件的存储读写换通道：dispose + 重新 init 让它重读一遍
        CX.applyCfg(r.cfg, [c.id]);
        if (note) note.textContent = cfgCb.checked
          ? '已切为「会话独立」：在本对话里改它的设置只影响本对话'
          : '已切回「跟随全局」：本对话的这份配置记录已删除';
        const cap = lowWrap.querySelector('em');
        if (cap) cap.textContent = cfgCb.checked ? '独立' : '跟随';
      } catch (e) {
        alert('切换失败：' + e.message);
        cfgCb.checked = !cfgCb.checked;
      }
      cfgCb.disabled = !CX._cfg.scope;
      setTimeout(() => { if (note) note.textContent = ''; }, 4000);
    };
    cfgTg.appendChild(cfgCb);
    cfgTg.appendChild(document.createElement('span'));
    lowWrap.appendChild(cfgTg);
    const capLow = document.createElement('em');
    capLow.textContent = cfgCb.checked ? '独立' : '跟随';
    lowWrap.appendChild(capLow);
    col.appendChild(lowWrap);
    row.appendChild(col);

    // 「已覆盖」徽标点击＝拨回全局值（恢复跟随全局；后端会删掉覆盖记录）
    const ovrBtn = info.querySelector('.pr-ovr');
    if (ovrBtn) bindOvrReset(ovrBtn, c, cb);
    const note = document.createElement('div');
    note.className = 'pr-note';
    row.appendChild(note);
    box.appendChild(row);
  }
  if (!list.length) box.innerHTML = '<div class="pr-note">组件/ 下没有发现组件</div>';
}

// 「已覆盖」徽标＝一键拨回全局启停值（恢复跟随全局）
function bindOvrReset(ovrBtn, c, cb) {
  ovrBtn.style.cursor = 'pointer';
  ovrBtn.onclick = async (ev) => {
    ev.stopPropagation();
    cb.checked = !!c.globalEnabled;
    cb.dispatchEvent(new Event('change'));
  };
}

/* ---------- 底部总开关：本对话使用独立的组件设置（一键切全场） ----------
   开＝全场进入会话独立（每行下开关可用，缺省独立）；
   关＝**整行完全回归全局**：每行的配置覆盖与启停覆盖一起清空，回到全局状态。
   行级细粒度只在本总开关开着时才有意义（关掉后下开关禁用）。 */
function cxsSyncScopeSwitch(cfg, sid) {
  const cb = $('cxs-cfg-scope');
  if (!cb) return;
  cb.checked = !!(cfg && cfg.scope);
  cb.disabled = false; // 上一次切换期间被禁用：同步完成即恢复可拨
  const tip = $('cxs-scope-tip');
  if (tip) {
    tip.textContent = cb.checked
      ? '本对话独立：每行下开关可选「会话独立 / 跟随全局」，独立项的设置只影响本对话'
      : '跟随全局：每行的启停与设置都按 设置 → 组件 里的全局值';
  }
  if (!cb._bound) {
    cb._bound = true;
    cb.onchange = async () => {
      const sid2 = cxsSessionId();
      if (!sid2) { cb.checked = !cb.checked; return; }
      cb.disabled = true;
      try {
        const r = await api('/api/components/cfg/scope', {
          method: 'POST',
          body: JSON.stringify({ session: sid2, on: cb.checked }),
        });
        // 全场换视角：所有已启用组件重读一遍存储；再把清单重渲染（启停可能回落到全局）
        const ids = (CX._list || []).filter((c) => c.enabled).map((c) => c.id);
        CX.applyCfg(r.cfg, ids);
        cxsRenderComponentList();
      } catch (e) {
        alert('切换失败：' + e.message);
        cb.checked = !cb.checked;
        cb.disabled = false;
      }
    };
  }
}

/* ---------- 插件页（会话生效清单；行渲染复用 app-settings.js 的体系分组） ---------- */

async function cxsRenderPluginList() {
  const box = $('cxs-plugin-list');
  if (!box) return;
  const sid = cxsSessionId();
  if (!sid) return;
  box.innerHTML = '加载中…';
  let data;
  try {
    data = await api('/api/plugins?s=' + encodeURIComponent(sid));
  } catch (e) {
    box.textContent = '插件清单读取失败：' + e.message;
    return;
  }
  if (window.CX && data && typeof data.sessionCustom !== 'undefined') {
    CX.sessionCustom = !!data.sessionCustom;
    syncPlusChecks(mainView);
  }
  const byLayer = {};
  for (const p of (data.plugins || [])) (byLayer[p.layer] = byLayer[p.layer] || []).push(p);
  box.innerHTML = '';
  const opts = { session: sid };
  for (const L of (data.layers || [])) {
    const group = byLayer[L.id] || [];
    delete byLayer[L.id];
    box.appendChild(pluginLayerSection(L, group, opts));
  }
  for (const [lid, group] of Object.entries(byLayer)) {
    box.appendChild(pluginLayerSection({ id: lid, name: group[0].layerName || lid }, group, opts));
  }
}

/* ---------- 组件/插件自己的设置区（照设置页：非独立页形态排在清单下方） ---------- */

function cxsMountComponentSections() {
  const box = $('cxs-component-sections');
  if (!box) return;
  box.innerHTML = '';
  for (const sec of CX.settingsSections()) {
    if (sec.pane) continue; // 带 pane 的走独立页（下方 cxsMountComponentPanes）
    const wrap = document.createElement('div');
    wrap.className = 'prov-section jx-section';
    const h = document.createElement('h4');
    h.textContent = sec.title;
    wrap.appendChild(h);
    const body = document.createElement('div');
    wrap.appendChild(body);
    try { sec.mount(body); } catch (e) { body.textContent = '组件设置加载失败：' + e.message; }
    box.appendChild(wrap);
  }
}

function cxsMountPluginSections() {
  const box = $('cxs-plugin-sections');
  if (!box) return;
  box.innerHTML = '';
  for (const sec of JX._settingsSections) {
    const wrap = document.createElement('div');
    wrap.className = 'prov-section jx-section';
    const h = document.createElement('h4');
    h.textContent = sec.title;
    wrap.appendChild(h);
    const body = document.createElement('div');
    wrap.appendChild(body);
    try { sec.mount(body); } catch (e) { body.textContent = '插件设置加载失败：' + e.message; }
    box.appendChild(wrap);
  }
}

/* ---------- 组件注册的独立设置页（如语音）：排在「组件/插件」之后 ----------
   与 app-settings.js 的 syncComponentPanePresence/mountComponentPanes 同一套
   逻辑，只是挂在 #cxs-nav / #cxs-panes 里、锚点换成「插件」项之后。 */

function cxsSyncComponentPanePresence() {
  const nav = document.getElementById('cxs-nav');
  const panes = document.getElementById('cxs-panes');
  if (!nav || !panes) return;
  const defs = CX.settingsSections().filter((s) => s.pane && s.pane.label);
  const live = new Set(defs.map((d) => d.id));
  document.querySelectorAll('#cxsession [data-cxs-pane]').forEach((el) => {
    const id = el.dataset.cxsPane;
    if (live.has(id)) return;
    const paneName = 'cxs-component-' + id;
    if (cxsNav.pane === paneName) { // 正停在被撤的页：退回组件页
      cxsNav.pane = 'cxs-components';
      cxsRenderNav();
    }
    el.remove();
  });
  for (const d of defs) {
    const paneName = 'cxs-component-' + d.id;
    if (nav.querySelector('.set-nav-item[data-pane="' + paneName + '"]')) continue;
    const navBtn = document.createElement('button');
    navBtn.type = 'button';
    navBtn.className = 'set-nav-item';
    navBtn.dataset.pane = paneName;
    navBtn.dataset.cxsPane = d.id;
    navBtn.innerHTML = (d.pane.icon || '') + esc(d.pane.label);
    navBtn.onclick = () => cxsNavGoto(paneName);
    // 排在「组件/插件」之下：插在「插件」项之后（没有就追加到末尾）
    const anchor = nav.querySelector('.set-nav-item[data-pane="cxs-plugins"]');
    if (anchor && anchor.nextSibling) nav.insertBefore(navBtn, anchor.nextSibling);
    else nav.appendChild(navBtn);
    const paneEl = document.createElement('div');
    paneEl.className = 'set-pane';
    paneEl.id = 'cxs-pane-' + paneName;
    paneEl.dataset.cxsPane = d.id;
    const anchorPane = document.getElementById('cxs-pane-cxs-plugins');
    if (anchorPane && anchorPane.parentNode === panes) panes.insertBefore(paneEl, anchorPane.nextSibling);
    else panes.appendChild(paneEl);
  }
}

function cxsMountComponentPanes() {
  cxsSyncComponentPanePresence();
  for (const d of CX.settingsSections()) {
    if (!d.pane || !d.pane.label) continue;
    const paneEl = document.getElementById('cxs-pane-cxs-component-' + d.id);
    if (!paneEl) continue;
    paneEl.innerHTML = '';
    const h = document.createElement('h3');
    h.textContent = d.title;
    paneEl.appendChild(h);
    const inner = document.createElement('div');
    paneEl.appendChild(inner);
    try { d.mount(inner); } catch (e) { inner.textContent = '组件设置加载失败：' + e.message; }
  }
}

// 组件激活状态或设置区有变（热开关/注册/注销）→ 浮窗开着就重挂；没开只同步
// 导航项在位情况（内容等下次打开再挂，免得后台空跑组件自己的请求）
window.addEventListener('cx:change', () => {
  const el = $('cxsession');
  if (!el || el.classList.contains('hidden')) return;
  cxsMountComponentSections();
  cxsMountComponentPanes();
  cxsMountPluginSections();
});

// 切会话即收起浮窗：清单是按打开时的会话渲染的，留着会让人把开关拨到
// 「看起来在改、其实改的是新会话」的错位清单上；重开一次就是新会话的清单
if (window.JX && JX.on) {
  JX.on('session:open', (s) => {
    if (cxsNav.open && s && s.id !== cxsForSid) closeCXSessionUI();
  });
}
