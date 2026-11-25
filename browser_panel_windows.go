//go:build windows

package main

// 桌面右栏原生浏览器面板：在桌面壳窗口（desktop_windows.go 的主 WebView2）
// 上叠一个 WS_CHILD 子窗口，内嵌第二个 WebView2 控件——用户得到的是真正的
// Edge 渲染与交互（原生滚动、输入法、右键菜单、下载），替换旧「无头 Edge +
// MJPEG 截图流镜像」方案（browser_host.go 原实现，人机交互体验与原生差距
// 无法靠补丁弥合）。AI 的 browser_* 工具驱动同一控件：人机共操天然成立。
//
// 会话隔离：右栏浏览器每个会话一个独立面板实例（App.panels，键=会话 id），
// 各自独立 WebView2 环境与 profile（登录态/页面互不串台，与有头独立窗口的
// per-session 约定一致）；同一时刻只显示当前会话的面板，其余收屏外（实例
// 保留，切回即恢复）。矩形上报按 session 路由（handleBrowserPanelRect）。
//
// AI 驱动通道：控件级官方 CDP 接口 ICoreWebView2.CallDevToolsProtocolMethod
// （evaluate/输入注入/截图全部可表达）。不走 --remote-debugging-port——新版
// WebView2（Chromium 136 起）已封禁该参数，chromedp 直连不可靠。
//
// 布局同步：右栏地址栏工具栏由主界面渲染（HTML），网页区域是占位 div；前端
// 把占位矩形（CSS px 原值——本进程对 Win32 系统级 DPI 感知，子窗口坐标与
// CSS px 同尺度，不得再乘 dpr，见 handleBrowserPanelRect）POST /api/browser/panel-rect，
// 宿主在 UI 线程移动子窗口并 PutBounds。HTML 元素无法浮于原生子窗口之上，
// 弹层需自行避让该矩形。前端上报的矩形左缘带 6px 内缩（app-right.js 的
// BPW_EDGE）：给右栏左缘的拖拽把手 .rp-resize 留命中区，网页与栏缘之间的
// 背景条是刻意设计，别当对齐 bug 修掉。
//
// 线程模型：所有 COM 调用必须发生在创建控件的 UI 线程（STA）——HTTP 侧一律
// 经桌面壳 WebView.Dispatch 投递，结果经 channel 回到调用方 goroutine。
// 生命周期用 createMu 串行化（创建/关闭），数据字段用 stateMu 短临界区——
// UI 回调（导航完成等）只碰 stateMu，绝不反向拿 createMu，防泵内死锁。
//
// 绑定库边界：jchv/go-webview2 的 pkg/edge 只导出了少量包装（Navigate/
// Show/Hide/GetController…），webview 对象与 CDP/事件面未导出。COM 对象布局
// 是稳定 ABI（首字段即 vtbl 指针），这里用同构 vtbl 镜像结构直接调用，
// 并自制事件回调对象（windows.NewCallback，仿库内 comproc 模式）。

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"
	"unsafe"

	edge "github.com/jchv/go-webview2/pkg/edge"
	"golang.org/x/sys/windows"
)

const (
	wsChild       = 0x40000000
	swpNoZOrder   = 0x0004
	swpNoActivate = 0x0010
	swHide        = 0
	swShow        = 5
	hwndBottom    = 1 // SetWindowPos 的 hWndInsertAfter=HWND_BOTTOM（压到兄弟窗口最底）
)

var (
	panelUser32              = windows.NewLazySystemDLL("user32.dll")
	procPanelCreateWindow    = panelUser32.NewProc("CreateWindowExW")
	procPanelDestroyWindow   = panelUser32.NewProc("DestroyWindow")
	procPanelSetWindowPos    = panelUser32.NewProc("SetWindowPos")
	procPanelGetWindowRect   = panelUser32.NewProc("GetWindowRect") // 临时诊断用
	procPanelShowWindow      = panelUser32.NewProc("ShowWindow")
	panelKernel32            = windows.NewLazySystemDLL("kernel32.dll")
	procPanelGetModuleHandle = panelKernel32.NewProc("GetModuleHandleW")
)

// ---------- COM vtbl 镜像（字段顺序 = Microsoft COM ABI，不可改动顺序） ----------

// wvRawVtbl 镜像 edge.iCoreWebView2Vtbl（ICoreWebView2）。
type wvRawVtbl struct {
	queryInterface, addRef, release                                                edge.ComProc
	getSettings, getSource, navigate, navigateToString                             edge.ComProc
	addNavigationStarting, removeNavigationStarting                                edge.ComProc
	addContentLoading, removeContentLoading                                        edge.ComProc
	addSourceChanged, removeSourceChanged                                          edge.ComProc
	addHistoryChanged, removeHistoryChanged                                        edge.ComProc
	addNavigationCompleted, removeNavigationCompleted                              edge.ComProc
	addFrameNavigationStarting, removeFrameNavigationStarting                      edge.ComProc
	addFrameNavigationCompleted, removeFrameNavigationCompleted                    edge.ComProc
	addScriptDialogOpening, removeScriptDialogOpening                              edge.ComProc
	addPermissionRequested, removePermissionRequested                              edge.ComProc
	addProcessFailed, removeProcessFailed                                          edge.ComProc
	addScriptToExecuteOnDocumentCreated, removeScriptToExecuteOnDocumentCreated    edge.ComProc
	executeScript, capturePreview, reload                                          edge.ComProc
	postWebMessageAsJSON, postWebMessageAsString                                   edge.ComProc
	addWebMessageReceived, removeWebMessageReceived                                edge.ComProc
	callDevToolsProtocolMethod, getBrowserProcessID, getCanGoBack, getCanGoForward edge.ComProc
	goBack, goForward, getDevToolsProtocolEventReceiver, stop                      edge.ComProc
	addNewWindowRequested, removeNewWindowRequested                                edge.ComProc
	addDocumentTitleChanged, removeDocumentTitleChanged, getDocumentTitle          edge.ComProc
	addHostObjectToScript, removeHostObjectFromScript, openDevToolsWindow          edge.ComProc
	addContainsFullScreenElementChanged, removeContainsFullScreenElementChanged    edge.ComProc
	getContainsFullScreenElement                                                   edge.ComProc
	addWebResourceRequested, removeWebResourceRequested                            edge.ComProc
	addWebResourceRequestedFilter, removeWebResourceRequestedFilter                edge.ComProc
	addWindowCloseRequested, removeWindowCloseRequested                            edge.ComProc
}

// ctlRawVtbl 镜像 edge._ICoreWebView2ControllerVtbl。
type ctlRawVtbl struct {
	queryInterface, addRef, release                       edge.ComProc
	getIsVisible, putIsVisible, getBounds, putBounds      edge.ComProc
	getZoomFactor, putZoomFactor                          edge.ComProc
	addZoomFactorChanged, removeZoomFactorChanged         edge.ComProc
	setBoundsAndZoomFactor, moveFocus                     edge.ComProc
	addMoveFocusRequested, removeMoveFocusRequested       edge.ComProc
	addGotFocus, removeGotFocus                           edge.ComProc
	addLostFocus, removeLostFocus                         edge.ComProc
	addAcceleratorKeyPressed, removeAcceleratorKeyPressed edge.ComProc
	getParentWindow, putParentWindow                      edge.ComProc
	notifyParentWindowPositionChanged, close              edge.ComProc
	getCoreWebView2                                       edge.ComProc
}

// panelRect 即 COREWEBVIEW2_PHYSICAL_BOUNDS（tagRECT 布局）。
type panelRect struct{ Left, Top, Right, Bottom int32 }

func wvVtblOf(wv *edge.ICoreWebView2) *wvRawVtbl {
	return *(**wvRawVtbl)(unsafe.Pointer(wv))
}

func ctlVtblOf(ctl *edge.ICoreWebView2Controller) *ctlRawVtbl {
	return *(**ctlRawVtbl)(unsafe.Pointer(ctl))
}

// ---------- 自制 COM 回调对象（仿库内 comproc 模式） ----------

// panelRecover 的实现在 browser_host.go（跨平台文件）：关闭阶段二的后台
// goroutine 也要用它兜 panic，所以不留在本平台文件里。

var (
	comQIProc  = windows.NewCallback(func(this, iid, obj uintptr) uintptr { return 0 })
	comRefProc = windows.NewCallback(func(this uintptr) uintptr { return 1 })
)

// evtHandler 通用双参事件回调（sender/args 均为 COM 接口指针）。
type evtHandler struct {
	vtbl *evtHandlerVtbl
	fn   func(sender, args unsafe.Pointer)
}

type evtHandlerVtbl struct{ queryInterface, addRef, release, invoke uintptr }

var evtHandlerVtblInst = evtHandlerVtbl{
	comQIProc, comRefProc, comRefProc,
	windows.NewCallback(func(this *evtHandler, sender, args unsafe.Pointer) uintptr {
		defer panelRecover("事件回调")
		if this != nil && this.fn != nil {
			this.fn(sender, args)
		}
		return 0
	}),
}

func newEvtHandler(fn func(sender, args unsafe.Pointer)) *evtHandler {
	return &evtHandler{vtbl: &evtHandlerVtblInst, fn: fn}
}

type cdpResult struct {
	hr   int32
	json string
}

// cdpCompletedHandler 是 ICoreWebView2CallDevToolsProtocolMethodCompletedHandler
// 的 Go 实现（绑定库未导出该回调接口）。returnObjectAsJson 字符串仅在回调
// 期间有效，复制后立即回传。
type cdpCompletedHandler struct {
	vtbl *cdpCompletedVtbl
	ch   chan cdpResult
}

type cdpCompletedVtbl struct{ queryInterface, addRef, release, invoke uintptr }

var cdpCompletedVtblInst = cdpCompletedVtbl{
	comQIProc, comRefProc, comRefProc,
	windows.NewCallback(func(this *cdpCompletedHandler, errCode uintptr, resultJSON *uint16) uintptr {
		defer panelRecover("CDP 回调")
		if this == nil || this.ch == nil {
			return 0
		}
		unretireCdpHandler(this) // 超时路径挂过保活的：回调真到了，可以放手
		r := cdpResult{hr: int32(uint32(errCode))}
		if resultJSON != nil {
			r.json = copyWVString(resultJSON)
		}
		select {
		case this.ch <- r:
		default:
		}
		return 0
	}),
}

// cdpRetired 超时后仍在原生侧挂着的 CDP 完成回调对象保活表。
// callCDP 的超时分支一旦返回，defer KeepAlive 即释放对象——但 evaluate 跑了
// 60 秒还没回不代表永远不回（重页面/长 XHR），完成回调可以更晚才到。挂进
// 表里等回调真到时自摘（unretireCdpHandler），不然就是又一个「原生指针指向
// 已回收内存」的静默闪退窗口（同 retirePanelChromium 的事故类）。
var (
	cdpRetiredMu sync.Mutex
	cdpRetired   = map[*cdpCompletedHandler]struct{}{}
)

func retireCdpHandler(h *cdpCompletedHandler) {
	cdpRetiredMu.Lock()
	cdpRetired[h] = struct{}{}
	cdpRetiredMu.Unlock()
}

func unretireCdpHandler(h *cdpCompletedHandler) {
	cdpRetiredMu.Lock()
	delete(cdpRetired, h)
	cdpRetiredMu.Unlock()
}

// ---------- 字符串工具 ----------

// copyWVString 复制 WebView2 给出的 UTF-16 字符串（不释放，调用方内存）。
func copyWVString(p *uint16) string {
	if p == nil {
		return ""
	}
	n := 0
	for *(*uint16)(unsafe.Add(unsafe.Pointer(p), 2*n)) != 0 {
		n++
		if n > 1<<20 { // 保险丝：异常指针不至于死循环
			break
		}
	}
	return string(utf16.Decode(unsafe.Slice(p, n)))
}

// readWVString 复制并释放调用 get_* 方法取回的出参字符串（CoTaskMem 分配）。
func readWVString(p *uint16) string {
	s := copyWVString(p)
	if p != nil {
		windows.CoTaskMemFree(unsafe.Pointer(p))
	}
	return s
}

func wvGetSource(wv *edge.ICoreWebView2) string {
	var p *uint16
	_, _, _ = wvVtblOf(wv).getSource.Call(uintptr(unsafe.Pointer(wv)), uintptr(unsafe.Pointer(&p)))
	return readWVString(p)
}

func wvGetDocumentTitle(wv *edge.ICoreWebView2) string {
	var p *uint16
	_, _, _ = wvVtblOf(wv).getDocumentTitle.Call(uintptr(unsafe.Pointer(wv)), uintptr(unsafe.Pointer(&p)))
	return readWVString(p)
}

// ctlWebView 经控制器 vtbl 取 CoreWebView2（绑定库未导出该包装）。
func ctlWebView(ctl *edge.ICoreWebView2Controller) *edge.ICoreWebView2 {
	var wv *edge.ICoreWebView2
	_, _, _ = ctlVtblOf(ctl).getCoreWebView2.Call(uintptr(unsafe.Pointer(ctl)), uintptr(unsafe.Pointer(&wv)))
	return wv
}

// ---------- 面板宿主 ----------

type browserPanel struct {
	app      *App
	dispatch func(f func()) // 桌面壳 WebView.Dispatch：投递到 UI 线程
	mainWnd  uintptr        // 桌面壳主窗口 HWND
	sid      string         // 面板归属的会话 id（空=共享面板）：profile 目录与 browser_nav 广播的 SessionID 用

	createMu  sync.Mutex // 生命周期串行化（创建/关闭），不跨 UI 等待持锁于 UI 线程
	createErr error

	stateMu                    sync.Mutex // 数据锁：短临界区，UI 回调也仅持此锁
	started                    bool
	chrome                     *edge.Chromium
	ctl                        *edge.ICoreWebView2Controller
	wv                         *edge.ICoreWebView2
	hwnd                       uintptr
	visible                    bool
	rectX, rectY, rectW, rectH int // 舞台矩形（与 CSS px 同尺度的窗口单位）
	dpr                        float64
	cssW, cssH                 int // 面板视口的 CSS px（= 舞台矩形宽高）
	url, title                 string
	navCh                      chan struct{} // 最近一次导航的完成信号（NavigationCompleted 关闭）

	// 事件 token：关闭时反注册（remove_XXX）要用；注册时由 WebView2 写入。
	titleToken int64
	closing       bool          // 关闭已发起：迟到事件一律跳过，不碰将亡控件
	closingDone   chan struct{} // 本实例正在进行的那次关闭（等进程退出+拆窗口）的完成信号；nil=没有在关（见 closePanel）
	dragHold      int32         // 1 = 用户正在拖动/缩放主窗口：暂停面板落位（见 holdForWindowDrag）
	titleHandler  *evtHandler
}

func newBrowserPanel(app *App, dispatch func(f func()), mainWnd uintptr, sid string) browserTarget {
	return &browserPanel{app: app, dispatch: dispatch, mainWnd: mainWnd, sid: sid}
}

// ensureStarted 惰性创建面板控件（首个矩形同步或首次导航触发）。创建在
// UI 线程执行（Embed 内部靠消息泵等环境就绪），HTTP 侧等 channel。
func (p *browserPanel) ensureStarted() error {
	if p.dispatch == nil {
		return fmt.Errorf("桌面窗口尚未就绪（可能正在热重启），稍后重试")
	}
	p.waitCloseSettled() // 上一轮关闭还在等进程退出：先等它关完，别在同一 profile 上叠新环境
	p.createMu.Lock()
	defer p.createMu.Unlock()
	p.stateMu.Lock()
	started, cerr := p.started, p.createErr
	p.stateMu.Unlock()
	if started {
		return nil // 已创建
	}
	if cerr != nil {
		return cerr
	}
	done := make(chan error, 1)
	p.dispatch(func() {
		// 创建链路过 WebView2 的环境/控制器创建回调（库级帧，panic 冒出去即
		// 整进程死）：兜住并把失败/panic 都变成回传给 HTTP 侧的 error。
		err := error(nil)
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("浏览器面板创建 panic: %v", r)
					log.Printf("[browser-panel] 创建 panic（已兜住）: %v\n%s", r, debug.Stack())
				}
			}()
			err = p.createOnUI()
		}()
		done <- err
	})
	select {
	case err := <-done:
		if err != nil {
			p.stateMu.Lock()
			p.createErr = err
			p.stateMu.Unlock()
			return fmt.Errorf("浏览器面板创建失败: %w", err)
		}
		p.stateMu.Lock()
		p.started = true
		p.stateMu.Unlock()
		return nil
	case <-time.After(30 * time.Second):
		err := fmt.Errorf("浏览器面板创建超时")
		p.stateMu.Lock()
		p.createErr = err
		p.stateMu.Unlock()
		return err
	}
}

// withPanelBrowserArgs 给面板控件的 WebView2 环境合并浏览器参数（仅创建
// 期间生效，用毕还原）：与主界面同款防遮挡开关 CalculateNativeWinOcclusion。
// 主窗口靠它在被遮挡/挪屏后仍持续出帧；面板此前一直没带——控件被挪到屏外
// （隐藏态）再回来后，Chromium 的原生遮挡判定会让合成器停帧：能导航、地址
// 栏与标题正常、画面纯白（desktop.log 2026-10-13 14:45:50：落位正确仍白屏）。
// 走官方 WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS 通道（只在环境创建时读取），
// newDesktopFrontend 同款 set/restore 模式；面板创建在 UI 线程经 Dispatch
// 串行，与主窗口的创建不会并发。
func withPanelBrowserArgs(f func()) {
	const feat = "CalculateNativeWinOcclusion"
	prev, had := os.LookupEnv(webView2ArgsEnv)
	merged := prev
	if !strings.Contains(prev, feat) {
		const df = "--disable-features="
		if i := strings.Index(prev, df); i >= 0 {
			merged = prev[:i+len(df)] + feat + "," + prev[i+len(df):]
		} else {
			merged = strings.TrimSpace(strings.TrimSpace(prev) + " " + df + feat)
		}
	}
	if merged != prev {
		_ = os.Setenv(webView2ArgsEnv, merged)
	}
	defer func() {
		if had {
			_ = os.Setenv(webView2ArgsEnv, prev)
		} else {
			_ = os.Unsetenv(webView2ArgsEnv)
		}
	}()
	f()
}

// dataPath 面板的 WebView2 数据目录（每会话独立 profile，与 createOnUI 共用
// 同一口径）。
func (p *browserPanel) dataPath() string {
	if key := browserProfileKey(p.sid); key != "" {
		return filepath.Join(p.app.Cfg.DataDir, "webview-browsers", key)
	}
	return filepath.Join(p.app.Cfg.DataDir, "webview-browser")
}

// ApplyGrants 预授权内容设置（browser_open 的 grants 参数，见
// writeProfileGrants）：面板的 WebView2 同为 Chromium profile 结构，注入
// 同款。Preferences 只在浏览器启动时读取，所以实例还在就必须让它真关干净
// ——走与日常关闭完全相同的序列（closePanel：发退出指令 → 等浏览器进程退出
// → 拆窗口），本函数等它关完再落盘，退场覆写才不会冲掉注入。
func (p *browserPanel) ApplyGrants(grants []string) error {
	if len(grants) == 0 {
		return nil
	}
	dir := p.dataPath()
	// 没在跑时它就是立刻可读的 channel
	<-p.closePanel("预授权写 profile（Preferences 需实例先真退场）")
	// 别人已经发起的那次关闭也在等：同一个 profile 不能边退场边写
	p.waitCloseSettled()
	waitProfileQuiet(dir, 5*time.Second)
	return writeProfileGrants(dir, grants)
}

// createOnUI 在 UI 线程创建子窗口与该会话的 WebView2 控件（独立环境与
// profile：data/webview-browsers/<会话>，与主界面 data/webview 隔离，同浏览器
// 模型于旧方案的 data/browser-profile）。
func (p *browserPanel) createOnUI() error {
	p.stateMu.Lock()
	x, y, w, h := p.rectX, p.rectY, p.rectW, p.rectH
	p.stateMu.Unlock()
	if w < 64 {
		w, x = 64, 0
	}
	if h < 64 {
		h, y = 64, 0
	}
	cls, _ := windows.UTF16PtrFromString("Static") // 系统控件类做中性容器
	name, _ := windows.UTF16PtrFromString("JXBrowserPanel")
	hInst, _, _ := procPanelGetModuleHandle.Call(0)
	hwnd, _, _ := procPanelCreateWindow.Call(
		0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(name)), wsChild,
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		p.mainWnd, 0, hInst, 0)
	if hwnd == 0 {
		return fmt.Errorf("CreateWindowExW 失败")
	}
	ch := edge.NewChromium()
	// 每会话独立 profile（data/webview-browsers/<会话>/）：页面与登录态互不
	// 串台；空会话（首页态点链接等无会话上下文的入口）沿用旧共享目录。
	// 不同 user-data-folder 的多个 WebView2 环境在同一进程共存，各带一组
	// 独立浏览器进程——惰性创建（首个矩形/导航才建），不用浏览器的会话零开销。
	ch.DataPath = p.dataPath()
	// 库级回调帧（go-webview2 的 comproc 直接调进来，不经过我们的 evtHandler）：
	// 这里 panic 就是整进程无声退出，先就地兜住再转给 onNavCompleted（它自己
	// 还有一层，两道都不多余）。
	ch.NavigationCompletedCallback = func(*edge.ICoreWebView2, *edge.ICoreWebView2NavigationCompletedEventArgs) {
		defer panelRecover("NavigationCompleted（库回调）")
		p.onNavCompleted()
	}
	var embedded bool
	withPanelBrowserArgs(func() { embedded = ch.Embed(hwnd) }) // 内部消息泵等环境/控制器就绪
	if !embedded {
		_, _, _ = procPanelDestroyWindow.Call(hwnd)
		return fmt.Errorf("WebView2 控件初始化失败")
	}
	ctl := ch.GetController()
	if ctl == nil {
		return fmt.Errorf("WebView2 控件未就绪")
	}
	wv := ctlWebView(ctl)
	if wv == nil {
		return fmt.Errorf("CoreWebView2 不可用")
	}
	// 新窗口语义（target=_blank / window.open / 中键）由文档创建脚本原地收敛
	// （blankInterceptorJS），不再挂原生 NewWindowRequested 处理器——其 args
	// vtbl 槽位与新版运行时错位，调用即写访问冲突整进程闪退（2026-11-18 实测
	// 复现，put_Handled 槽5 中招）。脚本注入走主接口 vtbl（navigate 与
	// CallDevToolsProtocolMethod 之间的槽位经日常工具链路实证无错位）。
	// 注入完成回调只来一次：直接挂保活表防 GC 踩裸指针（同 retire 常规）。
	var tt, swt int64
	swh := newEvtHandler(func(sender, args unsafe.Pointer) {})
	s16, _ := windows.UTF16PtrFromString(blankInterceptorJS)
	_, _, _ = wvVtblOf(wv).addScriptToExecuteOnDocumentCreated.Call(
		uintptr(unsafe.Pointer(wv)), uintptr(unsafe.Pointer(s16)), uintptr(unsafe.Pointer(swh)), uintptr(unsafe.Pointer(&swt)))
	retireEvtHandler(swh)
	th := newEvtHandler(func(sender, args unsafe.Pointer) { p.onTitleChanged() })
	_, _, _ = wvVtblOf(wv).addDocumentTitleChanged.Call(
		uintptr(unsafe.Pointer(wv)), uintptr(unsafe.Pointer(th)), uintptr(unsafe.Pointer(&tt)))
	p.stateMu.Lock()
	p.chrome, p.ctl, p.wv, p.hwnd = ch, ctl, wv, hwnd
	p.closing = false
	p.titleHandler = th
	p.titleToken = tt
	p.stateMu.Unlock()
	p.applyRectOnUI()
	return nil
}

// applyRectOnUI 把缓存的矩形应用到子窗口与控件（仅 UI 线程调用）。
// 隐藏统一走「屏外 + 保持 IsVisible=true」：实测 IsVisible=false 会让合成器
// 停帧，且恢复 true 后不保证续帧——「地址栏与标题都正常、画面纯白空白」的
// 根源正是旧的 w<8 分支（零矩形上报触发的 PutIsVisible(false)）。
//
// Z 序（真正的白屏根因，2026-10-13 定位）：面板子窗口必须浮在主 WebView2
// 之上才能被看见，而主 WebView2 铺满整个客户区、且 Chromium 会在激活/聚焦
// 时把自己重新提到兄弟窗口顶——CreateWindowExW 出生时在顶上不够，每次落位
// 都要显式 HWND_TOP 重申；主窗口激活/Z 序变化时另有 reassertPanelTop 去抖
// 兜底（见 desktop_windows.go 的 mainWndSubclassProc）。实测：面板被压到主
// WebView2 之下时导航/标题/CDP 全部正常，画面永远看不见。
// 例外：HTML 浮层让位态（App.panelOverlayYield，设置等全屏 .modal 打开期间）
// 有意压到 HWND_BOTTOM——主 WebView2 铺满客户区且不透明，压底即整个被盖住，
// 浮层得以完整显示在浏览器之上（见 YieldTop；HTML 无法浮于原生子窗口之上，
// 这是唯一的让路方式）。
func (p *browserPanel) applyRectOnUI() {
	defer panelRecover("applyRectOnUI") // 整段都是 COM 调用，落在 UI 线程原生栈上：panic 冒出去就是整进程死
	if atomic.LoadInt32(&p.dragHold) != 0 {
		// 用户正在拖动/缩放主窗口：本轮不落位（见 holdForWindowDrag）。面板是
		// 主窗口的子窗口，会跟着窗口一起动；拖动结束由 releaseAfterWindowDrag
		// 用缓存矩形一次性落位。
		return
	}
	p.stateMu.Lock()
	x, y, w, h, visible := p.rectX, p.rectY, p.rectW, p.rectH, p.visible
	ctl, hwnd := p.ctl, p.hwnd
	p.stateMu.Unlock()
	if ctl == nil || hwnd == 0 {
		return
	}
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if w < 8 || h < 8 {
		// 尚无有效矩形（创建后首个矩形未到）：占位尺寸，反正在屏外
		w, h = 64, 64
	}
	if !visible {
		x, y = -32001, -32001 // 屏外；控件保持 IsVisible=true 继续出帧
	}
	// 让位态（HTML 浮层开着）落位也压底：浮层期间窗口 resize 等触发的重落位
	// 不得把面板提回顶。
	ins := uintptr(0) // HWND_TOP：每次落位都把面板提回兄弟窗口顶（见上方 Z 序注释）
	if p.app != nil && atomic.LoadInt32(&p.app.panelOverlayYield) != 0 {
		ins = hwndBottom
	}
	// 强制一次真实位移（x+1 → x）：SetWindowPos 对相同坐标会被系统优化掉，
	// 合成器偶发停帧（画面纯白、标题正常）后，没有真实的窗口位置事件就唤
	// 不回来——实测同矩形重发不重绘，挪 1px 立即恢复。两步都在同一条 UI
	// 消息里完成，中间不会发生绘制，肉眼不可见。
	_, _, _ = procPanelSetWindowPos.Call(hwnd, ins, uintptr(x+1), uintptr(y), uintptr(w), uintptr(h),
		swpNoActivate)
	_, _, _ = procPanelSetWindowPos.Call(hwnd, ins, uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		swpNoActivate)
	if procPanelGetWindowRect.Find() == nil { // 临时诊断回读：SetWindowPos 之后窗口真实落在哪
		var rr panelRect
		if rb, _, _ := procPanelGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rr))); rb != 0 {
			log.Printf("[panel-rect-diag] apply 目标=(%d,%d,%dx%d) 实际落位=(%d,%d)-(%d,%d) %dx%d",
				x, y, w, h, rr.Left, rr.Top, rr.Right, rr.Bottom, rr.Right-rr.Left, rr.Bottom-rr.Top)
		}
	}
	_, _, _ = procPanelShowWindow.Call(hwnd, swShow)
	r := panelRect{0, 0, int32(w), int32(h)}
	_, _, _ = ctlVtblOf(ctl).putBounds.Call(uintptr(unsafe.Pointer(ctl)), uintptr(unsafe.Pointer(&r)))
	_ = ctl.PutIsVisible(true)
	_ = ctl.NotifyParentWindowPositionChanged()
}

// reassertTopOnUI 把面板子窗口提回兄弟窗口顶（去抖 150ms）。Chromium 的主
// WebView2 在窗口激活/聚焦时会把自己重新提到顶上，面板随即被压到它下面——
// 用户视角是「右栏一眨眼变成空白，地址栏还正常」。主窗口的 WM_ACTIVATE /
// Z 序变化消息会触发这里（desktop_windows.go 的 mainWndSubclassProc）；
// 仅 UI 线程调用。HWND_TOP 对已在顶部的窗口是无害空操作。
var lastPanelReassertMs int64

func (p *browserPanel) reassertTopOnUI() {
	if p.app != nil && atomic.LoadInt32(&p.app.panelOverlayYield) != 0 {
		return // HTML 浮层让位态：面板就该在主 WebView2 之下，激活/Z 序重申一概不做
	}
	now := time.Now().UnixMilli()
	if now-atomic.LoadInt64(&lastPanelReassertMs) < 150 {
		return
	}
	atomic.StoreInt64(&lastPanelReassertMs, now)
	p.stateMu.Lock()
	hwnd, visible := p.hwnd, p.visible
	p.stateMu.Unlock()
	if hwnd == 0 || !visible {
		return
	}
	_, _, _ = procPanelSetWindowPos.Call(hwnd, 0, 0, 0, 0, 0,
		swpNoSizeFlag|swpNoMoveFlag|swpNoActivate)
}

// YieldTop HTML 全屏浮层（设置等 .modal）的让位开关。桌面壳里 HTML 无法
// 浮于原生子窗口之上（见文件头「布局同步」），全屏浮层要盖住浏览器只有把
// 面板压到主 WebView2 之下——主 WebView2 铺满客户区且不透明，压底即整个被
// 盖住，浮层（连同其遮罩）完整显示。前端 MutationObserver 监视 .modal 显隐
// 上报 /api/browser/panel-overlay（browser_host.go 的 handleBrowserPanelOverlay
// 置 App.panelOverlayYield 并遍历调这里）。让位标记在 App 级：浮层开着时才
// 惰性创建的面板、或多会话切换到的面板，首次落位（applyRectOnUI）同样让位。
func (p *browserPanel) YieldTop(yield bool) {
	if p.dispatch == nil {
		return
	}
	p.dispatch(func() { p.applyYieldOnUI(yield) })
}

// applyYieldOnUI 让位/恢复的 UI 线程动作（YieldTop 投递）。让位=纯 Z 序压底
// （不碰矩形，规避拖动/落位的重入）；恢复走 applyRectOnUI 整链——提顶之外
// 还有 +1px 强制位移，被盖住一阵的合成器帧面（万一停帧）靠它救回。
func (p *browserPanel) applyYieldOnUI(yield bool) {
	defer panelRecover("applyYieldOnUI")
	if atomic.LoadInt32(&p.dragHold) != 0 {
		return // 拖动中：标记已在 App 级，拖完 releaseAfterWindowDrag 的落位按它执行
	}
	p.stateMu.Lock()
	hwnd, visible := p.hwnd, p.visible
	p.stateMu.Unlock()
	if hwnd == 0 || !visible {
		return // 未创建/屏外面板：标记已在 App 级，下次落位生效
	}
	if yield {
		_, _, _ = procPanelSetWindowPos.Call(hwnd, hwndBottom, 0, 0, 0, 0,
			swpNoSizeFlag|swpNoMoveFlag|swpNoActivate)
		return
	}
	p.applyRectOnUI()
}

// holdForWindowDrag / releaseAfterWindowDrag：用户拖动/缩放主窗口期间的面板让路。
// 由 desktop_windows.go 的 WM_ENTERSIZEMOVE / WM_EXITSIZEMOVE 钩子调用（仅 UI 线程）。
// 拖动期间系统正在模态移动循环里驱动窗口几何，此刻对 WebView2 子窗口做
// SetWindowPos / putBounds / NotifyParentWindowPositionChanged，等于把这些 COM 调用
// 插进系统的拖动循环里：拖动会错乱，控制器还在同一条回调栈上重入——2026-10-14
// 「全屏拖动闪退」排查中锁定的最后一段自研动作。拖动中只打标记不落位，拖完用缓存
// 矩形补落一次（面板是子窗口，拖动期间本来就跟着窗口走，视觉不跳）。
func (p *browserPanel) holdForWindowDrag() {
	atomic.StoreInt32(&p.dragHold, 1)
}

func (p *browserPanel) releaseAfterWindowDrag() {
	if atomic.CompareAndSwapInt32(&p.dragHold, 1, 0) {
		p.dispatch(p.applyRectOnUI)
	}
}

// SetRect 前端矩形同步入口（CSS px 原值，见 handleBrowserPanelRect 的换算
// 注释）；visible=true 时确保面板已创建。
func (p *browserPanel) SetRect(x, y, w, h int, dpr float64, visible bool) error {
	if dpr <= 0 {
		dpr = 1
	}
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if visible && (w < 8 || h < 8) {
		visible = false // 布局未就绪/极小：按隐藏处理，别用脏矩形
	}
	p.stateMu.Lock()
	// 只采纳合法矩形：隐藏上报只带 visible 字段（x/y/w/h 全零），清掉旧尺寸
	// 会让屏外分支退化成 64x64 占位，回来时还得等一次完整矩形
	if w >= 8 && h >= 8 {
		p.rectX, p.rectY, p.rectW, p.rectH = x, y, w, h
	}
	p.dpr, p.visible = dpr, visible
	if visible {
		// w/h 已是 CSS px（见 handleBrowserPanelRect 的换算注释：CSS px 与
		// 子窗口坐标 1:1），面板视口即舞台尺寸，不再除 dpr。
		p.cssW = clampInt(w, 1, 65535)
		p.cssH = clampInt(h, 1, 65535)
	}
	created := p.ctl != nil
	p.stateMu.Unlock()
	// 先存矩形、后创建：首个可见矩形到达时 createOnUI 读到的就是真实矩形，
	// 控件直接在目标位置出生。旧顺序创建先于存值，控件以 64x64 生在屏外、
	// 再靠位移挪回——「屏外出生」的控件合成器偶发不再出帧（能导航、纯白）。
	if visible && !created {
		if err := p.ensureStarted(); err != nil {
			return err
		}
	}
	p.stateMu.Lock()
	created = p.ctl != nil
	p.stateMu.Unlock()
	if !created {
		return nil
	}
	p.dispatch(p.applyRectOnUI)
	return nil
}

// ---------- 导航 ----------

// Navigate 打开/跳转地址，返回落定地址与标题。不做 scheme 白名单——
// 浏览器（这里是 WebView2）能开什么由它自己决定，开不了自然报错。
func (p *browserPanel) Navigate(raw string) (string, string, error) {
	return p.navigate(raw, true)
}

func (p *browserPanel) navigate(raw string, allowFile bool) (string, string, error) {
	u := strings.TrimSpace(raw)
	if u == "" {
		return "", "", fmt.Errorf("地址为空")
	}
	_ = allowFile // 历史参数：白名单已拆除，保留签名免动调用点
	if err := p.ensureStarted(); err != nil {
		return "", "", err
	}
	p.stateMu.Lock()
	if p.navCh != nil {
		close(p.navCh) // 旧等待者放行（读到中间态）
	}
	p.navCh = make(chan struct{})
	ch := p.navCh
	wv := p.wv
	p.stateMu.Unlock()
	p.dispatch(func() {
		if wv == nil {
			return
		}
		u16, _ := windows.UTF16PtrFromString(u)
		_, _, _ = wvVtblOf(wv).navigate.Call(uintptr(unsafe.Pointer(wv)), uintptr(unsafe.Pointer(u16)))
	})
	select {
	case <-ch:
	case <-time.After(browserTimeout):
		return "", "", fmt.Errorf("导航超时（60 秒）: %s", u)
	}
	p.stateMu.Lock()
	url, title := p.url, p.title
	p.stateMu.Unlock()
	return url, title, nil
}

func (p *browserPanel) NavAction(action string) error {
	if err := p.ensureStarted(); err != nil {
		return err
	}
	p.stateMu.Lock()
	wv := p.wv
	if p.navCh != nil {
		close(p.navCh)
	}
	p.navCh = make(chan struct{})
	ch := p.navCh
	p.stateMu.Unlock()
	if wv == nil {
		return fmt.Errorf("浏览器面板尚未就绪")
	}
	p.dispatch(func() {
		v := wvVtblOf(wv)
		switch action {
		case "back":
			_, _, _ = v.goBack.Call(uintptr(unsafe.Pointer(wv)))
		case "forward":
			_, _, _ = v.goForward.Call(uintptr(unsafe.Pointer(wv)))
		case "reload":
			_, _, _ = v.reload.Call(uintptr(unsafe.Pointer(wv)))
		}
	})
	// 无历史可跳时不会触发 NavigationCompleted：短等即读当前态，不报错
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
	}
	url, title := p.uiReadNav()
	p.stateMu.Lock()
	p.url, p.title = url, title
	p.stateMu.Unlock()
	return nil
}

// onNavCompleted NavigationCompleted（UI 线程）：读回地址标题、广播、放行等待者。
func (p *browserPanel) onNavCompleted() {
	defer panelRecover("onNavCompleted")
	p.stateMu.Lock()
	if p.closing {
		p.stateMu.Unlock()
		return // 正在关闭：不再碰将亡控件
	}
	wv, ch := p.wv, p.navCh
	p.navCh = nil
	p.stateMu.Unlock()
	if wv == nil {
		if ch != nil {
			close(ch)
		}
		return
	}
	u := wvGetSource(wv)
	t := wvGetDocumentTitle(wv)
	p.stateMu.Lock()
	p.url, p.title = u, t
	p.stateMu.Unlock()
	p.app.broadcast(Event{Type: "browser_nav", URL: u, Title: t, SessionID: p.sid})
	// 后端自 kick：导航完成即按缓存矩形重排一次（applyRectOnUI 带 1px 强制
	// 位移，能唤醒偶发停帧的合成器）。前端在 browser_nav 事件上也会补报，
	// 这里不依赖它——双保险，任何一端到即可恢复画面。
	p.applyRectOnUI()
	if ch != nil {
		close(ch)
	}
	// 标题常在完成后一拍才就绪（SPA 渐进渲染）：稍后补读一次
	time.AfterFunc(600*time.Millisecond, func() { p.dispatch(p.onTitleChanged) })
}

func (p *browserPanel) onTitleChanged() {
	defer panelRecover("onTitleChanged")
	p.stateMu.Lock()
	if p.closing {
		p.stateMu.Unlock()
		return
	}
	wv, old := p.wv, p.title
	p.stateMu.Unlock()
	if wv == nil {
		return
	}
	t := wvGetDocumentTitle(wv)
	if t == old {
		return
	}
	p.stateMu.Lock()
	p.title = t
	u := p.url
	p.stateMu.Unlock()
	p.app.broadcast(Event{Type: "browser_nav", URL: u, Title: t, SessionID: p.sid})
}

// blankInterceptorJS 文档创建时注入的新窗口语义收敛脚本：target=_blank 的
// 链接点击（含 Ctrl/Shift 左键）、中键（auxclick）、_blank 表单与 window.open
// 一律改为本面板原地导航——NewWindowRequested 不再触发。
//
// 为什么不用原生 NewWindowRequested 处理器：其 args vtbl 按 0.9.x 头文件序
// 排布（get_Uri=槽3、put_Handled=槽5），新版运行时实际布局已变（get_Uri 碰
// 巧仍在槽3，put_Handled 槽5 指向别处）——2026-11-18 实测点 target=_blank
// 链接，put_Handled.Call 即 0xc0000005 写访问冲突，整进程无声闪退（recover
// 兜不住硬件异常），详见《排查报告-20261118-右栏浏览器点超链接闪退》。逐槽
// 重测的实证成本是整进程崩溃（与 desktop_navguard_windows.go 头注释同款结
// 论），JS 拦截完全不碰 COM ABI，运行时升级免疫。漏网的浏览器内部路径走
// WebView2 默认弹窗兜底（不崩，只是偶尔弹独立小窗）。
const blankInterceptorJS = `(function(){
  var toBlank = function(el){
    if (!el || !el.closest) return null;
    var a = el.closest('a[href]');
    if (!a || a.hasAttribute('download')) return null;
    var t = (a.target || '').toLowerCase();
    if (t === '' || t === '_self' || t === '_parent' || t === '_top') return null;
    return a;
  };
  document.addEventListener('click', function(e){
    var a = toBlank(e.target);
    if (a) { e.preventDefault(); location.href = a.href; }
  }, true);
  document.addEventListener('auxclick', function(e){
    if (e.button !== 1) return;
    var a = toBlank(e.target);
    if (a) { e.preventDefault(); location.href = a.href; }
  }, true);
  document.addEventListener('submit', function(e){
    var f = e.target;
    if (!f || !f.tagName || f.tagName !== 'FORM') return;
    var t = (f.target || '').toLowerCase();
    if (t === '' || t === '_self' || t === '_parent' || t === '_top') return;
    e.preventDefault();
    var old = f.target; f.target = '_self';
    if (f.requestSubmit) { f.requestSubmit(f.submitter || undefined); } else { f.submit(); }
    f.target = old;
  }, true);
  try {
    window.open = function(u){ if (u) location.href = String(u); return null; };
  } catch (_) {}
})();`

// ---------- CDP 通道 ----------

// callCDP 在面板控件上调用 CDP 方法并等待结果（官方 DevTools 协议入口）。
func (p *browserPanel) callCDP(method string, params map[string]any) (json.RawMessage, error) {
	paramsJSON := "{}"
	if params != nil {
		if b, err := json.Marshal(params); err == nil {
			paramsJSON = string(b)
		}
	}
	p.stateMu.Lock()
	wv := p.wv
	p.stateMu.Unlock()
	if wv == nil {
		return nil, fmt.Errorf("浏览器面板尚未就绪：先 browser_open")
	}
	m16, err1 := windows.UTF16PtrFromString(method)
	pj16, err2 := windows.UTF16PtrFromString(paramsJSON)
	if err1 != nil || err2 != nil {
		return nil, fmt.Errorf("CDP 参数编码失败")
	}
	res := make(chan cdpResult, 1)
	h := &cdpCompletedHandler{vtbl: &cdpCompletedVtblInst, ch: res}
	// 引用保活（自制 COM 回调对象通例：COM 侧只存裸指针，Go 侧必须持有）：
	// dispatch 闭包执行完即失引用，若 evaluate 跑数秒（重渲染+同步 XHR），期间
	// GC 会把 h 当垃圾回收——原生完成回调踩已释放内存，整进程无声闪退、无堆栈
	// 无事件（2026-10-14 19:56:44 与 20:04:27 两次实锤均落于此窗口期）。
	defer runtime.KeepAlive(h)
	p.dispatch(func() {
		defer panelRecover("callCDP 投递")
		_, _, _ = wvVtblOf(wv).callDevToolsProtocolMethod.Call(
			uintptr(unsafe.Pointer(wv)), uintptr(unsafe.Pointer(m16)), uintptr(unsafe.Pointer(pj16)),
			uintptr(unsafe.Pointer(h)))
	})
	select {
	case r := <-res:
		if r.hr != 0 {
			return nil, fmt.Errorf("CDP %s 调用失败: 0x%08x", method, uint32(r.hr))
		}
		return json.RawMessage(r.json), nil
	case <-time.After(browserTimeout):
		// 超时≠回调不会来：对象挂保活表，回调真到时自摘（见 cdpRetired 注释）。
		retireCdpHandler(h)
		return nil, fmt.Errorf("CDP %s 调用超时", method)
	}
}

func (p *browserPanel) Evaluate(js string) (any, error) {
	raw, err := p.callCDP("Runtime.evaluate", map[string]any{"expression": js, "returnByValue": true})
	if err != nil {
		return nil, err
	}
	// returnObjectAsJson 即 CDP 命令结果本体：Runtime.evaluate →
	// {"result":{"type":"string","value":…},"exceptionDetails":{…}?}
	var out struct {
		Result struct {
			Type  string          `json:"type"`
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text        string `json:"text"`
			Description string `json:"description"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("evaluate 结果解析失败: %w", err)
	}
	if e := out.ExceptionDetails; e != nil {
		msg := e.Description
		if msg == "" {
			msg = e.Text
		}
		return nil, fmt.Errorf("页面脚本异常: %s", msg)
	}
	var v any
	if len(out.Result.Value) > 0 {
		_ = json.Unmarshal(out.Result.Value, &v)
	}
	return v, nil
}

// ---------- browserTarget 操作面 ----------

func (p *browserPanel) Content() (string, string, string, error) {
	v, err := p.Evaluate(`document.body ? document.body.innerText : ""`)
	if err != nil {
		return "", "", "", err
	}
	body, _ := v.(string)
	u, t := p.uiReadNav()
	p.stateMu.Lock()
	p.url, p.title = u, t
	p.stateMu.Unlock()
	return u, t, body, nil
}

// evalPoint 取选择器中心坐标（元素不存在或不可见时报错）。
func (p *browserPanel) evalPoint(selector string) (float64, float64, error) {
	sel, _ := json.Marshal(selector)
	js := `(function(){var el=document.querySelector(` + string(sel) + `);if(!el)return null;` +
		`var r=el.getBoundingClientRect();if(r.width<1||r.height<1)return null;` +
		`return [r.x+r.width/2, r.y+r.height/2];})()`
	v, err := p.Evaluate(js)
	if err != nil {
		return 0, 0, err
	}
	arr, ok := v.([]any)
	if !ok || len(arr) != 2 {
		return 0, 0, fmt.Errorf("元素不存在或不可见: %s", selector)
	}
	x, ok1 := arr[0].(float64)
	y, ok2 := arr[1].(float64)
	if !ok1 || !ok2 {
		return 0, 0, fmt.Errorf("元素位置异常: %s", selector)
	}
	return x, y, nil
}

func (p *browserPanel) waitVisible(selector string, d time.Duration) error {
	deadline := time.Now().Add(d)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, _, err := p.evalPoint(selector); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(250 * time.Millisecond)
	}
	if _, _, err := p.evalPoint(selector); err == nil {
		return nil
	}
	if lastErr != nil && strings.Contains(lastErr.Error(), "元素不存在") {
		return fmt.Errorf("元素不存在或不可见: %s", selector)
	}
	return fmt.Errorf("等待元素超时: %s", selector)
}

func (p *browserPanel) mouseAt(t string, x, y float64, extra map[string]any) error {
	params := map[string]any{"type": t, "x": x, "y": y}
	for k, v := range extra {
		params[k] = v
	}
	_, err := p.callCDP("Input.dispatchMouseEvent", params)
	return err
}

func (p *browserPanel) Click(selector string) error {
	if err := p.waitVisible(selector, 10*time.Second); err != nil {
		return err
	}
	x, y, err := p.evalPoint(selector)
	if err != nil {
		return err
	}
	p.showPointerAt(x, y)
	if err := p.mouseAt("mousePressed", x, y, map[string]any{"button": "left", "clickCount": 1}); err != nil {
		return err
	}
	if err := p.mouseAt("mouseReleased", x, y, map[string]any{"button": "left", "clickCount": 1}); err != nil {
		return err
	}
	time.Sleep(800 * time.Millisecond)
	return nil
}

func (p *browserPanel) TypeInto(selector, text string, clear bool) error {
	if err := p.waitVisible(selector, 10*time.Second); err != nil {
		return err
	}
	x, y, err := p.evalPoint(selector)
	if err != nil {
		return err
	}
	p.showPointerAt(x, y)
	// 点击聚焦：走真实鼠标事件路径，focus/blur 行为与用户操作一致
	if err := p.mouseAt("mousePressed", x, y, map[string]any{"button": "left", "clickCount": 1}); err != nil {
		return err
	}
	if err := p.mouseAt("mouseReleased", x, y, map[string]any{"button": "left", "clickCount": 1}); err != nil {
		return err
	}
	if clear {
		sel, _ := json.Marshal(selector)
		js := `(function(){var el=document.querySelector(` + string(sel) + `);if(!el)return;` +
			`if(el.value!==undefined){el.value='';el.dispatchEvent(new Event('input',{bubbles:true}));}` +
			`else{el.textContent='';}})()`
		if _, err := p.Evaluate(js); err != nil {
			return fmt.Errorf("清空输入框失败: %w", err)
		}
	}
	if text == "" {
		return nil
	}
	// 整段注入（Input.insertText）：中文/输入法的正确路径
	if _, err := p.callCDP("Input.insertText", map[string]any{"text": text}); err != nil {
		return fmt.Errorf("输入失败: %w", err)
	}
	return nil
}

func (p *browserPanel) Scroll(direction string, amount int) error {
	if amount <= 0 {
		amount = 5
	}
	dy := float64(amount) * 100
	if strings.EqualFold(strings.TrimSpace(direction), "up") {
		dy = -dy
	}
	p.stateMu.Lock()
	x := float64(p.cssW) / 2
	y := float64(p.cssH) / 2
	p.stateMu.Unlock()
	_, err := p.callCDP("Input.dispatchMouseEvent",
		map[string]any{"type": "mouseWheel", "x": x, "y": y, "deltaX": 0, "deltaY": dy})
	return err
}

func (p *browserPanel) Screenshot() ([]byte, error) {
	raw, err := p.callCDP("Page.captureScreenshot", map[string]any{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("截图结果解析失败: %w", err)
	}
	buf, err := base64.StdEncoding.DecodeString(out.Data)
	if err != nil || len(buf) == 0 {
		return nil, fmt.Errorf("截图数据无效")
	}
	return buf, nil
}

// CaptureFrame 远程帧镜像截取（WebP q60，handleBrowserFrame 的截取口）：
// 与 Screenshot 同一 CDP 方法，只换压缩格式——WebP 比 PNG 小一个量级，
// 是远程端 2fps 轮询下的带宽主力。
func (p *browserPanel) CaptureFrame() ([]byte, error) {
	raw, err := p.callCDP("Page.captureScreenshot", map[string]any{"format": "webp", "quality": 60})
	if err != nil {
		return nil, err
	}
	var out struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("截帧结果解析失败: %w", err)
	}
	buf, err := base64.StdEncoding.DecodeString(out.Data)
	if err != nil || len(buf) == 0 {
		return nil, fmt.Errorf("截帧数据无效")
	}
	return buf, nil
}

// SetViewport 桌面面板语义：视口=面板实际尺寸。宽度经 browser_resize 事件请
// 前端让位（--content-w 调整），高度跟随右栏；矩形回流后 State 反映实际值。
func (p *browserPanel) SetViewport(w, h int) error {
	if w == 0 || h == 0 {
		w, h = browserDefW, browserDefH
	}
	w = clampInt(w, browserMinSide, browserMaxW)
	h = clampInt(h, browserMinSide, browserMaxH)
	p.app.broadcast(Event{Type: "browser_resize", X: float64(w), Y: float64(h), SessionID: p.sid})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		p.stateMu.Lock()
		cw := p.cssW
		p.stateMu.Unlock()
		if cw >= w-4 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

func (p *browserPanel) State() BrowserState {
	p.stateMu.Lock()
	st := BrowserState{Running: p.started && !p.closing, URL: p.url, Title: p.title, VW: p.cssW, VH: p.cssH}
	p.stateMu.Unlock()
	if !st.Running {
		return st
	}
	if u, t := p.uiReadNav(); u != "" {
		st.URL, st.Title = u, t
	}
	return st
}

// Started 面板是否「在运行」：关闭一开始就为 false——工具层据此报「浏览器未
// 在运行」，下次 browser_open 会重建实例。
func (p *browserPanel) Started() bool {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	return p.started && !p.closing
}

func (p *browserPanel) Where() string {
	return "右栏「浏览器」面板（原生 Edge，用户可直接操作）"
}

// ---------- 关闭（真关：发退出指令 → 等浏览器进程退出 → 拆窗口） ----------
//
// 用户对「关闭浏览器」的硬要求：必须真关（进程真退出、窗口真拆掉）；不许
// 「几分钟后后台回收」这类延迟机制（出了 bug 感知不到，莫名挂十几分钟最难
// 排查），也不许「软关闭」（清页面收屏外、留实例）当最终方案。所以只有下面
// 这一条序列，每阶段各有一条落盘日志（desktop.log 里搜 [browser-panel] 关闭）：
//
//	阶段一（UI 线程）：反注册自有事件（remove_NewWindowRequested /
//	  remove_DocumentTitleChanged，拿注册时的 token）→ 立刻隐藏宿主子窗口
//	  （画面马上消失，窗口本身等进程退出后才拆）→ 读浏览器进程 PID 并在发
//	  退出指令之前打开它的进程句柄 → Controller.Close（给 WebView2 与它对应的
//	  浏览器进程发退出指令）。
//	阶段二（后台 goroutine）：等那个浏览器进程真的退出（进程句柄
//	  WaitForSingleObject，拿到确定的退出信号，不拿毫秒延时猜）；超过上限还
//	  没退就强制终止我们自己的这个子进程，并继续等到确实退出——没有「放弃拆、
//	  留空壳」的分支（finishPanelClose，browser_host.go）。
//	阶段三（投递回 UI 线程）：DestroyWindow 拆宿主子窗口。此时进程已确认退出，
//	  不可能再有回调打到将亡资源上。
//
// 等进程全程在后台，UI 线程只执行投递进来的 COM/Win32 动作，界面不卡。
// 拆解前状态已清空（closing 置位、控件引用置零、地址标题清零、导航等待者
// 放行）：迟到的导航/标题/弹窗/CDP 回调在各自入口就被 closing 挡掉。

// Close 关闭面板实例（右栏 ✕ / browser_close 的收口都调它）。
func (p *browserPanel) Close() { p.closePanel("未标注发起方") }

// CloseWithReason 同 Close，额外把发起方写进各阶段日志（右栏 ✕ 与 AI 的
// browser_close 走同一个收口，差别只在日志与收口方的标记）。其余形态
// （有头/无头）没有关闭阶段日志，收口按 browserTarget.Close 调。
func (p *browserPanel) CloseWithReason(reason string) { p.closePanel(reason) }

// closePanel 发起关闭，返回「浏览器进程已退出且宿主子窗口已拆」的完成信号。
// 普通调用方不等（Close 就这样用：界面与 HTTP 都不卡）；需要「确实关干净」
// 的调用方（ApplyGrants 写 profile 前）等这个 channel。
func (p *browserPanel) closePanel(reason string) <-chan struct{} {
	done := make(chan struct{})
	p.createMu.Lock()
	parts := p.detach(done) // 同一个锁区里就把「正在关」登记给并发方（见 waitCloseSettled）
	p.createErr = nil       // 关掉即允许下次重建（创建失败留下的旧错误不再挡住 browser_open）
	p.createMu.Unlock()
	if !parts.live {
		close(done) // 没开过或正在关：幂等（窗口只拆一次）
		return done
	}
	log.Printf("[browser-panel] 关闭: 发起（原因=%s sid=%q hwnd=%#x 控件=%v）", reason, p.sid, parts.hwnd, parts.ctl != nil)
	p.app.broadcast(Event{Type: "browser_nav", URL: "", Title: "", SessionID: p.sid}) // 工具栏地址/标题复位
	go func() {
		defer close(done)
		defer func() { // 拆完就保活：原生侧还握着它的 handler 裸指针
			retirePanelChromium(parts.chrome)
			retireEvtHandler(parts.title)
		}()
		defer func() {                          // 本次关闭收尾：清掉「关闭进行中」标记（等待重建的一方据此放行）
			p.stateMu.Lock()
			if p.closingDone == done {
				p.closingDone = nil
			}
			p.stateMu.Unlock()
		}()
		var ex panelExiter
		if parts.ctl != nil && parts.dispatch != nil {
			stage := make(chan panelExiter, 1)
			parts.dispatch(func() { stage <- p.closeControllerOnUI(parts) })
			select {
			case ex = <-stage:
			case <-time.After(5 * time.Second):
				log.Printf("[browser-panel] 关闭: 阶段一 5 秒未在 UI 线程执行（sid=%q，消息泵可能已停），直接转入等退出/拆窗口", p.sid)
			}
		} else {
			log.Printf("[browser-panel] 关闭: 没有控件可关（sid=%q 控件=%v UI投递=%v），无需发退出指令",
				p.sid, parts.ctl != nil, parts.dispatch != nil)
		}
		var destroy func()
		if parts.hwnd != 0 {
			destroy = func() { _, _, _ = procPanelDestroyWindow.Call(parts.hwnd) }
		}
		finishPanelClose(ex, parts.dispatch, p.sid, fmt.Sprintf("hwnd=%#x", parts.hwnd), destroy, panelCloseWait)
	}()
	return done
}

// panelParts 从面板里取走的现场（关闭与前端收尾共用）。
type panelParts struct {
	hwnd                uintptr
	ctl                 *edge.ICoreWebView2Controller
	wv                  *edge.ICoreWebView2
	chrome              *edge.Chromium // 绑定库实例：拆完后要保活（见 retirePanelChromium）
	title    *evtHandler
	titleTok int64
	dispatch            func(f func())
	live                bool // 取走前是否在运行：false=没开过/已在关，调用方据此幂等返回
}

// 已退休 WebView2 实例的 Go 侧保活表。
//
// 为什么必须保活（2026-10-22 19:57 无声退出的加固项）：go-webview2 把
// environment/controller 创建完成回调、WebMessageReceived、PermissionRequested、
// WebResourceRequested、AcceleratorKeyPressed、NavigationCompleted 这一批
// handler 对象以裸指针（uintptr）交给原生侧，**从不反注册**（库自己没提供
// remove）。Go 的 GC 只看 Go 侧的引用，原生侧那串裸指针不算引用——面板一关，
// Chromium 实例与它名下这批 handler 就变成不可达垃圾，一旦被回收，原生侧的
// 后续回调就踩进已释放的 Go 内存（windowsgui 下等于整进程无声死亡，无日志无
// WER）。保活这几个 Go 对象是零行为代价的保险：它们对应的 COM 对象本来也从
// 不 Release（本就没有 Release 路径），留着不但不多占 COM 引用，反而消除了
// 「原生指针指向已回收内存」这一整类事故。
var (
	retiredMu        sync.Mutex
	retiredChromiums []*edge.Chromium
	retiredHandlers  []*evtHandler
)

// retirePanelChromium 把拆完的绑定库实例登记进保活表（同一个实例只记一次）。
func retirePanelChromium(ch *edge.Chromium) {
	if ch == nil {
		return
	}
	retiredMu.Lock()
	for _, c := range retiredChromiums {
		if c == ch {
			retiredMu.Unlock()
			return
		}
	}
	retiredChromiums = append(retiredChromiums, ch)
	n := len(retiredChromiums)
	retiredMu.Unlock()
	log.Printf("[browser-panel] 已退休实例保活（累计 %d 个）：原生侧仍持有它的回调对象裸指针，Go 侧不许让它变成垃圾", n)
}

// retiredChromiumCount 保活表规模（单测断言「同一实例只记一次」用）。
func retiredChromiumCount() int {
	retiredMu.Lock()
	defer retiredMu.Unlock()
	return len(retiredChromiums)
}

// retireEvtHandler 自制事件回调对象的同款保活：remove_* 在正常关闭序列里
// 反注册过，但「拆解途中迟到事件」「remove 未生效/竞态」这类残余可能性没法
// 从外部证伪——对象本身只有几十字节，保活的代价比再赌一次静默闪退低。
func retireEvtHandler(h *evtHandler) {
	if h == nil {
		return
	}
	retiredMu.Lock()
	for _, x := range retiredHandlers {
		if x == h {
			retiredMu.Unlock()
			return
		}
	}
	retiredHandlers = append(retiredHandlers, h)
	retiredMu.Unlock()
}

// detach 取走控件与宿主子窗口的引用并清空状态（调用方须持 createMu）。
// closing 一置位，迟到回调就在各自入口被挡掉，碰不到即将销毁的控件——这是
// 关闭流程「不崩」的关键前置（2026-10-22 16:13 的无声退出正是拆解途中被
// 迟到回调踩到已释放控件）。真正在关时把 done 登记为「关闭进行中」，谁要在
// 同一个 profile 上重建（ensureStarted）或写它（ApplyGrants）都得先等这个信号；
// 前端收尾（releaseAfterMainLoopGone）没有关闭驱动，传 nil 即可。
func (p *browserPanel) detach(done chan struct{}) panelParts {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	parts := panelParts{
		hwnd: p.hwnd, ctl: p.ctl, wv: p.wv, chrome: p.chrome,
		title: p.titleHandler,
		titleTok: p.titleToken,
		dispatch: p.dispatch,
		// 「有东西可取」就算在运行：不只看 started——创建链路超时返回后控件仍可能
		// 稍晚落进字段（started 永远没置上），那种实例同样要把控件关掉、窗口拆掉；
		// 只按 started 判的话，取走现场却不做任何收尾，等于把一个活着的 WebView2
		// 环境连窗口一起丢在地上，下次重建就在同一 profile 上叠出第二套环境。
		live: !p.closing && (p.started || p.ctl != nil || p.hwnd != 0 || p.chrome != nil),
	}
	p.closing = true
	if parts.live && done != nil {
		p.closingDone = done
	}
	// 已在关的那次不动它的登记：并发的 Close / 前端收尾不该把等待者放走
	p.hwnd, p.ctl, p.wv, p.chrome = 0, nil, nil, nil
	p.titleHandler = nil
	p.titleToken = 0
	p.started, p.visible = false, false
	p.url, p.title = "", ""
	p.cssW, p.cssH = 0, 0
	if p.navCh != nil {
		close(p.navCh) // 旧等待者放行（读到中间态）
		p.navCh = nil
	}
	return parts
}

// waitCloseSettled 等上一轮关闭收尾（浏览器进程退出 + 拆窗口）。同一个 profile
// 目录上重建控件必须等它关完：否则新旧两个 WebView2 环境会同时占用同一 user
// data folder，还可能撞上正在退出的浏览器进程。
func (p *browserPanel) waitCloseSettled() {
	p.stateMu.Lock()
	done := p.closingDone
	p.stateMu.Unlock()
	if done == nil {
		return
	}
	max := panelCloseWait.exit + panelCloseWait.kill + 2*time.Second
	select {
	case <-done:
	case <-time.After(max):
		log.Printf("[browser-panel] 创建: 上一轮关闭 %s 仍未完成（sid=%q），照常创建（可能撞上正在退出的旧实例）", max, p.sid)
	}
}

// closeControllerOnUI 阶段一（仅 UI 线程）：反注册自有事件 → 先隐藏宿主子窗口
// （画面立即消失，窗口等进程退出后再拆）→ 读浏览器进程 PID 并在发退出指令前
// 打开它的进程句柄 → Controller.Close 发退出指令。返回进程句柄给阶段二等退出；
// 取不到 PID/句柄时返回 nil（阶段二按「没有进程可等」处理并照常拆窗口）。
func (p *browserPanel) closeControllerOnUI(parts panelParts) (ex panelExiter) {
	defer panelRecover("面板关闭 阶段一")
	// 逐步打点：阶段一全是原生调用，整段静默死亡时（windowsgui 下无 panic 日志、
	// 无 WER）唯一能留下的线索就是「最后一条打点」。2026-10-22 19:57 那次死在这段
	// 里面，只留了「关闭: 发起」，连是哪一个调用都判不出来——所以每步都落一行。
	log.Printf("[browser-panel] 关闭: 阶段一 开始（sid=%q 先反注册自有事件）", p.sid)
	v := (*wvRawVtbl)(nil)
	if parts.wv != nil {
		v = wvVtblOf(parts.wv)
	}
	if v != nil {
		if parts.title != nil {
			tok := parts.titleTok
			_, _, _ = v.removeDocumentTitleChanged.Call(
				uintptr(unsafe.Pointer(parts.wv)), uintptr(unsafe.Pointer(parts.title)), uintptr(unsafe.Pointer(&tok)))
		}
	}
	log.Printf("[browser-panel] 关闭: 阶段一 已反注册自有事件（sid=%q）", p.sid)
	if parts.hwnd != 0 {
		_, _, _ = procPanelShowWindow.Call(parts.hwnd, swHide) // 画面马上消失；窗口等进程退出后拆
	}
	log.Printf("[browser-panel] 关闭: 阶段一 已隐藏宿主子窗口（sid=%q hwnd=%#x）", p.sid, parts.hwnd)
	var pid uint32
	if v != nil {
		_, _, _ = v.getBrowserProcessID.Call(uintptr(unsafe.Pointer(parts.wv)), uintptr(unsafe.Pointer(&pid)))
	}
	log.Printf("[browser-panel] 关闭: 阶段一 已取浏览器进程 PID（sid=%q pid=%d）", p.sid, pid)
	ex = openPanelProcess(pid) // 关前抓句柄：关后 PID 可能被系统复用
	if parts.ctl != nil {
		_, _, _ = ctlVtblOf(parts.ctl).close.Call(uintptr(unsafe.Pointer(parts.ctl)))
		log.Printf("[browser-panel] 关闭: 已发退出指令（sid=%q Controller.Close 浏览器进程 pid=%d）", p.sid, pid)
	}
	return ex
}

// panelProcess 真实浏览器进程句柄（panelExiter 的 Windows 实现）：句柄在发
// 退出指令之前打开，之后按句柄等/杀——不按 PID 重开，避开 PID 被系统复用的
// 风险（拿复用的 PID 还能误杀别人的进程）。
type panelProcess struct {
	pid uint32
	h   windows.Handle
}

func openPanelProcess(pid uint32) panelExiter {
	if pid == 0 {
		log.Printf("[browser-panel] 关闭: 取不到浏览器进程 PID，跳过等退出（按已退出处理）")
		return nil
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		log.Printf("[browser-panel] 关闭: 打开浏览器进程句柄失败（pid=%d）: %v；按已退出处理", pid, err)
		return nil
	}
	return &panelProcess{pid: pid, h: h}
}

func (pp *panelProcess) PID() uint32 { return pp.pid }

// Wait 等进程退出：WaitForSingleObject 返回的是内核的退出事件（真实信号），
// 不是轮询也不是固定延时。
func (pp *panelProcess) Wait(max time.Duration) bool {
	if pp.h == 0 {
		return true
	}
	ev, err := windows.WaitForSingleObject(pp.h, uint32(max/time.Millisecond))
	return err == nil && ev == windows.WAIT_OBJECT_0
}

func (pp *panelProcess) Kill() error {
	if pp.h == 0 {
		return fmt.Errorf("进程句柄不可用")
	}
	return windows.TerminateProcess(pp.h, 1)
}

func (pp *panelProcess) Release() {
	if pp.h != 0 {
		windows.CloseHandle(pp.h)
		pp.h = 0
	}
}

// releaseAfterMainLoopGone 主窗口消息循环已退出后的兜底释放（前端热重启用，
// 仅 UI 线程调用）：不走 Dispatch（旧泵已停），直接 Close 控制器、拆子窗口，
// 让面板这棵浏览器进程树随旧前端一起退场。状态清理与关闭共用 detach（closing
// 置位后迟到事件一律跳过），下次 SetRect/Navigate 会经新窗口的 Dispatch 重建。
func (p *browserPanel) releaseAfterMainLoopGone() {
	p.createMu.Lock()
	defer p.createMu.Unlock()
	parts := p.detach(nil) // 前端要换新窗口：不再等这次关闭（旧泵已停，投递到不了）
	p.createErr = nil      // 允许在新前端上重建
	if parts.ctl != nil {
		_, _, _ = ctlVtblOf(parts.ctl).close.Call(uintptr(unsafe.Pointer(parts.ctl)))
	}
	if parts.hwnd != 0 {
		_, _, _ = procPanelDestroyWindow.Call(parts.hwnd) // 已随父窗口销毁时是无害重复
	}
	retirePanelChromium(parts.chrome) // 同上：原生侧的 handler 裸指针还在，Go 侧不许回收
	retireEvtHandler(parts.title)
}

// BroadcastPointer 点击/输入前亮出 AI 位置：在目标页面内注入临时标记元素
// （原生面板上 HTML 无法覆盖，只能进页面内部画）。
func (p *browserPanel) BroadcastPointer(selector string) {
	x, y, err := p.evalPoint(selector)
	if err != nil {
		return
	}
	p.showPointerAt(x, y)
}

func (p *browserPanel) showPointerAt(x, y float64) {
	js := fmt.Sprintf(`(function(x,y){try{var m=document.getElementById('__jx_ai_marker');`+
		`if(!m){m=document.createElement('div');m.id='__jx_ai_marker';`+
		`m.style.cssText='position:fixed;left:0;top:0;z-index:2147483647;pointer-events:none;`+
		`background:#3b82f6;color:#fff;font:600 11px/1 system-ui;padding:3px 6px;border-radius:4px;`+
		`transform:translate(-50%%,-130%%);box-shadow:0 2px 6px rgba(0,0,0,.35);opacity:0;transition:opacity .2s';`+
		`(document.body||document.documentElement).appendChild(m);}`+
		`m.textContent='AI';m.style.left=x+'px';m.style.top=y+'px';m.style.opacity='1';`+
		`clearTimeout(m.__t);m.__t=setTimeout(function(){m.style.opacity='0';},1500);}catch(e){}})(%g,%g)`, x, y)
	_, _ = p.Evaluate(js)
}

// uiReadNav 同步读当前地址与标题（UI 线程往返，短超时兜底缓存值）。
func (p *browserPanel) uiReadNav() (string, string) {
	res := make(chan [2]string, 1)
	p.dispatch(func() {
		p.stateMu.Lock()
		wv := p.wv
		p.stateMu.Unlock()
		if wv == nil {
			res <- [2]string{"", ""}
			return
		}
		res <- [2]string{wvGetSource(wv), wvGetDocumentTitle(wv)}
	})
	select {
	case r := <-res:
		return r[0], r[1]
	case <-time.After(5 * time.Second):
		p.stateMu.Lock()
		defer p.stateMu.Unlock()
		return p.url, p.title
	}
}
