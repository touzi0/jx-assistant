package main

// 浏览器宿主（chromedp，CDP 纯 Go 驱动系统自带的 Edge/Chrome）：
//   * headed：有头独立窗口（旧版行为）——用户在真实窗口里操作，AI 共用；
//   * headless：网页形态下充当「内嵌」目标的后台无头实例，仅供 AI 的
//     browser_* 工具使用（桌面形态的内嵌目标是右栏原生 WebView2 面板，
//     见 browser_panel_windows.go；旧「无头 Edge + MJPEG 截图流镜像」方案
//     已整体移除——图片镜像的交互体验与原生控件不可弥合）。
// 两形态可并存、各自独立进程与配置目录；browserTarget 操作面（browser_ops.go）
// 由本宿主与原生面板共同实现，工具层（tool_browser.go）形态无关。
// 生命周期：实例死亡（用户关窗/进程退出把 ctx 打成取消）必须被识别——
// startIfNeeded 发现僵尸态先彻底清场再重开；do/activeBrowser 对死实例统一报
// 「已被用户关闭」而非裸 context canceled。2026-10-13 QQ邮箱会话事故：旧版
// 全会话共用一个有头实例，窗口被用户手关后死 ctx 仍被当作「在运行」，
// 之后三次 browser_open 全秒报 context canceled，重启引擎才恢复。
// 安全：服务仅本机监听（既定边界）；AI 侧工具照旧经 capBrowser 能力门控；
// 页面登录态落在 data/browser-profile*（有头按会话隔离在
// data/browser-profiles-headed/<会话>/，网页形态无头与桌面右栏面板同样按
// 会话隔离——右栏面板见 data/webview-browsers/<会话>/），与用户日常浏览器
// 隔离；助手进程退出由顶层 kill-on-close Job Object 清场（processguard_windows.go）。

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

const (
	browserTimeout = 60 * time.Second // 单次 CDP 操作上限（与原 tool_browser 一致）
	browserDefW    = 1280
	browserDefH    = 900
	browserMinSide = 320 // 视口边长下限（对齐 ZCode 的 320–3840 × 320–2160）
	browserMaxW    = 3840
	browserMaxH    = 2160
)

// errBrowserClosed 实例已死（用户关闭窗口或进程退出）时的统一报错：把死因
// 说成人话，别让模型对着裸的 context canceled 猜谜。
var errBrowserClosed = errors.New("浏览器已被关闭（用户关闭了窗口或浏览器进程退出）：请调用 browser_open 重新打开")

// BrowserHost 是一个 chromedp 浏览器宿主实例，两种形态（可并存、各自独立进程与配置目录）：
//   * headless=true  后台无头：网页形态的内嵌目标，AI 工具专用（无画面镜像）；
//   * headless=false 有头独立窗口：弹一个用户可见的 Edge 窗口（旧版行为），
//     人直接在真实窗口里操作，AI 经同一批工具操作。
// 生命周期：browser_open / open-view 触发启动；browser_close 与 Close() 显式关闭。
type BrowserHost struct {
	app      *App
	headless bool
	profile  string // 显式配置目录（空=按形态取默认；有头按会话指定，见 App.headedBrowser）

	mu          sync.Mutex // 串行化生命周期与元数据读写
	allocCancel context.CancelFunc
	tabCtx      context.Context
	tabCancel   context.CancelFunc

	vw, vh   int    // 当前生效视口（CSS px）
	url      string // 最近一次导航落定的地址/标题（state 端点与工具回显用）
	title    string
	reopened bool // 最近一次 Navigate 是否检测到实例已死并自动重开（ReopenNote 回显用）

	lastX, lastY float64 // 最近鼠标位置（滚轮等不带坐标的输入挂在这里）
}

func newBrowserHost(app *App, headless bool, profile string) *BrowserHost {
	return &BrowserHost{app: app, headless: headless, profile: profile}
}

// findBrowserExe 优先找系统自带的 Edge（Win10/11 必装，符合自包含
// 原则——不依赖用户另装浏览器）；Edge 异常缺失时兜底找 Chrome；
// 都找不到交给 chromedp 自行探测。
func findBrowserExe() string {
	candidates := []string{
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
		os.Getenv("LOCALAPPDATA") + `\Microsoft\Edge\Application\msedge.exe`,
		os.Getenv("LOCALAPPDATA") + `\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
	}
	for _, p := range candidates {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// Browser 返回（惰性创建）该会话的无头宿主：网页形态的内嵌目标（桌面原生
// 壳的会话面板见 panelFor；Electron 壳见 nativePanel）。每会话一个实例、
// 独立 profile（data/browser-profiles/<会话>/）——A 会话 AI 开的页面不会被
// B 会话的导航顶掉，登录态互不串台；无会话上下文（空键）退回共享目录
// browser-profile（旧行为）。
func (a *App) embeddedBrowser(sid string) *BrowserHost {
	key := browserProfileKey(sid)
	a.browserMu.Lock()
	defer a.browserMu.Unlock()
	if a.browsersEmbedded == nil {
		a.browsersEmbedded = map[string]*BrowserHost{}
	}
	if h, ok := a.browsersEmbedded[key]; ok {
		return h
	}
	profile := filepath.Join(a.Cfg.DataDir, "browser-profile") // 共享（旧版目录）
	if key != "" {
		profile = filepath.Join(a.Cfg.DataDir, "browser-profiles", key)
	}
	h := newBrowserHost(a, true, profile)
	a.browsersEmbedded[key] = h
	return h
}

// browserProfileKey 会话 id → 有头实例/active 记账键（兼配置目录名）：仅放行
// 字母数字与 _-（会话 id 本是十六进制），防路径拼接被注入；异常输入归并到
// 空串=共享实例。返回值直接作 map 键与子目录名。
func browserProfileKey(sid string) string {
	sid = strings.TrimSpace(sid)
	if sid == "" || len(sid) > 64 {
		return ""
	}
	for _, r := range sid {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return ""
		}
	}
	return sid
}

// headedBrowser 返回（惰性创建）该会话的有头独立窗口宿主：每会话一个实例、
// 独立进程与独立配置目录（data/browser-profiles-headed/<会话>/）——登录态
// 互不串台，也天然避开 Chromium「同一 profile 单例锁」的互斥；无会话上下文
// 的调用（空键）退回旧版共享目录 browser-profile-headed。旧版全会话共用一个
// 实例：A 会话开着的窗口被用户一关，B 会话接着 open 就地复用已死 ctx 秒报
// context canceled 且永不自愈（2026-10-13 QQ邮箱会话事故），按会话隔离后各
// 会话的窗口生死互不拖累。
func (a *App) headedBrowser(sid string) *BrowserHost {
	key := browserProfileKey(sid)
	a.browserMu.Lock()
	defer a.browserMu.Unlock()
	if a.browsersHeaded == nil {
		a.browsersHeaded = map[string]*BrowserHost{}
	}
	if h, ok := a.browsersHeaded[key]; ok {
		return h
	}
	profile := filepath.Join(a.Cfg.DataDir, "browser-profile-headed") // 共享（旧版目录）
	if key != "" {
		profile = filepath.Join(a.Cfg.DataDir, "browser-profiles-headed", key)
	}
	h := newBrowserHost(a, false, profile)
	a.browsersHeaded[key] = h
	return h
}

// embeddedTarget 内嵌形态目标：Electron 壳=右栏 <webview> 单例面板；原生
// 桌面壳=该会话的右栏 WebView2 面板（panelFor）；网页形态=该会话的无头
// chromedp 实例。sid 为会话 id（空=无会话上下文的共享实例）。
func (a *App) embeddedTarget(sid string) browserTarget {
	if a.nativePanel != nil {
		return a.nativePanel
	}
	a.panelMu.Lock()
	dispatch := a.panelDispatch
	a.panelMu.Unlock()
	if dispatch != nil {
		return a.panelFor(sid)
	}
	return a.embeddedBrowser(sid)
}

// ---------- 原生桌面壳的会话面板集合 ----------

// panelFor 返回（惰性创建壳，WebView2 控件仍由首个矩形/导航触发）该会话的
// 右栏面板。每会话独立实例与 profile（data/webview-browsers/<会话>/，空键
// 沿用旧共享目录 data/webview-browser）：页面与登录态互不串台，切会话只是
// 换显示的面板，实例保留。仅原生桌面壳（panelDispatch 已就位）可达。
func (a *App) panelFor(sid string) browserTarget {
	key := browserProfileKey(sid)
	a.panelMu.Lock()
	defer a.panelMu.Unlock()
	if a.panels == nil {
		a.panels = map[string]browserTarget{}
	}
	if p, ok := a.panels[key]; ok {
		return p
	}
	p := newBrowserPanel(a, a.panelDispatch, a.panelMainWnd, sid)
	a.panels[key] = p
	return p
}

// panelEach 遍历当前面板集合（快照后锁外回调，避免回调里的 COM 投递与锁交叉）。
func (a *App) panelEach(fn func(p browserTarget)) {
	a.panelMu.Lock()
	list := make([]browserTarget, 0, len(a.panels))
	for _, p := range a.panels {
		list = append(list, p)
	}
	a.panelMu.Unlock()
	for _, p := range list {
		fn(p)
	}
}

// panelSetVisible 记录当前显示中的面板键（互斥显示的记账；隐藏上报据此路由）。
func (a *App) panelSetVisible(key string) {
	a.panelMu.Lock()
	a.panelVisible = key
	a.panelMu.Unlock()
}

// panelVisibleKey 读当前显示中的面板键。
func (a *App) panelVisibleKey() string {
	a.panelMu.Lock()
	defer a.panelMu.Unlock()
	return a.panelVisible
}

// panelHideCurrent 把当前显示中的面板收屏外（隐藏上报的路由目标）。返回是否动过。
func (a *App) panelHideCurrent() bool {
	key := a.panelVisibleKey()
	if key == "" {
		return false
	}
	a.panelMu.Lock()
	p := a.panels[key]
	a.panelVisible = ""
	a.panelMu.Unlock()
	if bp, ok := p.(interface {
		SetRect(x, y, w, h int, dpr float64, visible bool) error
	}); ok && bp != nil {
		_ = bp.SetRect(0, 0, 0, 0, 1, false)
		return true
	}
	return false
}

// panelHideOthers 除 key 外的面板全部收屏外（换显示面时的互斥收场：它们是
// 同一主窗口的兄弟子窗口，不收会叠在当前面板上）。只动已创建的控件。
func (a *App) panelHideOthers(key string) {
	a.panelMu.Lock()
	var list []browserTarget
	for k, p := range a.panels {
		if k == key {
			continue
		}
		list = append(list, p)
	}
	a.panelMu.Unlock()
	for _, p := range list {
		if bp, ok := p.(interface {
			SetRect(x, y, w, h int, dpr float64, visible bool) error
		}); ok && bp != nil {
			_ = bp.SetRect(0, 0, 0, 0, 1, false)
		}
	}
}

// panelFrontendReset 每轮桌面前端（热重启/重建）换新时重置面板宿主：旧窗口
// 的面板全部兜底释放（消息泵已停，Dispatch 投递不再到达），新面板用新窗口
// 的 UI 线程与 HWND 重建。仅 UI 线程调用（desktop_windows.go 的前端循环）。
func (a *App) panelFrontendReset(dispatch func(f func()), mainWnd uintptr) {
	a.panelMu.Lock()
	old := a.panels
	a.panels = nil
	a.panelDispatch = dispatch
	a.panelMainWnd = mainWnd
	a.panelVisible = ""
	a.panelMu.Unlock()
	// 浮层让位标记随前端复位：新页面 modal 全关，旧前端若在让位态中死亡，
	// 不清零会让新前端的浏览器面板出生即被压底（永远白屏类事故）。
	atomic.StoreInt32(&a.panelOverlayYield, 0)
	for _, p := range old {
		if lp, ok := p.(interface{ releaseAfterMainLoopGone() }); ok {
			lp.releaseAfterMainLoopGone()
		}
	}
}

// ---------- 面板关闭的阶段二：等进程真退出再拆窗口 ----------
//
// 用户对「关闭浏览器」的硬要求：必须真关（进程真退出、窗口真拆掉）；不许
// 「几分钟后后台回收」这类延迟机制（出了 bug 感知不到，莫名挂十几分钟最难
// 排查），也不许「软关闭」（清页面收屏外、留实例）当最终方案。阶段一的
// 「发退出指令」与阶段三的「拆宿主子窗口」在 browser_panel_windows.go 的
// closePanel 里（那两段是 UI 线程上的 COM/Win32 动作）；中间这段「等那个
// 浏览器进程真的退出」放在这里——它是纯 Go 的进程信号逻辑，注入平台动作就能
// 单测（browser_close_test.go），不必真起 WebView2。

// panelRecover 面板侧所有落在 WebView2 原生回调栈上、以及关闭流程后台
// goroutine 里的入口的统一兜底：这些帧里的 panic 冒出去就是整进程无声死亡
// （无 panic 日志、无 Windows 崩溃事件——windowsgui 子系统下回调栈上的崩溃
// 无现场可查），只能就地吞掉落日志。覆盖面刻意包含 go-webview2 库自己调进来
// 的帧（NavigationCompleted 等）：库不给护栏，护栏就放在我们自己这一侧（取证
// 建议「把护栏/recover 覆盖到库级回调帧」，见
// .jxai/tmp/取证报告-20261022-1645服务重启归因.md）。
func panelRecover(where string) {
	if r := recover(); r != nil {
		log.Printf("[browser-panel] %s panic（已兜住）: %v\n%s", where, r, debug.Stack())
	}
}

// panelExiter 对一个「已定位的浏览器进程」的等退出/强杀操作。真实实现
// （browser_panel_windows.go 的 panelProcess）在发退出指令之前就打开进程
// 句柄：关后 PID 可能被系统复用，按句柄等/杀才安全；单测用假实现。
//
// 为什么不用 ICoreWebView2Environment5::BrowserProcessExited 当退出信号：绑定库
// （jchv/go-webview2）没导出该事件，取它要自己对环境对象 QueryInterface 并手写
// 一个未经本机验证的 vtbl 镜像——镜像错一格就是整进程静默崩溃（正是本路径要
// 避免的事故类型）。而 WaitForSingleObject 等的是同一个内核退出事件，确定性
// 一样，还没有这层自研风险；PID 取不到时按「已退出」处理，照常拆窗口。
type panelExiter interface {
	PID() uint32
	// Wait 等进程退出，等到返回 true。真实实现是 WaitForSingleObject 等内核
	// 的退出事件（确定的退出信号），不是轮询、也不是拍脑袋的固定延时。
	Wait(max time.Duration) bool
	// Kill 强制终止这个进程（我们自己的浏览器子进程）。
	Kill() error
	// Release 释放句柄。
	Release()
}

// panelCloseTiming 关闭等待的两段上限：先等自然退出 exit，超时就强杀，强杀后
// 最多再等 kill。自然退出通常远快于此（控制器一关，浏览器进程就开始退场）。
type panelCloseTiming struct{ exit, kill time.Duration }

var panelCloseWait = panelCloseTiming{exit: 10 * time.Second, kill: 5 * time.Second}

// finishPanelClose 关闭的阶段二：等浏览器进程退出 →（超时）强杀并继续等到
// 确实退出 → 投递拆宿主子窗口（UI 线程，只投一次）。返回是否动用了强杀。
//
// 纪律：超时也必须真关——没有「等不到就放弃拆、留个空壳」的分支（空壳在后台
// 驻留，出 bug 也感知不到）。每一步都落 [browser-panel] 日志：排查「莫名挂住
// 的后台问题」全靠这几行。
func finishPanelClose(ex panelExiter, post func(f func()), sid, what string, destroy func(), tm panelCloseTiming) (killed bool) {
	defer panelRecover("面板关闭 阶段二")
	start := time.Now()
	if ex != nil {
		defer ex.Release()
		pid := ex.PID()
		log.Printf("[browser-panel] 关闭: 等浏览器进程退出中（sid=%q %s pid=%d）", sid, what, pid)
		if ex.Wait(tm.exit) {
			log.Printf("[browser-panel] 关闭: 收到退出信号（sid=%q pid=%d 等待 %s）", sid, pid, elapsedStr(start))
		} else {
			killed = true
			log.Printf("[browser-panel] 关闭: 等退出超时（sid=%q pid=%d %s 未退），强制终止这个浏览器子进程",
				sid, pid, tm.exit)
			if err := ex.Kill(); err != nil {
				log.Printf("[browser-panel] 关闭: 强杀失败（sid=%q pid=%d）: %v；继续等它退出", sid, pid, err)
			} else {
				log.Printf("[browser-panel] 关闭: 已强制终止（sid=%q pid=%d），继续等到确实退出", sid, pid)
			}
			if ex.Wait(tm.kill) {
				log.Printf("[browser-panel] 关闭: 强杀后已确认退出（sid=%q pid=%d 等待 %s）", sid, pid, elapsedStr(start))
			} else {
				log.Printf("[browser-panel] 关闭: 强杀后 %s 仍未确认退出（sid=%q pid=%d）——该进程需人工核查；"+
					"窗口照常拆掉（不留空壳）", tm.kill, sid, pid)
			}
		}
	} else {
		log.Printf("[browser-panel] 关闭: 没有可等的浏览器进程句柄（sid=%q），跳过等退出", sid)
	}
	if destroy == nil || post == nil {
		log.Printf("[browser-panel] 关闭: 拆窗口跳过（sid=%q %s 拆窗口动作=%v UI投递=%v）", sid, what, destroy != nil, post != nil)
		return killed
	}
	done := make(chan struct{})
	post(func() { // 拆窗口只在 UI 线程做：进程已确认退出，此前已反注册自有事件、状态已清空
		defer panelRecover("面板关闭 阶段三")
		destroy()
		close(done)
	})
	select {
	case <-done:
		log.Printf("[browser-panel] 关闭: 已拆宿主子窗口（sid=%q %s）", sid, what)
	case <-time.After(2 * time.Second):
		log.Printf("[browser-panel] 关闭: 拆窗口投递 2 秒未回（sid=%q %s，消息泵可能已停）", sid, what)
	}
	log.Printf("[browser-panel] 关闭: 完成（sid=%q %s 总耗时 %s 强杀=%v）", sid, what, elapsedStr(start), killed)
	return killed
}

// elapsedStr 把耗时打成日志里好读的形式（毫秒）。
func elapsedStr(t time.Time) string {
	return time.Since(t).Round(time.Millisecond).String()
}

// browserFor 按 browser_open 的 mode 取目标（headless 缺省=内嵌形态），并记为
// 该会话工具作用的当前实例（active）——其余工具（click/type/…）都作用于
// 本会话的 active，会话之间互不串目标。sid 为会话 id（空=非会话入口共用）。
func (a *App) browserFor(sid, mode string) browserTarget {
	var t browserTarget
	if mode == "headed" {
		t = a.headedBrowser(sid)
	} else {
		t = a.embeddedTarget(sid)
	}
	key := browserProfileKey(sid)
	a.setActiveBrowser(key, t)
	a.browserMu.Lock()
	if a.browserUserClosedAt == nil {
		a.browserUserClosedAt = map[string]time.Time{}
	}
	delete(a.browserUserClosedAt, key) // 重新打开：用户关闭标记作废（此后的「未在运行」另有原因）
	a.browserMu.Unlock()
	return t
}

// setActiveBrowser 记「本会话工具作用的实例」（懒建记账表）。必须走它而不是直
// 接写 map：回退分支可能落在从未走过 browser_open 的会话上（面板是被右栏矩形
// 上报直接拉起来的——用户只是打开右栏浏览器视图、没导航过），那时这张表还是
// nil，直接赋值即 panic：关闭浏览器这条路上曾经因此埋着一个空指针隐患。
func (a *App) setActiveBrowser(key string, t browserTarget) {
	a.browserMu.Lock()
	if a.browserActive == nil {
		a.browserActive = map[string]browserTarget{}
	}
	a.browserActive[key] = t
	a.browserMu.Unlock()
}

// activeBrowser 工具作用目标：本会话最近一次 browser_open 选定的实例；没有
// active 时兜底到仍在运行的那个；都没运行返回 nil（调用方报「先 browser_open」）。
// active「不在运行」分两种：显式 browser_close 的回退另一形态存活实例（旧行为）；
// 意外死亡（用户关掉窗口/进程退出）则不静默回退——回退到面板会把别的页面当
// 当前页读、误导模型，这里清掉死记账并返回 nil，让调用方明确报「已被用户关闭」。
func (a *App) activeBrowser(sid string) browserTarget {
	key := browserProfileKey(sid)
	a.browserMu.Lock()
	t := a.browserActive[key]
	a.browserMu.Unlock()
	if t != nil && t.Started() {
		return t
	}
	if bh, ok := t.(*BrowserHost); ok && bh.dead() {
		a.browserMu.Lock()
		delete(a.browserActive, key)
		a.browserMu.Unlock()
		return nil
	}
	// active 已关（显式 Close）：回退到仍在运行的实例，并更新 active
	for _, cand := range []browserTarget{a.embeddedTarget(sid), a.headedBrowser(sid)} {
		if cand != nil && cand.Started() {
			a.setActiveBrowser(key, cand)
			return cand
		}
	}
	return nil
}

// ---------- 生命周期 ----------

func (h *BrowserHost) Start() error {
	_, err := h.startIfNeeded()
	return err
}

// Started 实例是否在运行：ctx 存在且未被取消。浏览器进程一死（用户关掉
// 窗口/进程退出）chromedp 就会取消 ctx——只看 tabCtx!=nil 的旧判法会把
// 死实例当「在运行」（僵尸态），后续操作全报 context canceled。
func (h *BrowserHost) Started() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tabCtx != nil && h.tabCtx.Err() == nil
}

// dead 实例是否「带病存活」：已死但字段还挂着（区别于显式 Close 后的 nil）。
func (h *BrowserHost) dead() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tabCtx != nil && h.tabCtx.Err() != nil
}

// profileDir 实例的配置目录（profile 为空=按形态取默认；绝对化）。预授权
// 注入与启动共用，保证两处算的是同一个目录。
func (h *BrowserHost) profileDir() string {
	profile := h.profile
	if profile == "" {
		profile = filepath.Join(h.app.Cfg.DataDir, "browser-profile")
		if !h.headless {
			profile = filepath.Join(h.app.Cfg.DataDir, "browser-profile-headed")
		}
	}
	if abs, err := filepath.Abs(profile); err == nil {
		return abs
	}
	return profile
}

// writeProfileGrants 往 Chromium profile 的 Preferences 注入内容设置预授权：
// grants 每项「权限名@origin」（如 automatic_downloads@https://mail.qq.com——
// 权限名是 Chromium content settings 的类型名，对应浏览器设置页里的一项，
// 网上可查），落为 profile.content_settings.exceptions.<权限名> 下的
// "<origin>,*": {"setting":1}（1=允许）。只合并指定键，其余设置不动。
// 必须在该 profile 的浏览器未运行时调用——运行中的实例退出时会整体覆写
// Preferences，注入会被冲掉（ApplyGrants 负责先停实例）。
func writeProfileGrants(profileDir string, grants []string) error {
	if len(grants) == 0 {
		return nil
	}
	prefsPath := filepath.Join(profileDir, "Default", "Preferences")
	if err := os.MkdirAll(filepath.Dir(prefsPath), 0o755); err != nil {
		return err
	}
	raw := map[string]any{}
	if b, err := os.ReadFile(prefsPath); err == nil {
		_ = json.Unmarshal(b, &raw) // 解析失败从空重建：浏览器启动时自会补全结构
	}
	prof, _ := raw["profile"].(map[string]any)
	if prof == nil {
		prof = map[string]any{}
		raw["profile"] = prof
	}
	cs, _ := prof["content_settings"].(map[string]any)
	if cs == nil {
		cs = map[string]any{}
		prof["content_settings"] = cs
	}
	exc, _ := cs["exceptions"].(map[string]any)
	if exc == nil {
		exc = map[string]any{}
		cs["exceptions"] = exc
	}
	for _, g := range grants {
		name, origin, ok := strings.Cut(strings.TrimSpace(g), "@")
		if !ok || name == "" || origin == "" {
			return fmt.Errorf("预授权格式应为 权限名@origin（如 automatic_downloads@https://mail.qq.com），收到: %s", g)
		}
		list, _ := exc[name].(map[string]any)
		if list == nil {
			list = map[string]any{}
			exc[name] = list
		}
		list[origin+",*"] = map[string]any{"setting": 1}
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	return writeFileAtomic(prefsPath, out)
}

// waitProfileQuiet 等浏览器进程把退出时的 Preferences 覆写做完（修改时间
// 连续几拍不变视为写完），上限 max。文件不存在（全新 profile）立即返回。
func waitProfileQuiet(profileDir string, max time.Duration) {
	p := filepath.Join(profileDir, "Default", "Preferences")
	deadline := time.Now().Add(max)
	var prev time.Time
	stable := 0
	for time.Now().Before(deadline) {
		fi, err := os.Stat(p)
		switch {
		case err != nil:
			return // 还没有 Preferences：没有可被覆写的东西
		case fi.ModTime() == prev:
			stable++
			if stable >= 3 {
				return // ~450ms 无变化：退场覆写已结束
			}
		default:
			prev, stable = fi.ModTime(), 0
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// ApplyGrants 给本实例预授权内容设置（browser_open 的 grants 参数入口）：
// Preferences 只在浏览器启动时读取，所以实例在跑必须先停再注入，之后由
// 调用方的 Navigate 触发重开（登录态在 profile 里，无损）。右栏面板同款
// 方法见 browserPanel.ApplyGrants。
func (h *BrowserHost) ApplyGrants(grants []string) error {
	if len(grants) == 0 {
		return nil
	}
	if h.Started() {
		h.Close()
		waitProfileQuiet(h.profileDir(), 5*time.Second)
	}
	return writeProfileGrants(h.profileDir(), grants)
}

// startIfNeeded 拉起浏览器，返回是否「检测到上一实例已死并重开」。发现
// 僵尸态先彻底清场再重启：用户手动关掉窗口后，下一次 browser_open 应当
// 重开一个新窗口，而不是抱着死 ctx 报错（调用方需持有 h.mu）。
// 按形态取启动参数：内嵌无头走镜像帧流那套；有头弹独立可见窗口（不静音、
// 不设窗口尺寸）。profile 必须转绝对路径：Chromium 对相对 user-data-dir 会
// 静默秒退（stderr 无输出），chromedp 只能报出空的 "chrome failed to start"
// ——实测踩坑。
func (h *BrowserHost) startIfNeeded() (bool, error) {
	reopened := false
	if h.tabCtx != nil {
		if h.tabCtx.Err() == nil {
			return false, nil
		}
		h.closeLocked() // 上一实例已死：彻底清场后重开
		reopened = true
	}
	profile := h.profileDir()
	if err := os.MkdirAll(profile, 0o755); err != nil {
		return reopened, fmt.Errorf("创建浏览器配置目录失败: %w", err)
	}
	var opts []chromedp.ExecAllocatorOption
	if h.headless {
		opts = append(chromedp.DefaultExecAllocatorOptions[:], // 默认即 headless + disable-gpu + no-first-run
			chromedp.Flag("user-data-dir", profile),
			chromedp.Flag("window-size", fmt.Sprintf("%d,%d", browserDefW, browserDefH)),
			chromedp.Flag("mute-audio", true),
			// 无头页面无人观察时会被挂起（定时器停摆、SPA 不刷新）：AI 可能在
			// 面板未打开时操作页面，必须保持常活（自动化栈的标准三旗）
			chromedp.Flag("disable-background-timer-throttling", true),
			chromedp.Flag("disable-backgrounding-occluded-windows", true),
			chromedp.Flag("disable-renderer-backgrounding", true),
		)
	} else {
		// 有头独立窗口（旧版行为）：可见、有声、真窗口；用户直接操作，AI 共用
		opts = append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.Flag("headless", false), // 有头模式：用户可见，操作可信
			chromedp.Flag("disable-gpu", false),
			chromedp.Flag("user-data-dir", profile),
		)
	}
	if exe := findBrowserExe(); exe != "" {
		opts = append(opts, chromedp.ExecPath(exe))
	}
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cancel := chromedp.NewContext(allocCtx)
	// 预热：真正拉起浏览器进程，把启动失败暴露在第一次调用上。必须带超时——
	// 启动卡住（如残留实例占用 profile）时没有超时会无限占住 h.mu，之后所有
	// 浏览器端点（state/输入/导航）全部被堵死；快速失败让前端能走降级路径。
	// 注意不能拿 WithTimeout 派生 ctx 去 Run：会话会绑定到该 ctx，成功后取消
	// 派生 ctx 等于掐断会话（实测后续 navigate 全部 context canceled）。本机
	// 还实测过服务进程内启动的 Edge 会「进程起来、ws 已连、首个标签页事件
	// 永不到达」式挂起（同参数探针进程正常），浏览器宿主方案待更换，此处只
	// 保证不拖死进程：看门狗 20 秒未完成即拆掉这次尝试。
	runDone := make(chan error, 1)
	go func() { runDone <- chromedp.Run(ctx) }()
	select {
	case err := <-runDone:
		if err != nil {
			cancel()
			allocCancel()
			return reopened, fmt.Errorf("启动浏览器失败（需要系统自带的 Edge，或已安装的 Chrome）: %w", err)
		}
	case <-time.After(20 * time.Second):
		cancel()
		allocCancel()
		return reopened, fmt.Errorf("启动浏览器失败（需要系统自带的 Edge，或已安装的 Chrome）: 20 秒内未完成启动（可能被残留浏览器实例占用配置目录）")
	}
	h.allocCancel, h.tabCancel = allocCancel, cancel
	h.tabCtx = ctx
	h.vw, h.vh = browserDefW, browserDefH
	return reopened, nil
}

func (h *BrowserHost) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closeLocked()
}

// closeLocked 彻底清场（调用方需持有 h.mu）：取消并清空 ctx/句柄与元数据。
// 显式关闭与「发现实例已死后重开前的清场」共用，保证字段回到干净初始态。
func (h *BrowserHost) closeLocked() {
	if h.tabCancel != nil {
		h.tabCancel()
	}
	if h.allocCancel != nil {
		h.allocCancel()
	}
	h.tabCtx, h.allocCancel, h.tabCancel = nil, nil, nil
	h.url, h.title, h.reopened = "", "", false
}

// ---------- 导航 ----------

// Navigate 打开/跳转地址，返回落定后的地址与标题，并广播 browser_nav。
// 不做 scheme 白名单——浏览器能开什么由浏览器/CDP 自己决定：edge://、
// about:、view-source: 等一律放行，WebView2/chromedp 打不开会自然报错
// （比如 WebView2 面板开不了 edge:// 内部页，是微软对控件的限制）。
func (h *BrowserHost) Navigate(raw string) (string, string, error) {
	return h.navigate(raw, true)
}

func (h *BrowserHost) navigate(raw string, allowFile bool) (string, string, error) {
	u := strings.TrimSpace(raw)
	if u == "" {
		return "", "", fmt.Errorf("地址为空")
	}
	_ = allowFile // 历史参数：白名单已拆除，保留签名免动调用点
	reopened, err := h.startIfNeeded()
	if err != nil {
		return "", "", err
	}
	h.mu.Lock()
	h.reopened = reopened
	h.mu.Unlock()
	var url, title string
	if err := h.do(func(ctx context.Context) error {
		if err := chromedp.Run(ctx, chromedp.Navigate(u)); err != nil {
			return err
		}
		if err := chromedp.Run(ctx, chromedp.WaitReady("body", chromedp.ByQuery)); err != nil {
			return err
		}
		return chromedp.Run(ctx, chromedp.Location(&url), chromedp.Title(&title))
	}); err != nil {
		return "", "", err
	}
	h.setNav(url, title)
	return url, title, nil
}

// ReopenNote 最近一次 Navigate 若检测到上一实例已死并自动重开，返回给工具
// 层的回显说明（老窗口多半是用户手关的，得让模型知道发生了什么）。
func (h *BrowserHost) ReopenNote() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.reopened {
		return "检测到上一实例已被关闭，已自动重新打开"
	}
	return ""
}

// NavAction 后退/前进/刷新（面板与工具共用）。
func (h *BrowserHost) NavAction(action string) error {
	if err := h.Start(); err != nil {
		return err
	}
	var act chromedp.Action
	switch action {
	case "back":
		act = chromedp.NavigateBack()
	case "forward":
		act = chromedp.NavigateForward()
	case "reload":
		act = chromedp.Reload()
	default:
		return fmt.Errorf("未知导航动作: %s（支持 back/forward/reload）", action)
	}
	if err := h.do(func(ctx context.Context) error {
		return chromedp.Run(ctx, act)
	}); err != nil {
		return err
	}
	h.readNav()
	return nil
}

func (h *BrowserHost) setNav(url, title string) {
	h.mu.Lock()
	h.url, h.title = url, title
	h.mu.Unlock()
}

func (h *BrowserHost) readNav() {
	var url, title string
	if err := h.do(func(ctx context.Context) error {
		return chromedp.Run(ctx, chromedp.Location(&url), chromedp.Title(&title))
	}); err != nil {
		return
	}
	h.setNav(url, title)
}

// ---------- 视口 ----------

// SetViewport 设定 CSS 视口（Emulation.SetDeviceMetricsOverride）：网页形态下
// 供 AI 做响应式测试（无面板跟随，画面不展示）。桌面形态的内嵌目标是原生
// 面板（browser_panel_windows.go），视口=面板实际尺寸，不走本方法。
func (h *BrowserHost) SetViewport(w, hi int) error {
	if w == 0 || hi == 0 {
		w, hi = browserDefW, browserDefH
	}
	w = clampInt(w, browserMinSide, browserMaxW)
	hi = clampInt(hi, browserMinSide, browserMaxH)
	if err := h.Start(); err != nil {
		return err
	}
	if err := h.do(func(ctx context.Context) error {
		return chromedp.Run(ctx, emulation.SetDeviceMetricsOverride(int64(w), int64(hi), 1.0, false))
	}); err != nil {
		return err
	}
	h.mu.Lock()
	h.vw, h.vh = w, hi
	h.mu.Unlock()
	return nil
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ---------- 输入（人机共操通道） ----------

// BrowserInputMsg 面板转发的人的输入事件；AI 工具的点击/输入最终也汇入同一批 CDP 调用。
type BrowserInputMsg struct {
	Type   string  `json:"type"` // down|up|move|scroll|key|text
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	DX     float64 `json:"dx"`
	DY     float64 `json:"dy"`
	Button string  `json:"button"` // left|middle|right（缺省 left）
	Count  int     `json:"count"`  // down/up 的连击数（1=单击 2=双击）
	Key    string  `json:"key"`    // key：键名（Enter/Backspace/ArrowDown/…）
	Text   string  `json:"text"`   // text：整段文本（InsertText，中文正确）
	Ctrl   bool    `json:"ctrl"`
	Alt    bool    `json:"alt"`
	Shift  bool    `json:"shift"`
	Meta   bool    `json:"meta"`
}

var browserKeyMap = map[string]struct {
	code    int64
	keyName string
	codeStr string
}{
	"enter":      {13, "Enter", "Enter"},
	"backspace":  {8, "Backspace", "Backspace"},
	"tab":        {9, "Tab", "Tab"},
	"escape":     {27, "Escape", "Escape"},
	"delete":     {46, "Delete", "Delete"},
	"arrowleft":  {37, "ArrowLeft", "ArrowLeft"},
	"arrowup":    {38, "ArrowUp", "ArrowUp"},
	"arrowright": {39, "ArrowRight", "ArrowRight"},
	"arrowdown":  {40, "ArrowDown", "ArrowDown"},
	"home":       {36, "Home", "Home"},
	"end":        {35, "End", "End"},
	"pageup":     {33, "PageUp", "PageUp"},
	"pagedown":   {34, "PageDown", "PageDown"},
}

// Input 把面板输入翻译为 CDP 输入事件。坐标一律是视口 CSS px
// （前端按 显示尺寸÷视口尺寸 换算好再发）。人的明确操作允许直接拉起宿主。
func (h *BrowserHost) Input(m BrowserInputMsg) error {
	if err := h.Start(); err != nil {
		return err
	}
	if m.Type == "move" || m.Type == "down" || m.Type == "up" || m.Type == "scroll" {
		if m.X != 0 || m.Y != 0 {
			h.mu.Lock()
			h.lastX, h.lastY = m.X, m.Y
			h.mu.Unlock()
		}
	}
	return h.do(func(ctx context.Context) error {
		switch m.Type {
		case "down", "up":
			t := input.MousePressed
			if m.Type == "up" {
				t = input.MouseReleased
			}
			count := int64(m.Count)
			if count < 1 {
				count = 1
			}
			return chromedp.Run(ctx, input.DispatchMouseEvent(t, m.X, m.Y).
				WithButton(mouseButtonOf(m.Button)).WithClickCount(count))
		case "move":
			return chromedp.Run(ctx, input.DispatchMouseEvent(input.MouseMoved, m.X, m.Y).
				WithButton(input.None))
		case "scroll":
			x, y := m.X, m.Y
			if x == 0 && y == 0 {
				h.mu.Lock()
				x, y = h.lastX, h.lastY
				h.mu.Unlock()
			}
			return chromedp.Run(ctx, input.DispatchMouseEvent(input.MouseWheel, x, y).
				WithDeltaX(m.DX).WithDeltaY(m.DY))
		case "key":
			return dispatchKey(ctx, m)
		case "text":
			if m.Text == "" {
				return nil
			}
			return chromedp.Run(ctx, input.InsertText(m.Text))
		default:
			return fmt.Errorf("未知输入类型: %s", m.Type)
		}
	})
}

func mouseButtonOf(b string) input.MouseButton {
	switch b {
	case "middle":
		return input.Middle
	case "right":
		return input.Right
	default:
		return input.Left
	}
}

func dispatchKey(ctx context.Context, m BrowserInputMsg) error {
	name := strings.ToLower(strings.TrimSpace(m.Key))
	var mods input.Modifier
	if m.Alt {
		mods |= input.ModifierAlt
	}
	if m.Ctrl {
		mods |= input.ModifierCtrl
	}
	if m.Meta {
		mods |= input.ModifierMeta
	}
	if m.Shift {
		mods |= input.ModifierShift
	}
	if k, ok := browserKeyMap[name]; ok {
		p := input.DispatchKeyEvent(input.KeyRawDown).
			WithKey(k.keyName).WithCode(k.codeStr).
			WithWindowsVirtualKeyCode(k.code).WithModifiers(mods)
		if err := chromedp.Run(ctx, p); err != nil {
			return err
		}
		return chromedp.Run(ctx, input.DispatchKeyEvent(input.KeyUp).
			WithKey(k.keyName).WithCode(k.codeStr).
			WithWindowsVirtualKeyCode(k.code).WithModifiers(mods))
	}
	// 未收录的键名：单字符走 InsertText，其余拒绝（前端只送已知键名）
	if len([]rune(m.Key)) == 1 && mods == 0 {
		return chromedp.Run(ctx, input.InsertText(m.Key))
	}
	return fmt.Errorf("不支持的键: %s", m.Key)
}

// ---------- 截图 / 状态 ----------

func (h *BrowserHost) Screenshot() ([]byte, error) {
	var buf []byte
	if err := h.do(func(ctx context.Context) error {
		return chromedp.Run(ctx, chromedp.CaptureScreenshot(&buf))
	}); err != nil {
		return nil, err
	}
	return buf, nil
}

// CaptureFrame 无头宿主的远程帧镜像截取（WebP q60）：网页形态家端被远程接入
// 时，右栏浏览器画面同样可被镜像。
func (h *BrowserHost) CaptureFrame() ([]byte, error) {
	var buf []byte
	if err := h.do(func(ctx context.Context) error {
		b, err := page.CaptureScreenshot().WithFormat(page.CaptureScreenshotFormatWebp).WithQuality(60).Do(ctx)
		if err != nil {
			return err
		}
		buf = b
		return nil
	}); err != nil {
		return nil, err
	}
	return buf, nil
}

type BrowserState struct {
	Running bool   `json:"running"`
	URL     string `json:"url"`
	Title   string `json:"title"`
	VW      int    `json:"viewportW"`
	VH      int    `json:"viewportH"`
}

func (h *BrowserHost) State() BrowserState {
	h.mu.Lock()
	running, vw, vh := h.tabCtx != nil, h.vw, h.vh
	url, title := h.url, h.title
	h.mu.Unlock()
	if !running {
		return BrowserState{Running: false, URL: url, Title: title, VW: vw, VH: vh}
	}
	// 运行中实时读（SPA 的 title 与内部路由变化不触发导航事件，缓存会过期）
	var u, t string
	if err := h.do(func(ctx context.Context) error {
		return chromedp.Run(ctx, chromedp.Location(&u), chromedp.Title(&t))
	}); err == nil && u != "" {
		h.setNav(u, t)
		url, title = u, t
	}
	return BrowserState{Running: true, URL: url, Title: title, VW: vw, VH: vh}
}

// SubscribeFrames 已随 MJPEG 镜像方案移除（桌面=原生面板，网页=无面板）。

// do 在浏览器上下文里执行一段操作（统一超时；不持锁执行，锁外并发安全由 CDP 串行化）。
func (h *BrowserHost) do(action func(ctx context.Context) error) error {
	h.mu.Lock()
	ctx := h.tabCtx
	h.mu.Unlock()
	if ctx == nil {
		return fmt.Errorf("浏览器尚未打开：先调用 browser_open")
	}
	runCtx, cancel := context.WithTimeout(ctx, browserTimeout)
	defer cancel()
	err := chromedp.Run(runCtx, chromedp.ActionFunc(action))
	if err != nil && h.dead() {
		// 操作中途实例死了（用户关掉窗口/进程退出）：报「已被关闭」而不是
		// 裸的 context canceled——模型看得懂，也知道下一步该重新 browser_open。
		return errBrowserClosed
	}
	return err
}

// BroadcastPointer 对 chromedp 宿主是空操作：AI 位置标记只对原生面板有意义
// （面板在目标页面内注入标记元素；有头窗口里用户直接看见真实点击）。
func (h *BrowserHost) BroadcastPointer(selector string) {}

// ---------- browserTarget 操作面适配（工具层形态无关，见 browser_ops.go） ----------

func (h *BrowserHost) Content() (string, string, string, error) {
	var url, title, body string
	if err := h.do(func(ctx context.Context) error {
		return chromedp.Run(ctx,
			chromedp.Location(&url),
			chromedp.Title(&title),
			chromedp.Evaluate(`document.body ? document.body.innerText : ""`, &body),
		)
	}); err != nil {
		return "", "", "", err
	}
	h.setNav(url, title)
	return url, title, body, nil
}

func (h *BrowserHost) Click(selector string) error {
	if err := h.do(func(ctx context.Context) error {
		return chromedp.Run(ctx,
			chromedp.WaitVisible(selector, chromedp.ByQueryAll),
			chromedp.Click(selector, chromedp.ByQueryAll),
			chromedp.Sleep(800*time.Millisecond),
		)
	}); err != nil {
		return fmt.Errorf("点击失败（元素不存在或不可见）: %w", err)
	}
	return nil
}

func (h *BrowserHost) TypeInto(selector, text string, clear bool) error {
	if err := h.do(func(ctx context.Context) error {
		return chromedp.Run(ctx, chromedp.WaitVisible(selector, chromedp.ByQueryAll))
	}); err != nil {
		return fmt.Errorf("输入失败（元素不存在或不可见）: %w", err)
	}
	if clear {
		// Clear 对少数控件（contenteditable 等）会失败，尽力而为
		_ = h.do(func(ctx context.Context) error {
			return chromedp.Run(ctx, chromedp.Clear(selector, chromedp.ByQueryAll))
		})
	}
	if err := h.do(func(ctx context.Context) error {
		return chromedp.Run(ctx, chromedp.SendKeys(selector, text, chromedp.ByQueryAll))
	}); err != nil {
		return fmt.Errorf("输入失败: %w", err)
	}
	return nil
}

func (h *BrowserHost) Scroll(direction string, amount int) error {
	if amount <= 0 {
		amount = 5
	}
	dy := float64(amount) * 100
	if strings.EqualFold(strings.TrimSpace(direction), "up") {
		dy = -dy
	}
	return h.Input(BrowserInputMsg{Type: "scroll", DY: dy})
}

func (h *BrowserHost) Evaluate(js string) (any, error) {
	var res any
	if err := h.do(func(ctx context.Context) error {
		return chromedp.Run(ctx, chromedp.Evaluate(js, &res))
	}); err != nil {
		return nil, err
	}
	return res, nil
}

func (h *BrowserHost) Where() string {
	if h.headless {
		return "内嵌浏览器（后台无头）"
	}
	return "独立浏览器窗口"
}

// ---------- HTTP handlers ----------

func (a *App) handleBrowserNavigate(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Session string `json:"session"` // 右栏所属会话：工具栏操作的是该会话的面板
		URL     string `json:"url"`
		Action  string `json:"action"` // back|forward|reload
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "参数解析失败: "+err.Error())
		return
	}
	h := a.browserFor(p.Session, "") // 面板地址栏也是用户发起：该会话的内嵌形态随之成为工具作用目标
	if p.Action != "" {
		if err := h.NavAction(p.Action); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		st := h.State()
		writeJSON(w, http.StatusOK, st)
		return
	}
	url, title, err := h.Navigate(p.URL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, BrowserState{Running: true, URL: url, Title: title})
}

func (a *App) handleBrowserViewport(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Session string `json:"session"`
		Width   int    `json:"width"`
		Height  int    `json:"height"`
		Reset   bool   `json:"reset"`
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "参数解析失败: "+err.Error())
		return
	}
	if p.Reset {
		p.Width, p.Height = 0, 0
	}
	h := a.embeddedTarget(p.Session)
	if err := h.SetViewport(p.Width, p.Height); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, h.State())
}

func (a *App) handleBrowserState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.embeddedTarget(r.URL.Query().Get("session")).State())
}

// handleBrowserPanelRect 桌面右栏原生面板的矩形同步端点：前端把网页占位区
// 的矩形上报，宿主移动 WebView2 子窗口。网页模式没有原生面板，忽略。
//
// 会话路由（右栏浏览器每会话一个独立面板）：上报带 session——可见上报
// 落位到该会话的面板（顺带把其余会话的面板收屏外，同一时刻只显示一个）；
// 隐藏上报作用于「当前显示中的面板」（前端只有桌面壳一个写者，隐藏语义
// 就是把显示着的那个收起来）。老前端不带 session：归并到共享面板（""键）。
//
// 写者纪律：面板可见性的状态机只允许桌面壳前端一个写者——历史上出过两类
// 事故：网页形态的页面（手机/浏览器里开的同一 UI）加载时发一次隐藏上报，
// 把桌面端好好显示的面板顶掉（desktop.log 2026-10-12 17:52:52：正确落位
// 一秒后被另一连接的全零上报隐藏）；以及非浏览器来源（Go http 客户端）的
// 全零上报。因此：带矩形却未标 desktop 的上报忽略；UA 不含 Mozilla 的
// （非浏览器页）上报忽略。前端侧配合（app-right.js bpwReport）：网页形态
// 一个字节都不发。
// handleBrowserFrame GET /api/browser/frame?session=<id>&v=<客户端已有帧版本>
//
// 远程接入时右栏浏览器的只读画面镜像：远程端（手机/外网电脑，远程形态前端）
// 以 ~2fps 轮询本端点，家端浏览器是唯一渲染权威，远程只是它的照片直播。
// 设计要点（人的意图）：
//   - 懒截图：没人轮询就不截图——收起右栏/切走视图时零开销；
//   - 内容指纹去重：页面静止时连续轮询只回 changed=false（几十字节），有
//     变化才带整帧 WebP base64；
//   - 最小间隔节流：frameEvery 内的重复请求共享缓存帧，多客户端或异常高频
//     请求都不会把 UI 线程的截图打满；
//   - ver 用毫秒时间戳（JSON 的 float64 往返在 2^53 内精确）：浏览器真关闭
//     重开后缓存重建，版本号不会回退到客户端已有的旧值而永远发不出新帧。
func (a *App) handleBrowserFrame(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("session")
	h := a.embeddedTarget(sid)
	st := h.State()
	if !st.Running {
		writeJSON(w, http.StatusOK, map[string]any{"running": false})
		return
	}
	cap, ok := h.(browserFrameCaptor)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "当前浏览器形态不支持远程画面镜像")
		return
	}
	key := browserProfileKey(sid)
	a.frameMu.Lock()
	if a.frameCache == nil {
		a.frameCache = map[string]*browserFrameEntry{}
	}
	e := a.frameCache[key]
	if e == nil {
		e = &browserFrameEntry{}
		a.frameCache[key] = e
	}
	a.frameMu.Unlock()

	clientVer, _ := strconv.ParseInt(r.URL.Query().Get("v"), 10, 64)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ver == 0 || time.Since(e.last) >= frameEvery {
		data, err := cap.CaptureFrame()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		sum := sha256.Sum256(data)
		if sum != e.sum {
			nv := time.Now().UnixMilli() // 毫秒时间戳：JSON 经 float64 往返（前端
			// JSON.parse）仍精确（2^53 内），纳秒会丢精度导致版本永远对不上；
			// 同毫秒内内容再变时单调 +1，避免版本碰撞把新帧吃掉
			if nv <= e.ver {
				nv = e.ver + 1
			}
			e.ver = nv
			e.sum = sum
			e.data = data
		}
		e.last = time.Now()
	}
	resp := map[string]any{"running": true, "ver": e.ver, "vw": st.VW, "vh": st.VH}
	if e.ver != clientVer && len(e.data) > 0 {
		resp["changed"] = true
		resp["mime"] = "image/webp"
		resp["data"] = base64.StdEncoding.EncodeToString(e.data)
	} else {
		resp["changed"] = false
	}
	writeJSON(w, http.StatusOK, resp)
}

// browserFrameCaptor 内嵌形态的可选帧截取（WebP q60，远程镜像专用——比
// Screenshot 的 PNG 小一个量级，是 2fps 轮询下的带宽主力）。原生桌面面板
// 与无头 chromedp 宿主实现；Electron 面板桥只出 PNG，暂不参与（无实现即
// 由 handler 报「不支持」）。
type browserFrameCaptor interface {
	CaptureFrame() ([]byte, error)
}

// frameEvery 相邻两次截图的最小间隔：远程观察 2fps，450ms 足够跟住且不挤占
// UI 线程（截图经 Dispatch 投递，与用户操作同队列）。
const frameEvery = 450 * time.Millisecond

// browserFrameEntry 一个会话的镜像缓存（自带锁：截图慢时不阻塞其他会话）。
type browserFrameEntry struct {
	mu   sync.Mutex
	ver  int64 // 帧版本=内容变化时刻的毫秒时间戳；0=还没有帧
	last time.Time
	sum  [32]byte // 最后一帧的 SHA-256（内容指纹）
	data []byte
}

// frameForget 清掉该会话的帧镜像缓存（浏览器真关闭后远程端不该再轮询到旧画面）。
func (a *App) frameForget(sid string) {
	a.frameMu.Lock()
	delete(a.frameCache, browserProfileKey(sid))
	a.frameMu.Unlock()
}

func (a *App) handleBrowserPanelRect(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Session string  `json:"session"`
		X       float64 `json:"x"`
		Y       float64 `json:"y"`
		W       float64 `json:"w"`
		H       float64 `json:"h"`
		Dpr     float64 `json:"dpr"`
		Visible bool    `json:"visible"`
		IW      float64 `json:"iw"`      // 临时诊断字段（前端视口宽）
		IH      float64 `json:"ih"`      // 临时诊断字段（前端视口高）
		OZ      float64 `json:"oz"`      // 临时诊断字段（visualViewport.scale）
		Desktop bool    `json:"desktop"` // 上报者是否桌面壳（唯一合法写者）
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "参数解析失败: "+err.Error())
		return
	}
	// 写者校验一：非浏览器来源（Go http 客户端等）一律忽略
	if ua := r.UserAgent(); !strings.Contains(ua, "Mozilla") {
		log.Printf("[panel-rect] 忽略非浏览器来源上报（ua=%q visible=%v）", ua, p.Visible)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	// 写者校验二：带矩形的可见上报必须来自桌面壳（旧前端/异常来源忽略；
	// 隐藏上报不带 desktop 字段是合法的——桌面壳的隐藏分支本就只发 visible）
	if p.Visible && !p.Desktop {
		log.Printf("[panel-rect] 忽略非桌面壳的可见上报（x=%.0f y=%.0f w=%.0f h=%.0f）", p.X, p.Y, p.W, p.H)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	// 原生桌面壳未就位（网页形态/Electron 壳/启动间隙）：没有原生面板可动
	a.panelMu.Lock()
	dispatch := a.panelDispatch
	a.panelMu.Unlock()
	if dispatch == nil {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	log.Printf("[panel-rect-diag] 收到 x=%.1f y=%.1f w=%.1f h=%.1f dpr=%.2f visible=%v session=%s iw=%.0f ih=%.0f oz=%.0f desktop=%v",
		p.X, p.Y, p.W, p.H, p.Dpr, p.Visible, p.Session, p.IW, p.IH, p.OZ, p.Desktop) // 临时诊断日志
	dpr := p.Dpr
	if dpr <= 0 {
		dpr = 1
	}
	// 隐藏上报：作用于当前显示中的面板（见函数头注释的会话路由说明）
	if !p.Visible {
		a.panelHideCurrent()
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	// 坐标换算：前端 x/y/w/h 是相对主 WebView2 视口的 CSS px。主 WebView2
	// 精确铺满主窗口客户区（绑定库 WM_SIZE → GetClientRect → PutBounds），
	// 于是「客户区窗口单位 ÷ 上报视口宽高」= CSS px → 子窗口坐标的实测
	// 比例：100% 缩放下为 1，非 100% 缩放/多屏异 DPI 自我校准。此前两代
	// 实现（先乘 dpr、后假设 1:1）都在部分机器上翻过车——比例是量出来的，
	// 不再是假设。
	sx, sy := 1.0, 1.0
	if cw, ch := mainClientSize(); cw > 0 && ch > 0 && p.IW > 0 && p.IH > 0 {
		sx = clampFloat(float64(cw)/p.IW, 0.5, 3)
		sy = clampFloat(float64(ch)/p.IH, 0.5, 3)
	}
	ps, ok := a.panelFor(p.Session).(interface {
		SetRect(x, y, w, h int, dpr float64, visible bool) error
	})
	if !ok || ps == nil {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	if err := ps.SetRect(
		int(p.X*sx+0.5), int(p.Y*sy+0.5), int(p.W*sx+0.5), int(p.H*sy+0.5),
		dpr, p.Visible); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 该会话面板落位成为当前显示：记账 + 其余面板收屏外（互斥显示）
	a.panelSetVisible(browserProfileKey(p.Session))
	a.panelHideOthers(browserProfileKey(p.Session))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleBrowserPanelOverlay HTML 浮层让位信号：桌面壳里 HTML 无法浮于原生
// 浏览器面板之上，设置等全屏 .modal 打开时前端（app-right.js 的 MutationObserver）
// 上报 on=true，宿主把面板压到主 WebView2 之下（YieldTop）；全部 modal 关闭后
// on=false 恢复提顶。写者纪律同 handleBrowserPanelRect：仅浏览器 UA + desktop
// 标记（网页/远程形态的页面发不来，Electron 壳的 webview 是 DOM 元素天然让位、
// 也不该发）。标记记在 App 级（panelOverlayYield），惰性创建的面板首次落位
// 同样生效。
func (a *App) handleBrowserPanelOverlay(w http.ResponseWriter, r *http.Request) {
	var p struct {
		On      bool `json:"on"`
		Desktop bool `json:"desktop"`
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "参数解析失败: "+err.Error())
		return
	}
	if !p.Desktop || !strings.Contains(r.UserAgent(), "Mozilla") {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	v := int32(0)
	if p.On {
		v = 1
	}
	atomic.StoreInt32(&a.panelOverlayYield, v)
	a.panelMu.Lock()
	dispatch := a.panelDispatch
	a.panelMu.Unlock()
	if dispatch == nil {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	a.panelEach(func(t browserTarget) {
		if bp, ok := t.(interface{ YieldTop(yield bool) }); ok && bp != nil {
			bp.YieldTop(p.On)
		}
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// clampFloat 限幅（比例校准用：异常上报不至于把面板缩出天际）。
func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// fileURLFromAbs 把本地绝对路径拼成合法 file:// URL（逐段转义，保留 / 分隔）。
func fileURLFromAbs(abs string) string {
	segs := strings.Split(strings.TrimPrefix(filepath.ToSlash(abs), "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return "file:///" + strings.Join(segs, "/")
}

// workspaceFileForView 确认 rel 是 sid 会话里可预览的已存在文件（聊天里
// file-link 的点击预览与交付卡 html 预览共用）：用户主动点击触发，不经模型
// 权限门控，也不做工作区边界拦截（这些路由不在 call_api 目录里，AI 调不到
// ——对用户自己的点击做限制违背设计意图）；只查存在性且排除目录。
// 返回会话与解析后的绝对路径（远程会话 = 远程侧绝对路径）。
func (a *App) workspaceFileForView(sid, rel string) (m *SessionMeta, abs string, ok bool) {
	rel = strings.TrimSpace(rel)
	m, err := a.Store.Get(sid)
	if err != nil || m == nil {
		return nil, "", false
	}
	tctx := &toolContext{app: a, sess: m}
	abs, err = tctx.resolve(rel)
	if err != nil {
		return nil, "", false
	}
	if isDir, _, serr := tctx.envStat(abs); serr != nil || isDir {
		return nil, "", false
	}
	return m, abs, true
}

// handleBrowserOpenView 聊天链接点开的统一入口（用户主动操作，不经模型权限门控，
// 也不做工作区边界拦截——本路由不在 call_api 目录里，AI 调不到）：
//   * url：直接在内嵌实例打开（不做 scheme 白名单，浏览器能开什么由它
//     自己决定）；
//   * path：已存在的文件（工作区内外均可）——本地转 file://（同目录相对
//     资源可用），SSH 远程会话转 /api/browser/view 流式地址。
// 一律经 browserFor(p.Session, "") 打开：内嵌实例成为该会话的工具作用目标。不注入
// 任何对话回报（用户打开什么不经模型；需要看画面时用户会说，模型用
// panel_screenshot 截右侧栏）。
func (a *App) handleBrowserOpenView(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Session string `json:"session"`
		URL     string `json:"url"`
		Path    string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "参数解析失败: "+err.Error())
		return
	}
	viewedPath := strings.TrimSpace(p.Path)
	var target string
	var m *SessionMeta
	if u := strings.TrimSpace(p.URL); u != "" {
		target = u // 不做 scheme 白名单：浏览器能开什么由它自己决定（见 Navigate 注释）
	} else if viewedPath != "" {
		sid := strings.TrimSpace(p.Session)
		if sid == "" {
			writeErr(w, http.StatusBadRequest, "打开工作区文件需要 session")
			return
		}
		var abs string
		var ok bool
		if m, abs, ok = a.workspaceFileForView(sid, viewedPath); !ok {
			writeErr(w, http.StatusNotFound, "文件不存在: "+viewedPath)
			return
		}
		if m.Remote != nil {
			// 远程工作区：本机无此文件，走流式渲染端点（相对资源不加载，已知限制）
			q := url.Values{}
			q.Set("sid", m.ID)
			q.Set("path", viewedPath)
			target = "http://" + r.Host + "/api/browser/view?" + q.Encode()
		} else {
			target = fileURLFromAbs(abs)
		}
	} else {
		writeErr(w, http.StatusBadRequest, "需要 url 或 path")
		return
	}
	h := a.browserFor(p.Session, "") // 用户发起的打开：内嵌形态成为该会话的工具作用目标
	if _, _, err := h.Navigate(target); err != nil {
		writeErr(w, http.StatusBadGateway, "打开失败: "+err.Error())
		return
	}
	// 不再注入【网页查看】回报：对话里不显示、模型也不看（用户口径：打开
	// 什么是个人的事，需要时用户会自己说）。模型要看画面走 panel_screenshot
	// 工具（对右侧栏截图，任何视图通用），见 panelshot_windows.go。
	writeJSON(w, http.StatusOK, map[string]string{"url": target})
}

// handleBrowserView 远程（SSH/SFTP）会话文件的流式渲染，仅供内嵌浏览器经
// open-view 转址访问；与 open-view 同口径（用户手势链路，仅存在性检查）。
func (a *App) handleBrowserView(w http.ResponseWriter, r *http.Request) {
	sid := strings.TrimSpace(r.URL.Query().Get("sid"))
	rel := strings.TrimSpace(r.URL.Query().Get("path"))
	if sid == "" || rel == "" {
		writeErr(w, http.StatusBadRequest, "需要 sid 与 path")
		return
	}
	m, abs, ok := a.workspaceFileForView(sid, rel)
	if !ok {
		writeErr(w, http.StatusNotFound, "文件不存在: "+rel)
		return
	}
	tctx := &toolContext{app: a, sess: m}
	data, err := tctx.envReadFile(abs)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// 按扩展名给真实 Content-Type：右栏浏览器经「预览（右栏）」打开远程
	// 会话的图片/音视频时才能正常渲染（旧实现写死 text/html，图片直接坏）。
	w.Header().Set("Content-Type", viewContentType(rel))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// viewableMime 浏览器原生可渲染类型的精确 mime（含 charset 的文本类、图片、
// 音视频、pdf、svg、html）。第二个返回值=false 表示不在表内（未知类型，调用方
// 自行决定降级策略）——inline 打开与 text/plain 兜底都要区分「表内的文本」与
// 「表外的不明类型」（后者按 inline 给 text/plain 会把 docx 当文本乱码显示）。
func viewableMime(name string) (string, bool) {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	switch ext {
	case "html", "htm":
		return "text/html; charset=utf-8", true
	case "svg":
		return "image/svg+xml", true
	case "png", "jpg", "jpeg", "gif", "webp", "bmp", "ico", "avif":
		return "image/" + ext, true
	case "pdf":
		return "application/pdf", true
	case "mp4", "webm":
		return "video/" + ext, true
	case "mp3", "wav", "ogg", "m4a":
		if ext == "m4a" {
			return "audio/mp4", true
		}
		return "audio/" + ext, true
	case "txt", "md", "json", "xml", "csv", "js", "css", "log", "yml", "yaml", "go", "py":
		return "text/plain; charset=utf-8", true
	default:
		return "", false
	}
}

// viewContentType 远程文件流式预览的 Content-Type：浏览器原生可渲染的
// 类型给出精确 mime（含 charset 的文本类、图片、音视频、pdf、svg），不在
// 表内的一律 text/plain——浏览器显示源码/乱码总比错误渲染强。
func viewContentType(name string) string {
	if ct, ok := viewableMime(name); ok {
		return ct
	}
	return "text/plain; charset=utf-8"
}
