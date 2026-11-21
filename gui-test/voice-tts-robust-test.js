/* 语音组件朗读链健壮性桩测 —— 纯 node，不用起服务。
 *
 * 背景（2026-11-20 用户报障）：「朗读经常读几句就停下，没办法自然续接」。
 * 根因（隔离实例实测）：微软免密钥通道偶发限流（新域名 429 / 后备旧域名
 * 403，20s 间隔低频也有 ~10% 失败率）→ 旧版后端两代通道全挂时回 200 空体
 * → 旧版前端拿 0 字节 blob 播放失败 → stopAudio()+reject 砍整条链；且
 * reject 晚于 stopAudio 内 ttsStopChain 的 resolve 被吞，「朗读中断」
 * toast 是死代码——用户观感就是无声停住。
 *
 * 本文件复刻修复后 组件/voice/client.js 的 ttsPlay / ttsFetchChunk 核心
 * （口径标注行号；时基压缩：重试退避与看门狗余量换算成毫秒级，顺序与
 * 条件分支与真实代码同构），断言：
 *   A. 单块合成失败（重试耗尽）→ 跳过该块继续播完，链尾一次性轻提示；
 *   B. 连续 2 块失败 → 停链且 reject 真正到达 catch（不被 resolve 吞）；
 *   C. onended 不派发（WebView2 长寿命元素换 src 偶发）→ 看门狗续接；
 *   D. 偶发失败重试成功 → 全程无感（无跳过、无提示）；
 *   E. 后端 200 空体（旧版后端兼容）→ 按失败处理，重试后正常；
 *   F. 2026-11-03 的连点竞态不回归（停止→立刻再点，新链正常出声）。
 * 运行：仓库根目录 node gui-test/voice-tts-robust-test.js
 */
'use strict';

// ---------- 桩环境 ----------
const revoked = [];
const URL = {
  createObjectURL: () => 'blob:fake-' + Math.random().toString(36).slice(2, 8),
  revokeObjectURL: (u) => revoked.push(u),
};
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const flush = () => sleep(0);

/** 模拟 <audio>：play() 返回 pending promise；pause() 在 play 未落定前打断时
 * 以 AbortError reject 它（与 Chromium/WebView2 真实行为一致，场景 F 用）。 */
class FakeAudio {
  constructor() {
    this.src = ''; this.onended = null; this.onerror = null; this.onloadedmetadata = null;
    this.duration = NaN; this.currentTime = 0; this._playP = null;
  }
  play() {
    this._playP = new Promise((res, rej) => { this._res = res; this._rej = rej; });
    return this._playP;
  }
  pause() {
    if (this._playP && this._rej) { const r = this._rej; this._res = this._rej = null; r(new Error('AbortError')); }
  }
  started() { if (this._res) { const r = this._res; this._res = this._rej = null; r(); } }
  removeAttribute() { this.src = ''; }
  load() {}
}

/** 一条独立链的全局态与观测（每个场景一套，互不污染）。 */
function harness() {
  const h = {
    playEl: null, playURL: null, ttsAbort: null, ttsStopChain: null,
    plays: [], toasts: [], fetchCalls: [],
    el: null, // 暴露当前 FakeAudio（长寿命单元素）
  };
  h.voiceToast = (msg) => h.toasts.push(msg);

  // fetch 桩：按块序号路由——plan[i] 是数组，每次调用弹出一项：
  //   {ok:blob} / {empty:true}（200 空体）/ {err:'xx'}（网络层失败）
  h.routeFetch = (plan) => {
    const state = plan.map((x) => Array.isArray(x) ? x.slice() : [x]);
    return (i) => {
      h.fetchCalls.push(i);
      const next = state[i] && state[i].length ? state[i].shift() : { ok: true };
      return new Promise((res, rej) => setTimeout(() => {
        if (next.err) return rej(new Error(next.err));
        res({ size: next.empty ? 0 : 1024 }); // 桩 blob：只看 size
      }, 1));
    };
  };

  h.stopAudio = () => {
    if (h.ttsAbort) { h.ttsAbort.abort(); h.ttsAbort = null; }
    if (h.ttsStopChain) { try { h.ttsStopChain(); } catch (e) {} h.ttsStopChain = null; }
    if (h.playEl) {
      h.playEl.onended = h.playEl.onerror = h.playEl.onloadedmetadata = null;
      h.playEl.pause();
      h.playEl.removeAttribute('src');
      try { h.playEl.load(); } catch (e) {}
    }
    if (h.playURL) { URL.revokeObjectURL(h.playURL); h.playURL = null; }
  };

  // ===== 复刻 client.js ttsFetchChunk（时基压缩：[1s,5s,15s] → [1,5,15]ms） =====
  h.ttsFetchChunk = (fetchOne, i, signal) => {
    const delays = [1, 5, 15];
    const attempt = (n) => Promise.resolve(fetchOne(i)).then((b) => {
      if (b && b.size > 0) return b;
      throw new Error('合成了空音频');
    }).catch((e) => {
      if (signal.aborted || n >= delays.length) throw e;
      return sleep(delays[n]).then(() => attempt(n + 1));
    });
    return attempt(0);
  };

  // ===== 复刻 client.js ttsPlay（守卫/推进/reject 顺序同构；看门狗余量压缩） =====
  h.ttsPlay = (chunks, fetchOne) => {
    h.stopAudio();
    h.ttsAbort = new AbortController();
    const signal = h.ttsAbort.signal;
    const blobs = chunks.map(() => null);
    let idx = 0, stopped = false, skipped = 0, consecSkip = 0, curWD = null;
    const prefetch = () => {
      for (let i = idx; i < Math.min(idx + 3, chunks.length); i++) {
        if (!blobs[i]) {
          const p = h.ttsFetchChunk(fetchOne, i, signal);
          p.catch(() => {});
          blobs[i] = p;
        }
      }
    };
    prefetch();
    return new Promise((resolve, reject) => {
      const playChunk = () => {
        if (stopped) return;
        if (idx >= chunks.length) {
          h.stopAudio();
          if (skipped > 0) h.voiceToast('有 ' + skipped + ' 小段合成失败，已跳过继续');
          resolve();
          return;
        }
        const i = idx;
        blobs[i].then((blob) => {
          if (stopped || signal.aborted) return;
          h.playURL = URL.createObjectURL(blob);
          if (!h.playEl) h.playEl = new FakeAudio();
          const el = h.playEl;
          h.el = el;
          el.src = h.playURL;
          let advanced = false, lastT = -1, wdResets = 0;
          const advance = () => {
            if (advanced || stopped) return;
            advanced = true;
            clearTimeout(curWD);
            if (h.playURL) { URL.revokeObjectURL(h.playURL); h.playURL = null; }
            idx += 1;
            prefetch();
            playChunk();
          };
          el.onended = advance;
          // 看门狗（真实代码 +2500ms 余量 / 续等 2.5s → 桩测 30ms / 0.03s）
          const armWD = (sec) => {
            clearTimeout(curWD);
            curWD = setTimeout(() => {
              if (advanced || stopped) return;
              const d = el.duration, t = el.currentTime;
              if (isFinite(d) && d > 0 && t < d - 0.3 && t > lastT && wdResets < 20) {
                wdResets += 1; lastT = t; armWD(0.03);
                return;
              }
              advance();
            }, Math.max(0.01, sec) * 20 + 30);
          };
          el.onloadedmetadata = () => {
            if (advanced || stopped) return;
            if (isFinite(el.duration) && el.duration > 0) armWD(el.duration);
          };
          armWD(chunks[i].length / 4 + 0.15);
          const gone = () => stopped || signal.aborted;
          const failChunk = (msg) => {
            if (gone()) return;
            skipped += 1;
            consecSkip += 1;
            if (consecSkip >= 2) {
              reject(new Error(msg + '（连续 ' + consecSkip + ' 段失败，已停止朗读）'));
              h.stopAudio();
              return;
            }
            advance();
          };
          el.onerror = () => failChunk('播放失败');
          h.plays.push(i); // play 已发起即计入（真实代码同位打点；落定与否不影响推进逻辑）
          el.play().then(() => { consecSkip = 0; }).catch((e) => { if (gone()) return; failChunk('播放失败：' + (e && e.message ? e.message : e)); });
        }).catch((e) => {
          if (signal.aborted || stopped) return;
          skipped += 1;
          consecSkip += 1;
          if (consecSkip >= 2) {
            reject(new Error('朗读服务暂时不可用：' + (e && e.message ? e.message : e)
              + '（连续 ' + consecSkip + ' 段失败，已停止朗读）'));
            h.stopAudio();
            return;
          }
          idx += 1;
          prefetch();
          playChunk();
        });
      };
      h.ttsStopChain = () => { stopped = true; clearTimeout(curWD); resolve(); };
      playChunk();
    });
  };
  return h;
}

/** 驱动播放到落定：每当出现新一块 play 发起，正常路径落定 play、触发 metadata+ended。 */
async function pump(h, p, opts = {}) {
  let settled = false, outcome = null;
  p.then((v) => { settled = true; outcome = { ok: true, v }; })
    .catch((e) => { settled = true; outcome = { ok: false, e }; });
  let pumped = 0;
  const t0 = Date.now();
  while (!settled) {
    if (Date.now() - t0 > 5000) throw new Error('pump 超时（链卡死）');
    await sleep(3);
    const el = h.el;
    if (el && h.plays.length > pumped) {
      pumped = h.plays.length;
      el.started(); // 音频设备就绪，play() 落定
      if (opts.noEnd) continue; // 场景 C：吞掉 ended，只给 metadata
      el.duration = 1; el.currentTime = 1;
      if (el.onloadedmetadata) el.onloadedmetadata();
      el.currentTime = 1;
      if (el.onended) el.onended();
    }
  }
  await flush();
  return outcome;
}

(async () => {
  let fail = 0;
  const t = (name, ok, detail) => {
    console.log((ok ? '  ok  ' : '  FAIL') + ' ' + name + (ok ? '' : '  <- ' + detail));
    if (!ok) fail++;
  };
  const CHUNKS = ['块零零零零。', '块一一一一。', '块二二二二。', '块三三三三。', '块四四四四。', '块五五五五。'];

  // A. 单块合成失败（重试耗尽）→ 跳过继续，链尾一次性提示
  {
    const h = harness();
    const fetchOne = h.routeFetch([{ ok: 1 }, { ok: 1 }, [{ err: 'HTTP 502' }, { err: 'HTTP 502' }, { err: 'HTTP 502' }, { err: 'HTTP 502' }], { ok: 1 }, { ok: 1 }, { ok: 1 }]);
    const out = await pump(h, h.ttsPlay(CHUNKS, fetchOne));
    t('A 链正常落定（resolved）', out.ok === true, JSON.stringify(out));
    t('A 播放顺序跳过失败块 [0,1,3,4,5]', JSON.stringify(h.plays) === JSON.stringify([0, 1, 3, 4, 5]), JSON.stringify(h.plays));
    t('A 链尾一次性轻提示', h.toasts.length === 1 && h.toasts[0].includes('1 小段'), JSON.stringify(h.toasts));
    t('A 失败块重试了 3 次（共 4 次调用）', h.fetchCalls.filter((x) => x === 2).length === 4, JSON.stringify(h.fetchCalls));
  }

  // B. 连续 2 块失败 → 停链，且 reject 真正到达 catch（不被 resolve 吞）
  {
    const h = harness();
    const fetchOne = h.routeFetch([{ ok: 1 }, { ok: 1 }, [{ err: '429' }, { err: '429' }, { err: '429' }, { err: '429' }], [{ err: '403' }, { err: '403' }, { err: '403' }, { err: '403' }], { ok: 1 }, { ok: 1 }]);
    const out = await pump(h, h.ttsPlay(CHUNKS, fetchOne));
    t('B 链以 reject 落定', out.ok === false, JSON.stringify(out && out.e && out.e.message));
    t('B 错误消息带上「已停止朗读」', out.e && /已停止朗读/.test(out.e.message), out.e && out.e.message);
    t('B 只播到第 2 块', JSON.stringify(h.plays) === JSON.stringify([0, 1]), JSON.stringify(h.plays));
  }

  // C1. onended 完全不派发（metadata 正常、currentTime 推到尾）→ 看门狗续接
  {
    const h = harness();
    const fetchOne = h.routeFetch([{ ok: 1 }, { ok: 1 }, { ok: 1 }, { ok: 1 }]);
    const P = h.ttsPlay(CHUNKS.slice(0, 3), fetchOne);
    // 吞掉 ended：metadata 到位 + currentTime 推到 duration，但不触发 onended
    let settled = false; P.then(() => settled = true, () => settled = true);
    const t0 = Date.now();
    while (!settled && Date.now() - t0 < 5000) {
      await sleep(3);
      const el = h.el;
      if (el && h.plays.length && el.onloadedmetadata && isFinite(el.duration) === false) {
        el.duration = 0.5;
        el.onloadedmetadata(); // 看门狗重武装为 0.5*20+30=40ms
      }
      if (el && el.onended) { el.currentTime = Math.min((el.currentTime || 0) + 0.05, el.duration); } // 时间在走
    }
    t('C1 ended 丢失仍能播完全部块', settled && JSON.stringify(h.plays) === JSON.stringify([0, 1, 2]), JSON.stringify(h.plays) + ' settled=' + settled);
    t('C1 无跳过无提示', h.toasts.length === 0, JSON.stringify(h.toasts));
  }

  // C2. 播放停滞（currentTime 不动）→ 看门狗两次续等后推进
  {
    const h = harness();
    const fetchOne = h.routeFetch([{ ok: 1 }, { ok: 1 }]);
    const P = h.ttsPlay(CHUNKS.slice(0, 2), fetchOne);
    let settled = false; P.then(() => settled = true, () => settled = true);
    const t0 = Date.now();
    while (!settled && Date.now() - t0 < 5000) {
      await sleep(3);
      const el = h.el;
      if (el && h.plays.length && el.onended && isFinite(el.duration) === false) {
        el.duration = 0.5;
        if (el.onloadedmetadata) el.onloadedmetadata();
        // currentTime 保持 0：第一次看门狗续等（0>lastT=-1），第二次 t>lastT 不成立 → advance
      }
    }
    t('C2 停滞也能续接播完', settled && JSON.stringify(h.plays) === JSON.stringify([0, 1]), JSON.stringify(h.plays) + ' settled=' + settled);
  }

  // D. 偶发失败重试成功 → 全程无感
  {
    const h = harness();
    const fetchOne = h.routeFetch([{ ok: 1 }, [{ err: '429' }, { ok: 1 }], { ok: 1 }, { ok: 1 }, { ok: 1 }, { ok: 1 }]);
    const out = await pump(h, h.ttsPlay(CHUNKS, fetchOne));
    t('D 全部块按序播完', out.ok === true && JSON.stringify(h.plays) === JSON.stringify([0, 1, 2, 3, 4, 5]), JSON.stringify(h.plays));
    t('D 无跳过提示（无感重试）', h.toasts.length === 0, JSON.stringify(h.toasts));
  }

  // E. 后端 200 空体（旧版后端兼容）→ 按失败处理，重试后正常
  {
    const h = harness();
    const fetchOne = h.routeFetch([[{ empty: true }, { empty: true }, { ok: 1 }], { ok: 1 }, { ok: 1 }, { ok: 1 }]);
    const out = await pump(h, h.ttsPlay(CHUNKS.slice(0, 4), fetchOne));
    t('E 空体两次后重试成功，全部播完', out.ok === true && JSON.stringify(h.plays) === JSON.stringify([0, 1, 2, 3]), JSON.stringify(h.plays));
    t('E 无跳过提示', h.toasts.length === 0, JSON.stringify(h.toasts));
  }

  // F. 2026-11-03 连点竞态不回归：A 链 play pending 中被停 → 立刻 B 链
  {
    const h = harness();
    // A 只有一块；B 两块。A 的 fetch 立刻成功，play() 挂起 pending。
    const planA = { 0: [{ ok: 1 }] };
    const planB = { 0: [{ ok: 1 }], 1: [{ ok: 1 }] };
    const mk = (plan) => (i) => {
      h.fetchCalls.push('AB' + i);
      const q = plan[i] || [{ ok: 1 }];
      const first = q.shift();
      return new Promise((res, rej) => setTimeout(() => {
        if (first.err) return rej(new Error(first.err));
        res({ size: first.empty ? 0 : 512 });
      }, 1));
    };
    const pA = h.ttsPlay(['A块。'], mk(planA));
    let aSettled = false; pA.then(() => aSettled = true, () => aSettled = true);
    await sleep(5); // A 的 fetch 已完成，进入 then：src 已设、play() pending
    h.stopAudio();  // 用户点停止（A 的 play promise 排队 AbortError reject）
    const pB = h.ttsPlay(['B0块。', 'B1块。'], mk(planB)); // 立刻再点朗读
    await flush();  // 旧 play reject 微任务落地（新代码：gone() 让路）
    h.el.started(); // B 的 play() 落定
    await sleep(5);
    const bAborted = h.ttsAbort ? h.ttsAbort.signal.aborted : null;
    // 推完 B 的两块
    let settled = false; pB.then(() => settled = true, () => settled = true);
    const t0 = Date.now();
    while (!settled && Date.now() - t0 < 3000) {
      await sleep(3);
      if (h.el && h.plays.length) {
        h.el.started();
        h.el.duration = 0.4;
        if (h.el.onloadedmetadata) h.el.onloadedmetadata();
        h.el.currentTime = 0.4;
        if (h.el.onended) h.el.onended();
        await sleep(2);
      }
    }
    t('F A（被停）安静落定', aSettled === true, 'aSettled=' + aSettled);
    t('F B 的 AbortController 未被误杀', bAborted === false, 'bAborted=' + bAborted);
    t('F B 两块都出声（竞态不回归）', settled && h.plays.length >= 2, JSON.stringify(h.plays));
  }

  // G. play() 被连续拒绝（无手势/自动播放策略）→ 停链报错，而不是无声快进到尾
  {
    const h = harness();
    const fetchOne = h.routeFetch([{ ok: 1 }, { ok: 1 }, { ok: 1 }, { ok: 1 }, { ok: 1 }, { ok: 1 }]);
    // play() 永远 AbortError/NotAllowedError：pump 不调 started()，pause() 打断
    // 但这里没有 pause——直接让 play() reject：
    const P = h.ttsPlay(CHUNKS, fetchOne);
    let settled = false, err = null;
    P.then(() => settled = true, (e) => { settled = true; err = e; });
    // 拦截：每块 play 发起后立即以 NotAllowedError reject
    const t0 = Date.now();
    while (!settled && Date.now() - t0 < 3000) {
      await sleep(2);
      const el = h.el;
      if (el && el._rej && h.plays.length) { const r = el._rej; el._res = el._rej = null; r(new Error('NotAllowedError')); }
    }
    t('G play 连续被拒 → 链以 reject 停止', settled && err && /播放失败/.test(err.message), err && err.message);
    t('G 不是无声快进（只试了 2 块）', h.plays.length === 2, JSON.stringify(h.plays));
  }

  console.log(fail === 0 ? '\n全部断言通过。' : '\n' + fail + ' 条断言失败');
  process.exit(fail === 0 ? 0 : 1);
})();
