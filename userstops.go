package main

// 用户从界面发起的「终止」操作（右侧栏 ✕ 关浏览器页面、进度面板停运行中的
// 程序）：与 AI 工具路径（browser_close / job_kill / 停止会话）共用同一批
// 底层收口，但多两件用户路径专属的事——
//   1. 打「用户关闭」标记 / 写明退出文案，让之后的工具报错与作业状态把
//      「用户主动终止」和「自身崩溃」分开，模型不误判；
//   2. 向所属会话投一条 report 注记（deliverToSessionNoKick：会话空闲只
//      落库、不拉起新一轮；运行中则进排队/插话队列，本轮结束自然看到），
//      模型下次醒来就知道发生了什么，而不是被用户消息触发一次对话。
//
// 关浏览器的收口只此一处：closeSessionBrowser（下方）同时服务右栏 ✕ 与 AI 的
// browser_close——两条路不再是两份近似实现，差异只在「谁发起」的标记与文案。

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"
)

// browserUserClosed 该会话的用户是否在右栏主动关闭过浏览器页面（且此后未
// 再打开）：activeHost（tool_browser.go）据此把「浏览器未在运行」区分为用户
// 主动关闭（非崩溃），避免模型把用户操作误判成异常。按会话记账——A 会话
// 关掉自己的面板不该影响 B 会话的报错口径。
func (a *App) browserUserClosed(sid string) bool {
	a.browserMu.Lock()
	defer a.browserMu.Unlock()
	return !a.browserUserClosedAt[browserProfileKey(sid)].IsZero()
}

// browserCloseBy 关闭发起方。只影响「用户关闭」标记与注记文案，不影响收口
// 动作本身——两条路必须共用同一套收尾：面板可见性记账、browser_nav 复位、
// 另一形态回退各写一份的话，迟早走样。
type browserCloseBy int

const (
	closeByUser browserCloseBy = iota // 右栏浏览器视图 ✕
	closeByAI                         // AI 的 browser_close 工具
)

func (b browserCloseBy) String() string {
	if b == closeByAI {
		return "AI"
	}
	return "用户"
}

// browserCloseResult 收口结果。Closed=false 表示本来就没有在运行的实例（幂等
// 成功）；Fallback 是显式关闭后仍在运行的另一形态实例（AI 路径据此回执
// 「另一实例仍在运行」）。
type browserCloseResult struct {
	Closed   bool
	Where    string // 被关实例的形态描述（target.Where() 的结果，落 [browser-close] 日志便于取证）
	Fallback browserTarget
}

// closeSessionBrowser 是「关闭某个会话的浏览器」的唯一收口：右栏 ✕
// （handleBrowserClose）与 AI 的 browser_close（tool_browser.go）都调它。两条
// 路只有两处不同——
//   - 关哪个目标：用户 ✕ 只关内嵌形态（有头独立窗口自带关闭按钮，用户自己
//     管）；AI 关工具当前作用的实例（activeBrowser 的口径，含另一形态回退）；
//   - 谁发起：用户发起要打「用户关闭」标记 + 投一条 report 注记（模型的错误
//     口径靠它区分「用户主动关闭」与「自身崩溃」）；AI 发起不打标记、不投
//     注记——发起者就是模型自己，工具返回值即回执，再投一条等于让它自说自话。
//
// 其余全部共用：锁外取目标、面板可见性记账清理、browser_nav 复位广播、另一
// 形态实例的存活判定与回退。
func (a *App) closeSessionBrowser(sid string, by browserCloseBy) browserCloseResult {
	var res browserCloseResult
	key := browserProfileKey(sid)
	sid = strings.TrimSpace(sid)
	var target browserTarget
	if by == closeByAI {
		target = a.activeBrowser(sid)
	} else {
		target = a.embeddedTarget(sid) // 锁外取：panelFor/Browser 内部拿锁，持锁调用会死锁
	}
	if target == nil || !target.Started() {
		return res // 本来就没开：幂等成功（不打标记、不投注记）
	}
	res.Closed, res.Where = true, target.Where()
	a.browserMu.Lock()
	if by == closeByUser {
		if a.browserUserClosedAt == nil {
			a.browserUserClosedAt = map[string]time.Time{}
		}
		a.browserUserClosedAt[key] = time.Now()
	}
	_, used := a.browserActive[key] // 注记只投给真正在用浏览器的会话（没开过的对话收到只会一头雾水）
	a.browserMu.Unlock()
	// 面板形态可把「谁发起」写进关闭各阶段日志（[browser-panel] 关闭: …）；
	// 其余形态（有头/无头）没有关闭阶段日志，按接口 Close 即可。
	if rc, ok := target.(interface{ CloseWithReason(string) }); ok {
		rc.CloseWithReason(by.String() + " 发起")
	} else {
		target.Close()
	}
	if a.panelVisibleKey() == key { // 关的是当前显示中的面板：显示记账一并清空
		a.panelSetVisible("")
	}
	a.broadcast(Event{Type: "browser_nav", URL: "", Title: "", SessionID: sid}) // 工具栏地址/标题复位（面板形态 Close 自带广播，无头/有头形态这里统一补）
	a.frameForget(sid) // 远程帧镜像缓存一并清掉：关闭后远程端不该再轮询到旧画面
	log.Printf("[browser-close] 发起=%s 会话=%q 目标=%s", by, sid, res.Where)
	if by == closeByAI {
		res.Fallback = a.activeBrowser(sid) // 显式关闭：回退到另一形态仍在运行的实例
		return res
	}
	if sid != "" && used {
		if m, err := a.Store.Get(sid); err == nil {
			a.deliverToSessionNoKick(m, "【用户关闭浏览器】用户在右栏浏览器面板点 ✕ 关闭了页面（用户主动关闭，非崩溃）。"+
				"浏览器页面已关闭；下次 browser_open 会重新打开，各实例保留的地址与登录态仍在。若你正在依赖该页面的内容，请与用户确认后续。",
				kindReport, "webview")
		}
	}
	return res
}

// handleBrowserClose 右栏浏览器视图 ✕ 的「真关闭」端点：走 closeSessionBrowser
// 的统一收口（标记、注记、可见性清理、复位广播全在那里，与 AI 的 browser_close
// 同一套），这里只负责解析请求与回执。
func (a *App) handleBrowserClose(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Session string `json:"session"`
	}
	_ = json.NewDecoder(r.Body).Decode(&p) // body 可省：没有会话上下文就只关实例
	res := a.closeSessionBrowser(p.Session, closeByUser)
	writeJSON(w, http.StatusOK, map[string]any{"closed": res.Closed})
}

// handleStopChild 进度面板「运行中的程序」的停止端点：用户点名停某一个。
// 两类目标共用同一返回语义（stopped=true / 404）：
//   · 后台作业（job-*）：j.cancel() 优雅取消——与 job_kill、会话停止同一路径；
//   · 前台子进程（child-*）：killChildTree 按进程树强杀——与 stopRun 的硬终结
//     兜底 killSessionChildren 同款手法。
// 停止后投 report 注记告知模型：程序是被用户手动停止的（非自身出错），别误判。
func (a *App) handleStopChild(w http.ResponseWriter, r *http.Request) {
	m := a.sessionFromPath(w, r)
	if m == nil {
		return
	}
	var p struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil || strings.TrimSpace(p.ID) == "" {
		writeErr(w, http.StatusBadRequest, "需要 id")
		return
	}
	id := strings.TrimSpace(p.ID)
	desc := ""
	// 后台作业：先记退出文案再 cancel（setExit 只认首个，晚到的「被终止」不会覆盖）
	a.jobsMu.Lock()
	j := a.jobs[id]
	a.jobsMu.Unlock()
	if j != nil && j.SessionID == m.ID {
		j.setExit("被用户在进度面板终止")
		j.stop(5 * time.Second) // 发终止并限时等进程树死透（结算纪律），超时由顶层 Job 兜底
		desc = "后台作业 " + j.Command + "（" + j.ID + "，已取消）"
	}
	// 前台子进程：按进程树强杀（killSessionChildren 的单点版）
	if desc == "" {
		childRegMu.Lock()
		c := childReg[id]
		childRegMu.Unlock()
		if c == nil || c.SessionID != m.ID || c.Exit() != "" {
			writeErr(w, http.StatusNotFound, "程序不存在或已结束（或属于其他会话）")
			return
		}
		if c.PID > 0 {
			killChildTree(c.PID)
		}
		c.setExit("被用户在进度面板终止")
		desc = "子进程 " + c.Command + "（" + c.Label + "，已强制结束）"
	}
	a.broadcast(Event{Type: "children", SessionID: m.ID}) // 进度面板立即刷新
	a.deliverToSessionNoKick(m, "【用户停止程序】用户在进度面板手动停止了正在运行的"+desc+
		"。这是用户主动操作，不是程序自身出错或崩溃；后台作业已产生的输出仍可用 job_output 读取。如需继续该任务，请先与用户确认。", kindReport, "stop")
	writeJSON(w, http.StatusOK, map[string]any{"stopped": true})
}
