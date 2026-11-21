/* CX：组件运行时 —— 发现、注入、注册、热开关。
 *
 * 组件是本栈原生的轻量外围扩展（组件/<名字>/：manifest.json + client.js），
 * 与插件（插件兼容层，DSH 适配）相互独立。本运行时随页面加载：
 *   1. boot：GET /api/components，把已启用组件的 js/css 注入页面；
 *   2. 组件脚本执行时调用 CX.register({id, init, dispose}) 注册自身，
 *      init() 立即执行；dispose() 是热关闭的关键——撤销 init 的全部副作用；
 *   3. 设置页开关 → CX.toggle(id, on)：
 *      开 = 重新注入脚本（立即生效）；
 *      关 = 调 dispose() + 移除标签。组件没提供 dispose 时返回 needsRefresh。
 * 全程不需要刷新页面、不需要重启服务。
 *
 * 组件设置区：组件在 init 里调 CX.registerSettings({id, title, mount}) 把自己的
 * 设置区交给 CX 保管；设置页（「组件」页）按 CX.settingsSections() 渲染，
 * 组件激活状态一变（注册/注销/热开关）CX 就派发 window 事件 cx:change，设置页
 * 收到后重挂——于是关掉组件时它那段设置连同按钮一起消失。
 *
 * 会话级组件配置（合并层，2026-11-08 定稿）：本运行时拦组件自己的存储命名空间
 * localStorage['cx:<组件id>:*']——当前会话「会话独立」时，读命中会话覆盖、
 * 未覆盖的键回落全局值，写只落会话覆盖（与全局值相同即不记覆盖）。组件零改动，
 * 它读到的永远是该会话视角的值；不注入对话的设置会话化也无感知（自动覆盖）。
 * 覆盖记录存会话附属数据（sessioncx.go / sessioncxcfg.go），切会话时重新拉齐。 */

(function () {
  // 同一 window 里文档被整页替换（配对页 gate 的 document.open/write 换页、离线缓存
  // 兜底换页）时，window.CX 还活着但 document 已是新的：旧 CX 绑的是旧文档的 DOM，
  // 组件按钮在新文档里全都没有（语音按钮回落到 .hidden、待办按钮没人注入）。
  // 只认「同一文档」的 CX——跨文档就重新初始化、在新文档上重挂组件（2026-10-21）。
  if (window.CX && window.CX._doc === document) return;

  // 诊断打点（后端 /api/diag 落一行日志，只用于组件加载链路排障）。
  // 走 fetch 而不是 1px 图片：远程（手机）模式只有 fetch 会被 remote-boot 劫持进
  // 加密隧道，图片请求打到网关源上永远到不了电脑端日志——组件在手机上不显示时，
  // 这是唯一能把现场带回来的路子。（cx.js 先于其它脚本执行，不吃 jxDiag。）
  function cxDiag(msg) {
    try {
      fetch('/api/diag?m=' + encodeURIComponent('[CX] ' + msg) + '&t=' + Date.now(),
        { keepalive: true, cache: 'no-store' }).catch(function () { /* 静默 */ });
    } catch (e) { /* 静默 */ }
  }

  const CX = {
    _doc: document,   // 归属文档：换页后要重新初始化（见上方守卫）
    _ok: false,       // 清单是否至少成功拉到过一次（成功拉空也置位，不空转重试）
    _booting: false,  // 一轮清单拉取正在进行（可见/网络/隧道/pageshow 可能同时触发）
    _list: [],        // /api/components 的清单（带会话查询时＝该会话的生效视图）
    _active: {},      // id -> { dispose }（已 init 的组件）
    _gen: {},         // id -> 代数（防快速反复开关的旧请求串台）
    _failed: {},      // id -> 上次失败时间戳（组件没起来过，等退避后重放）
    _sections: [],    // {id, title, mount}：组件设置区（设置页「组件」渲染）
    _session: '',     // 当前会话id（''＝无会话，按全局开关装配；随 session:open 切换）
    _cfg: { scope: false, components: {} }, // 本会话的组件配置视图（总开关+每组件覆盖）
    _cfgSession: '',  // _cfg 属于哪个会话（切会话要整段重装配）
    _listGen: 0,      // 清单拉取代数（切会话与 boot 并发时，旧的晚到响应作废）
    sessionCustom: false, // 当前会话是否定制过组件/插件生效集（加号菜单的 ✓）

    // 清单地址：带当前会话 → 该会话的生效视图（会话覆盖优先于全局开关）
    _listURL() {
      return '/api/components' + (this._session ? '?s=' + encodeURIComponent(this._session) : '');
    },

    // 当前会话 id（''＝无会话/全局视角）。组件（如 user-memory 的会话独立
    // 记忆）用它给自己的按会话读写接口拼 ?s= 参数。
    session() {
      return this._session || '';
    },

    // 会话切换（含回首页＝空串）：记录会话并拉新清单对账——本会话关掉的组件
    // 热卸载、本会话额外打开的热注入，与设置页热开关同一条注入/卸载路径。
    setSession(id) {
      id = id || '';
      if (this._session === id) return;
      this._session = id;
      // 会话切换事件：按会话视角自管状态的组件（如 user-memory 的气泡颜色）
      // 监听它重取自己的值；与 cx:change（生效集变化）互补。
      try { window.dispatchEvent(new CustomEvent('cx:session', { detail: { session: id } })); } catch (e) { /* 老引擎忽略 */ }
      if (!id) {
        this.sessionCustom = false;
        this._cfg = { scope: false, components: {} }; // 回首页：全局视角（也解掉旧会话的拦截）
      }
      this.refresh();
    },

    async boot(tryN) {
      if (this._booting) return; // 同一时刻只跑一轮：pageshow/可见/网络恢复常一起到
      this._booting = true;
      tryN = tryN || 0;
      let data = null;
      const gen = ++this._listGen;
      const url = this._listURL();
      try {
        const res = await fetch(url);
        if (!res.ok) throw new Error('HTTP ' + res.status);
        data = await res.json();
      } catch (e) {
        // 远程模式首屏经中继/直连拉组件清单，手机在后台被系统重建页面、中继未就绪
        // 时这一拉会失败——过去一次失败就整个页面生命周期没有组件（按钮全不见、
        // 页面其余功能却正常，2026-10-21 手机端实测反馈）。这里退避重试，并在
        // 页面可见/网络恢复/隧道就绪时自愈（见下方监听）。
        if (gen !== this._listGen) { this._booting = false; return; } // 切会话已另起一轮
        console.warn('[CX] 清单读取失败（第 ' + (tryN + 1) + ' 次）', e);
        cxDiag('boot 清单读取失败 #' + (tryN + 1) + ' ' + (e && e.message || e));
        this._booting = false;
        this._bootRetry(tryN);
        return;
      }
      this._booting = false;
      if (gen !== this._listGen) return; // 等待期间切了会话：setSession 的 refresh 接管
      this._ok = true;
      this._setCfg(data && data.cfg); // 配置先就位：组件的 init 第一口读到的就是会话视角
      this._applyList((data && data.components) || []);
      this.sessionCustom = !!(data && data.sessionCustom);
    },

    /** 清单没拉到就退避重试（1.5s→20s，最多 5 轮）；拉到过一次就停，不空转 */
    _bootRetry(tryN) {
      if (tryN >= 5) { cxDiag('boot 连续 5 轮失败，等页面可见/网络恢复再试'); return; }
      const waits = [1500, 3000, 6000, 12000, 20000];
      const w = waits[tryN] || 20000;
      setTimeout(() => {
        if (this._doc !== document) return;      // 换页了：新文档的 CX 自己会拉
        if (this._ok) return;                    // 期间已经拉到了
        this.boot(tryN + 1);
      }, w);
    },

    /** 自愈（页面回到前台 / 网络恢复 / 隧道就绪 / pageshow / 30s 兜底）：
     *  - 清单还没成功拉到过 → 重新 boot（沿用上面的退避）；
     *  - 清单拉到了、但某个组件的 client.js 当时没抓下来 → 退避后重放它。
     * 只碰「自己没起来过」的组件：用户主动关掉的（清单里 enabled=false）、已经从
     * 清单消失的，一律不进重放；已在跑的组件由 register() 幂等吸收、不会重复 init。
     * 所以组件热启动/热关闭的语义一行没变，这里补的只是「本该出现却没起来」。 */
    heal() {
      if (this._doc !== document) return;   // 换页了：新文档的 CX 自己会拉
      if (!this._ok) { this.boot(0); return; }
      this._replayFailed();
    },

    /** 重放「没起来过」的组件（退避窗口 3s，避免抖动风暴；全部就绪时零请求） */
    _replayFailed() {
      const now = Date.now();
      for (const id of Object.keys(this._failed)) {
        const c = this._manifest(id);
        if (!c || !c.enabled) { delete this._failed[id]; continue; } // 关掉的/没了的：不再提
        if (now - this._failed[id] < 3000) continue;
        delete this._failed[id];
        cxDiag('重放组件 ' + id);
        this._inject(c);
      }
    },

    _manifest(id) {
      return this._list.find((c) => c.id === id) || null;
    },

    /** 资产拉取（带重试）：远程走中继，首屏抖动一次不该让组件永久缺席 */
    _fetchText(url, tries) {
      tries = tries == null ? 3 : tries;
      return (async () => {
        let lastErr = null;
        for (let i = 0; i < tries; i++) {
          try {
            const r = await fetch(url);
            if (!r.ok) throw new Error('HTTP ' + r.status);
            return await r.text();
          } catch (e) {
            lastErr = e;
            if (i < tries - 1) await new Promise((res) => setTimeout(res, 600 * (i + 1)));
          }
        }
        throw lastErr || new Error('fetch failed');
      })();
    },

    // 注入一个组件的资产（带代数防串台；重注入前先清旧标签）。
    // 资产统一经 /api/components/ 用 fetch 拿文本再注入（CSS 进 <style>、JS 用
    // text 立即执行）：本地 fetch 直连本机服务；远程模式下 /api/ 前缀被
    // remote-boot 的 fetch 劫持自动封帧走中继——组件加载因此天然兼容远程，
    // 不需要任何远程特判。（旧做法插相对 src 的 <link>/<script> 标签，远程时
    // 相对网关 origin 解析必 404，组件全体消失。）异步回来先查代数，快速开关
    // 时旧请求作废，防止旧代码覆盖新一轮注入。
    _inject(c) {
      if (!c) return;
      const gen = (this._gen[c.id] = (this._gen[c.id] || 0) + 1);
      this._removeTags(c.id);
      const base = '/api/components/' + c.id + '/';
      // 资产带上当前会话：会话把全局关着的组件打开时，资产按会话生效集放行
      const sessQ = this._session ? '&s=' + encodeURIComponent(this._session) : '';
      // 抓取失败登记一次（供 heal 的 _replayFailed 重放）：组件不是页面的一部分，
      // 这一轮抓不到它就等于整页生命周期都没有它——主体不会因此缺席，组件会，
      // 于是表现为「只剩主体功能」。只登记自己这一代的那次失败。
      const miss = (what, e) => {
        if (this._gen[c.id] !== gen || this._doc !== document) return;
        this._failed[c.id] = Date.now();
        console.warn('[CX] 组件 ' + c.id + ' ' + what + '加载失败', e);
        cxDiag(c.id + ' ' + what + ' 加载失败 ' + (e && e.message || e));
      };
      if (c.hasCss) {
        this._fetchText(base + 'client.css?t=' + gen + sessQ)
          .then((css) => {
            if (this._gen[c.id] !== gen || this._doc !== document) return;
            const style = document.createElement('style');
            style.dataset.cx = c.id;
            style.textContent = css;
            document.head.appendChild(style);
          })
          .catch((e) => miss('css ', e));
      }
      if (c.hasJs) {
        this._fetchText(base + 'client.js?t=' + gen + sessQ)
          .then((code) => {
            if (this._gen[c.id] !== gen || this._doc !== document) return;
            delete this._failed[c.id]; // 起来了：以后不再重放它
            const s = document.createElement('script');
            s.dataset.cx = c.id;
            // sourceURL 让报错堆栈按组件文件名显示（text 注入默认是匿名 inline）
            s.text = code + '\n//# sourceURL=' + base + 'client.js';
            document.body.appendChild(s);
          })
          .catch((e) => miss('js ', e));
      }
    },

    _removeTags(id) {
      document.querySelectorAll('[data-cx="' + id + '"]').forEach((el) => el.remove());
    },

    // 组件脚本调用：注册并立即 init（重复注册按新 dispose 覆盖、不重复 init）
    register(def) {
      if (!def || !def.id) return;
      if (this._active[def.id]) {
        // 已激活（脚本因重新注入再次执行）：只刷新 dispose 引用
        this._active[def.id].dispose = def.dispose || null;
        return;
      }
      this._active[def.id] = { dispose: def.dispose || null };
      this._signal(); // 激活状态变了：通知设置页重挂（含 init 里刚注册的设置区）
      if (def.init) {
        try { def.init(); } catch (e) { console.warn('[CX] 组件 ' + def.id + ' init 失败', e); }
      }
    },

    // 设置页开关：POST 状态 + 前端即时加载/卸载。带 session＝会话级覆盖
    // （本对话开关浮窗用：只写该会话的覆盖记录，全局开关不动），前端路径不变。
    async toggle(id, on, session) {
      const body = { id, enabled: on };
      if (session) body.session = session;
      const res = await fetch('/api/components', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (!res.ok) {
        const d = await res.json().catch(() => ({}));
        throw new Error(d.error || res.statusText);
      }
      const d = await res.json().catch(() => ({}));
      if (session && d) this.sessionCustom = !!d.sessionCustom;
      const c = this._manifest(id);
      if (c) { // 清单跟着更新：兜底提示与组件互查看的就是它
        c.enabled = !!on;
        c.override = session ? on !== c.globalEnabled : false;
      }
      delete this._failed[id]; // 开关一次就作废旧的重放登记：关掉的不许被重放复活
      if (on) {
        this._inject(c);
        return { hot: true };
      }
      // 关闭：有 dispose 则热卸载，否则提示刷新
      const rec = this._active[id];
      if (rec && typeof rec.dispose === 'function') {
        try { rec.dispose(); } catch (e) { console.warn('[CX] 组件 ' + id + ' dispose 失败', e); }
        delete this._active[id];
        this._signal(); // 卸载完成：设置页重挂，该组件的设置区随之消失
        this._removeTags(id);
        return { hot: true };
      }
      this._removeTags(id);
      delete this._active[id];
      this._signal(); // 卸载完成：设置页重挂，该组件的设置区随之消失
      return { hot: false };
    },

    // 重拉清单并对账（市场安装/卸载组件后由 skill-market 组件调用；切会话时
    // 由 setSession 调用）：新出现且启用的组件立即注入、消失或不再启用的组件
    // 热卸载，全程不刷新页面。对账看两个维度——清单成员（装/卸）与生效开关
    // （会话切换带来的开/关），后者是会话级覆盖的装配路径。
    async refresh() {
      const gen = ++this._listGen;
      const sessionChanged = this._cfgSession !== this._session;
      let fresh;
      try {
        const res = await fetch(this._listURL());
        if (!res.ok) throw new Error('HTTP ' + res.status);
        const data = await res.json();
        fresh = (data && data.components) || [];
        if (gen === this._listGen) {
          this.sessionCustom = !!(data && data.sessionCustom);
          this._setCfg(data && data.cfg); // 会话视角的组件配置（总开关 + 覆盖）
        }
      } catch (e) {
        console.warn('[CX] 清单刷新失败', e);
        return;
      }
      if (gen !== this._listGen) return; // 已被更新的拉取顶掉：晚到的丢弃
      // 切了会话：已装的组件带的是上个会话的配置，全部卸掉按新会话重装
      // （_applyList 的「已在跑就不动」只适用于同一会话内的增删对账）
      if (sessionChanged) {
        for (const id of Object.keys(this._active)) this._deactivate(id);
      }
      // 瞬时空清单（远程中继抖动 / 服务端正扫目录）不等于「组件全被删了」——
      // 照单全收会把所有组件 dispose 掉且再也没有补回的机会（按钮消失的另一种来路），
      // 这里当作异常跳过本轮对账（2026-10-21）。
      if (!fresh.length && this._list.length) {
        console.warn('[CX] 清单返回空，疑似瞬时异常，跳过本轮对账');
        cxDiag('refresh 返回空清单，跳过对账');
        return;
      }
      this._applyList(fresh);
    },

    // 清单对账（refresh/boot 收口处）：把页面装配到 fresh 描述的状态。
    // 两个维度都看：清单成员（市场装/卸）与生效开关（会话切换带来的开/关，
    // 后者是会话级覆盖的装配路径），卸载/注入与设置页热开关同一条路。
    _applyList(fresh) {
      const prev = new Map(this._list.map((c) => [c.id, c]));
      const next = new Map(fresh.map((c) => [c.id, c]));
      // 卸载：清单里消失的，或本会话不再启用的
      for (const [id, was] of prev) {
        const now = next.get(id);
        if (now && now.enabled) continue;
        if (was.enabled || this._active[id]) this._deactivate(id);
      }
      this._list = fresh;
      // 注入：启用的组件里还没起来的（新装、本会话新启用、此前抓取失败）
      for (const c of fresh) {
        if (c.enabled && !this._active[c.id]) this._inject(c);
      }
      this._signal(); // 增删/启停都会影响设置页「组件」清单与组件设置区
    },

    /* ---------- 会话级组件配置（合并层） ---------- */

    // 装上配置视图（boot/refresh 从 /api/components?s= 的 cfg 字段来）
    _setCfg(cfg) {
      const base = { scope: false, components: {} };
      this._cfg = (cfg && cfg.components) ? cfg : base;
      this._cfgSession = this._session;
      return this._cfg;
    },

    // 外部换掉配置视图（浮窗改完 mode/scope 后拿接口回包调它），并可指定
    // 需要「重读配置」的组件（dispose + 重新 init：组件自己重新读一遍存储）。
    applyCfg(cfg, reloadIds) {
      this._setCfg(cfg);
      for (const id of (reloadIds || [])) this.reloadComponent(id);
      this._signal(); // 浮窗/设置页按新视图重渲染
      return this._cfg;
    },

    // 全局设置页开着时不走会话视角：那里的读写是「全局值」，与浮窗里的会话
    // 视角分开（两块界面不会同时用，按可见性判定足够）。
    _cfgBypassNow() {
      try {
        const el = document.getElementById('settings');
        return !!(el && !el.classList.contains('hidden'));
      } catch (e) { return false; }
    },

    // 某组件在本会话的配置覆盖（null＝跟随全局，读写真存储）。缺省＝独立：
    // 总开关开着而又没显式标记「跟随全局」时即为会话独立（总开关即一键切全场）。
    _cfgFor(id) {
      const c = this._cfg;
      if (!c || !c.scope) return null;
      if (this._cfgBypassNow()) return null; // 全局设置页：写全局值
      const e = (c.components || {})[id];
      if (e && e.on === false) return null;
      if (!e) { // 清单里还没有它的条目（组件刚装）：独立、无覆盖
        this._cfg.components = this._cfg.components || {};
        this._cfg.components[id] = { on: true, values: {} };
        return this._cfg.components[id].values;
      }
      if (!e.values) e.values = {};
      return e.values;
    },

    // 组件存储键 → 组件id（只认 cx:<组件id>:<键> 这一层命名空间）
    _cxKeyComp(key) {
      if (typeof key !== 'string' || key.slice(0, 3) !== 'cx:') return '';
      const i = key.indexOf(':', 3);
      return i > 3 ? key.slice(3, i) : '';
    },

    // 写一条会话覆盖（value=null 表示删；失败只告警——组件自己的内存态已经对了）
    _cfgPost(id, key, value) {
      if (!this._session) return;
      try {
        fetch('/api/components/cfg', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ session: this._session, id, key, value }),
        }).then((r) => { if (!r.ok) console.warn('[CX] 会话配置写入失败', r.status); })
          .catch((e) => console.warn('[CX] 会话配置写入失败', e));
      } catch (e) { /* 忽略：下一次读取仍按内存覆盖走 */ }
    },

    // 装读取通道：拦 localStorage 的 cx:<组件id>:* 三个动词（组件零改动）。
    // 会话独立 → 命中会话覆盖、未覆盖的键回落全局值；写只落会话覆盖（与全局
    // 值相同即不记，恢复语义「跟随全局」靠删记录实现）。跟随全局 → 原样透传。
    _installCfgChannel() {
      if (this._cfgChannel) return;
      const self = this;
      let ls = null;
      try { ls = window.localStorage; } catch (e) { return; } // 隐私模式/被禁用
      if (!ls || !ls.getItem) return;
      const oGet = ls.getItem.bind(ls);
      const oSet = ls.setItem.bind(ls);
      const oRem = ls.removeItem.bind(ls);
      this._cfgChannel = { get: oGet, set: oSet, remove: oRem };
      ls.getItem = function (key) {
        const comp = self._cxKeyComp(key);
        if (comp) {
          const vals = self._cfgFor(comp);
          if (vals && Object.prototype.hasOwnProperty.call(vals, key)) return vals[key];
        }
        return oGet(key);
      };
      ls.setItem = function (key, val) {
        const comp = self._cxKeyComp(key);
        const vals = comp ? self._cfgFor(comp) : null;
        const str = String(val);
        if (!vals) { oSet(key, str); return; }
        if (oGet(key) === str) { // 与全局值一致：不记覆盖（跟随全局的同一份值）
          if (Object.prototype.hasOwnProperty.call(vals, key)) {
            delete vals[key];
            self._cfgPost(comp, key, null);
          }
          return;
        }
        vals[key] = str;
        self._cfgPost(comp, key, str);
      };
      ls.removeItem = function (key) {
        const comp = self._cxKeyComp(key);
        const vals = comp ? self._cfgFor(comp) : null;
        if (!vals) { oRem(key); return; }
        // 会话独立时删键＝删掉会话覆盖，读取回落到全局值
        if (Object.prototype.hasOwnProperty.call(vals, key)) {
          delete vals[key];
          self._cfgPost(comp, key, null);
        }
      };
    },

    // 重装一个组件（配置视图变了要让它重新读一遍存储）：dispose + 重新注入
    reloadComponent(id) {
      const c = this._manifest(id);
      if (!c || !c.enabled) return false;
      this._deactivate(id);
      this._inject(c);
      return true;
    },

    reloadAll() {
      let n = 0;
      for (const c of this._list) if (c.enabled && this.reloadComponent(c.id)) n++;
      return n;
    },

    // 热卸载一个组件（对账与开关共用）：dispose + 摘标签 + 清登记
    _deactivate(id) {
      const rec = this._active[id];
      if (rec && typeof rec.dispose === 'function') {
        try { rec.dispose(); } catch (e) { console.warn('[CX] 组件 ' + id + ' dispose 失败', e); }
      }
      delete this._active[id];
      delete this._failed[id];
      this._removeTags(id);
    },

    // 组件交出（或更新）自己的设置区；通常在 init 里调用。
    // 设置区随组件激活状态显示：没激活的组件那段不会渲染出来。
    // def 可带 save: async () => boolean——接入设置页底部「保存」的统一落库
    // 流程（改动暂存在 mount 出的 DOM 里，全局保存时被 CX.saveSettingsSections
    // 收集；返回 false 或抛错 = 保存失败，设置窗不关、用户就地修正）。
    // 组件自己的设置区不该再放保存按钮（设置页右下角已有全局保存）。
    registerSettings(def) {
      if (!def || !def.id || typeof def.mount !== 'function') return;
      const i = this._sections.findIndex((s) => s.id === def.id);
      if (i >= 0) this._sections[i] = def; else this._sections.push(def);
      this._signal();
    },

    unregisterSettings(id) {
      const i = this._sections.findIndex((s) => s.id === id);
      if (i >= 0) this._sections.splice(i, 1);
      this._signal();
    },

    // 当前该渲染的组件设置区（只有激活中的组件；设置页照这个清单排）
    settingsSections() {
      return this._sections.filter((s) => this.isActive(s.id));
    },

    // 设置页底部「保存」的组件链：逐个调激活组件注册项的 save（若有），
    // 全部成功才返回 true。任一失败即短路返回 false——saveSettings 不关窗。
    // （「本对话的组件与插件」浮窗没有全局保存：那份挂载的改动停留在草稿。）
    async saveSettingsSections() {
      for (const s of this.settingsSections()) {
        if (typeof s.save !== 'function') continue;
        try {
          if (!(await s.save())) return false;
        } catch (e) {
          console.error('[cx] 组件设置保存失败:', s.id, e);
          return false;
        }
      }
      return true;
    },

    // 激活状态/设置区有变 → 通知页面（设置页监听 cx:change 重挂组件设置区）
    _signal() {
      try { window.dispatchEvent(new CustomEvent('cx:change')); } catch (e) { /* 老引擎忽略 */ }
    },

    // 当前是否已激活（供组件互查）
    isActive(id) { return !!this._active[id]; },
  };

  CX._installCfgChannel(); // 先装读取通道：组件的第一口存储读就按会话视角走
  window.CX = CX;
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', () => CX.boot());
  } else {
    CX.boot();
  }

  // 会话切换（app-chat.js 的 session:open：主对话区每成功打开一个会话）→
  // 按新会话的生效集装配（会话覆盖优先于全局开关）；回首页时由 app-chat.js
  // 直接调 CX.setSession('') 回到全局集。cx.js 在全部 app 脚本之后加载，
  // JX 事件总线此刻已就位。
  if (window.JX && JX.on) {
    JX.on('session:open', (s) => { if (s && s.id) CX.setSession(s.id); });
  }

  // 自愈：手机端页面常在后台被系统重建（WebView 渲染进程回收/换页），那一刻中继
  // 可能未就绪，首拉清单/抓组件脚本会失败——过去失败就永久没有组件（语音、待办
  // 入口全不见，页面其余功能却正常）。这里的自愈挂在「页面生命周期」上，跟主体
  // 请求同一套时机：页面回到前台、网络恢复、bfcache 恢复（pageshow）、隧道就绪
  // （remote-boot 的 jx:tunnel）、以及每 30 秒兜底。已经全部就绪时零请求零开销。
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) CX.heal();
  });
  window.addEventListener('online', () => CX.heal());
  window.addEventListener('pageshow', () => CX.heal());
  window.addEventListener('jx:tunnel', () => CX.heal());
  setInterval(() => CX.heal(), 30000);
})();
