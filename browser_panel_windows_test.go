//go:build windows

package main

// 右栏原生面板「关闭 → 重建 → 再关闭」状态机的单测：不建任何 WebView2 控件、
// 不发一个 COM 调用（ctl/wv 一律留 nil，阶段一因此整段跳过），只验证 Go 侧
// 状态机的四条纪律——
//  1. 关闭取现场（detach）之后字段必须全清：closing 置位、控件/窗口/绑定库
//     实例引用归零、navCh 放行且置 nil（否则第二次 detach 会重复 close 同
//     一个 channel 直接 panic）；
//  2. 「有东西可取」就算在运行（2026-10-22 加固项）：started 没置上但控件/
//     窗口已在字段里的实例，也必须走完整收尾，不能被当成「没开过」而把活的
//     控件与窗口丢在地上；
//  3. 关闭收尾后 closingDone 必须清空——它是「重建前先等上一轮关完」的唯一
//     信号，残留一次就会让下次 ensureStarted 白等 17 秒（历史上就是这样把
//     browser_open 拖成超时的）；
//  4. 拆完的绑定库实例必须保活（retirePanelChromium）：原生侧仍握着它的回调
//     对象裸指针，Go 侧一回收就是整进程无声死亡。

import (
	"testing"
	"time"
	"unsafe"

	edge "github.com/jchv/go-webview2/pkg/edge"
)

// newFakePanel 造一个面板对象（不建控件）：dispatch 用同步执行，免得测试
// 依赖桌面壳的 UI 消息泵。
func newFakePanel(t *testing.T, a *App) *browserPanel {
	t.Helper()
	p := newBrowserPanel(a, func(f func()) { f() }, 0, "2cf9006240c072a709").(*browserPanel)
	return p
}

// armPanel 把面板置成「一次成功的创建之后」的字段形态（等价 createOnUI 的
// 写入 + ensureStarted 的 started=true），供关闭/重建状态机测试使用。
func armPanel(p *browserPanel) {
	p.stateMu.Lock()
	p.started, p.closing = true, false
	p.hwnd = 0 // 不给真窗口句柄：阶段三拆窗口会跳过（测试不碰 Win32）
	p.ctl, p.wv = nil, nil
	p.chrome = edge.NewChromium() // 纯 Go 结构，不触发原生调用
	p.titleToken = 22
	p.url, p.title = "https://example.com/", "Example Domain"
	p.stateMu.Unlock()
}

// waitDone 等关闭完成信号（正常应在毫秒级内完成）。
func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("关闭在 5 秒内没有收尾（等待进程退出/拆窗口的那一段卡住了）")
	}
}

// ① detach 取走现场后必须全清；重复 detach 幂等且不重复关 navCh。
func TestPanelDetachClearsStateAndIsIdempotent(t *testing.T) {
	a, _ := newDispatchTestApp(t)
	p := newFakePanel(t, a)
	armPanel(p)
	p.stateMu.Lock()
	p.navCh = make(chan struct{})
	navCh := p.navCh
	p.stateMu.Unlock()

	parts := p.detach(nil)
	if !parts.live {
		t.Fatal("在运行的面板被 detach 却报 live=false")
	}
	if parts.chrome == nil {
		t.Fatal("绑定库实例没被取走（拆完就没人保活它了：原生侧还握着它的回调裸指针）")
	}
	if parts.titleTok != 22 {
		t.Fatalf("事件 token 没被取走（反注册要用）：%d", parts.titleTok)
	}
	select {
	case <-navCh:
	default:
		t.Fatal("导航等待者没被放行（导航会白等到超时）")
	}
	p.stateMu.Lock()
	leaked := p.ctl != nil || p.wv != nil || p.hwnd != 0 || p.chrome != nil ||
		p.navCh != nil || p.started || !p.closing
	p.stateMu.Unlock()
	if leaked {
		t.Fatal("detach 后仍留着旧值（重建会沿用已释放的控件/句柄）")
	}
	if p.Started() {
		t.Fatal("detach 后 Started() 仍为 true（工具层会以为浏览器还在跑）")
	}

	again := p.detach(nil) // 重复取现场：幂等，且不能第二次 close(navCh) 炸掉
	if again.live || again.ctl != nil || again.hwnd != 0 {
		t.Fatalf("重复 detach 不幂等：live=%v ctl=%v hwnd=%#x", again.live, again.ctl, again.hwnd)
	}
}

// ② 「有东西可取」就算在运行：started 没置上但控件/窗口已在字段里，也要收尾。
func TestPanelDetachTreatsLeftoversAsLive(t *testing.T) {
	a, _ := newDispatchTestApp(t)
	p := newFakePanel(t, a)

	// 创建链路超时返回后控件稍晚落进字段的形态：started 永远没置上
	p.stateMu.Lock()
	p.started, p.closing = false, false
	p.hwnd = 0x1 // 无效句柄：真去 DestroyWindow 也只会返回 FALSE
	p.chrome = edge.NewChromium()
	p.stateMu.Unlock()
	if parts := p.detach(nil); !parts.live {
		t.Fatal("控件/窗口已在字段里却报 live=false：这份现场会被取走却不做任何收尾（活着的环境被丢在地上）")
	}

	// 真的什么都没有：幂等成功
	p2 := newFakePanel(t, a)
	if parts := p2.detach(nil); parts.live {
		t.Fatal("没开过的面板被报成在运行")
	}
}

// ③ 关闭 → 重建 → 再关闭：两次都要走完整收尾，且收尾后 closingDone 必须清空。
func TestPanelCloseRebuildCloseClearsCloseState(t *testing.T) {
	a, _ := newDispatchTestApp(t)
	p := newFakePanel(t, a)
	armPanel(p)
	before := retiredChromiumCount()

	waitDone(t, p.closePanel("第一次关闭（用户 发起）"))
	p.stateMu.Lock()
	stale := p.closingDone
	chrome1 := p.chrome
	p.stateMu.Unlock()
	if stale != nil {
		t.Fatal("第一次关闭收尾后 closingDone 没清（下次重建会白等 17 秒）")
	}
	if chrome1 != nil {
		t.Fatal("关闭后仍挂着绑定库实例引用")
	}
	if got := retiredChromiumCount(); got != before+1 {
		t.Fatalf("拆完的实例没进保活表：%d → %d", before, got)
	}

	// 重建（等价 createOnUI + ensureStarted 的字段写入）：新实例、新 token
	armPanel(p)
	waitDone(t, p.closePanel("第二次关闭（AI 发起）"))
	p.stateMu.Lock()
	stale = p.closingDone
	p.stateMu.Unlock()
	if stale != nil {
		t.Fatal("第二次关闭收尾后 closingDone 没清（连关两次后重建会卡住）")
	}
	if got := retiredChromiumCount(); got != before+2 {
		t.Fatalf("第二次关闭的实例没进保活表：%d", got)
	}

	// 已经关干净再关一次：幂等，不再触发一轮收尾、也不重复保活
	waitDone(t, p.closePanel("第三次关闭（空转）"))
	if got := retiredChromiumCount(); got != before+2 {
		t.Fatalf("空转的关闭又保活了一次（表长 %d）", got)
	}
}

// ④ 从没开过的面板：closePanel 立即返回、不收任何现场、不保活。
func TestPanelCloseNeverStartedIsIdempotent(t *testing.T) {
	a, _ := newDispatchTestApp(t)
	p := newFakePanel(t, a)
	before := retiredChromiumCount()
	done := p.closePanel("没开过")
	waitDone(t, done)
	if p.Started() {
		t.Fatal("没开过的面板 Started() 竟是 true")
	}
	if got := retiredChromiumCount(); got != before {
		t.Fatalf("没开过却保活了一个实例（表长 %d）", got)
	}
}

// ⑤ 保活表对同一实例只记一次（重复调用不得把表撑大）。
func TestRetirePanelChromiumDedup(t *testing.T) {
	ch := edge.NewChromium()
	before := retiredChromiumCount()
	retirePanelChromium(ch)
	retirePanelChromium(ch)
	retirePanelChromium(nil)
	if got := retiredChromiumCount(); got != before+1 {
		t.Fatalf("同一实例被重复记了（%d → %d）", before, got)
	}
}

// ⑥ CDP 完成回调的保活表：超时挂入 → 回调自摘；同一对象重复挂不得撑表。
func TestCdpRetireHandlerLifecycle(t *testing.T) {
	h := &cdpCompletedHandler{vtbl: &cdpCompletedVtblInst, ch: make(chan cdpResult, 1)}
	retireCdpHandler(h)
	retireCdpHandler(h)
	cdpRetiredMu.Lock()
	n := len(cdpRetired)
	cdpRetiredMu.Unlock()
	if n != 1 {
		t.Fatalf("重复挂入撑大了保活表（len=%d）", n)
	}
	unretireCdpHandler(h)
	unretireCdpHandler(h) // 重复自摘也幂等
	cdpRetiredMu.Lock()
	n = len(cdpRetired)
	cdpRetiredMu.Unlock()
	if n != 0 {
		t.Fatalf("自摘后表未清空（len=%d）", n)
	}
}

// ⑦ 自制事件回调对象的保活：nil 不记、同一对象只记一次。
func TestRetireEvtHandlerDedup(t *testing.T) {
	h := newEvtHandler(func(sender, args unsafe.Pointer) {})
	retireEvtHandler(h)
	retireEvtHandler(h)
	retireEvtHandler(nil)
	retiredMu.Lock()
	n := len(retiredHandlers)
	retiredMu.Unlock()
	if n != 1 {
		t.Fatalf("事件回调保活表长度异常（len=%d，应为 1）", n)
	}
}
