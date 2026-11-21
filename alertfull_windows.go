//go:build windows

package main

// 久等提醒（ask_alert）的「桌面全屏蒙层」半边：提醒到点时，除了主窗口里的
// 提醒层（app-chat.js 的 #ask-alert）与「把窗口拉回眼前」（alertfront_windows.go），
// 再盖一层**全屏置顶半透明蒙层**——主窗口在别的虚拟桌面/被一堆窗口埋住时，
// 用户也一定看得见。
//
// 设计口径（与主窗口层同一套触发、同一个设置项；前端两处出、点一处全退）：
//   · 盖住**鼠标所在的那块显示器**（人的眼睛大概率跟着鼠标）；
//   · WS_EX_NOACTIVATE：绝不抢焦点——用户正在别处打字时不会被这层打断；
//   · 半透明深色蒙层 + 中央卡片（标题/摘要/提示），不做动画：原生层是「兜底
//     看得见」，渐进变色的精致观感留给主窗口那层；
//   · 点击任意位置＝关闭这层，并回调 onDismiss（fireAskAlert 注入：广播
//     ask_alert_dismiss，前端据此把主窗口的提醒层一起收起）；反过来前端收层
//     时也会 POST /api/alert/screen 把这层关掉——任意一处先关，另一处同退；
//   · 关闭请求带 qid（questionId/approvalId，与前端 showAskAlert 的 askAlertQid
//     同源口径）：上一轮迟到的关闭请求不许把刚弹出的新一轮关掉。

import (
	"log"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

var (
	alertFsUser32 = syscall.NewLazyDLL("user32.dll")
	alertFsGdi32  = syscall.NewLazyDLL("gdi32.dll")

	alertFsGetCursorPos    = alertFsUser32.NewProc("GetCursorPos")
	alertFsMonitorFromPt   = alertFsUser32.NewProc("MonitorFromPoint")
	alertFsGetMonitorInfoW = alertFsUser32.NewProc("GetMonitorInfoW")
	alertFsRegisterClassEx = alertFsUser32.NewProc("RegisterClassExW")
	alertFsCreateWindowEx  = alertFsUser32.NewProc("CreateWindowExW")
	alertFsDefWindowProc   = alertFsUser32.NewProc("DefWindowProcW")
	alertFsShowWindow      = alertFsUser32.NewProc("ShowWindow")
	alertFsSetWindowPos    = alertFsUser32.NewProc("SetWindowPos")
	alertFsSetLayeredAlpha = alertFsUser32.NewProc("SetLayeredWindowAttributes")
	alertFsInvalidateRect  = alertFsUser32.NewProc("InvalidateRect")
	alertFsPostMessageW    = alertFsUser32.NewProc("PostMessageW")
	alertFsGetMessageW     = alertFsUser32.NewProc("GetMessageW")
	alertFsBeginPaint      = alertFsUser32.NewProc("BeginPaint")
	alertFsEndPaint        = alertFsUser32.NewProc("EndPaint")
	alertFsGetClientRectW  = alertFsUser32.NewProc("GetClientRect")
	alertFsFillRect        = alertFsUser32.NewProc("FillRect")
	alertFsDrawTextW       = alertFsUser32.NewProc("DrawTextW")
	alertFsCreateFontW     = alertFsGdi32.NewProc("CreateFontW")
	alertFsSelectObject    = alertFsGdi32.NewProc("SelectObject")
	alertFsDeleteObject    = alertFsGdi32.NewProc("DeleteObject")
	alertFsSetTextColor    = alertFsGdi32.NewProc("SetTextColor")
	alertFsSetBkMode       = alertFsGdi32.NewProc("SetBkMode")
	alertFsCreateSolidBrsh = alertFsGdi32.NewProc("CreateSolidBrush")
	alertFsGetDeviceCaps   = alertFsUser32.NewProc("GetDeviceCaps")
	alertFsGetModuleHandle = alertFsUser32.NewProc("GetModuleHandleW")
	alertFsLoadCursorW     = alertFsUser32.NewProc("LoadCursorW")
)

const (
	alertFsWmShow = 0x8000 + 0x61 // WM_APP+0x61：显示（文案已放进共享状态）
	alertFsWmHide = 0x8000 + 0x62 // WM_APP+0x62：隐藏

	alertFsExLayered    = 0x0008_0000
	alertFsExTopmost    = 0x0000_0008
	alertFsExToolWindow = 0x0000_0080
	alertFsExNoActivate = 0x0800_0000
	alertFsWsPopup      = 0x8000_0000

	alertFsSwHide        = 0
	alertFsSwShowNoAct   = 8
	alertFsSwpNoActivate = 0x0010
	alertFsLwaAlpha      = 0x0002
	alertFsMonitorNearest = 2 // MONITOR_DEFAULTTONEAREST
	alertFsLogPixelsX    = 88
	alertFsIdcArrow      = 32512
	alertFsWmPaint       = 0x000F
	alertFsWmLButtonDown = 0x0201

	alertFsDtCenter      = 0x0001
	alertFsDtVCenter     = 0x0004
	alertFsDtSingleLine  = 0x0020
	alertFsDtWordBreak   = 0x0010
	alertFsDtEndEllipsis = 0x8000
	alertFsDtNoPrefix    = 0x0080
	alertFsBkTransparent = 1

	alertFsAlpha = 140 // 蒙层整体不透明度（/255）：醒目但仍透得出桌面

	// 观感配色与主窗口提醒层同族（主题点缀色 #4d6bfe）。COLORREF 排布 0x00BBGGRR。
	alertFsBrushDim  = 0x00202018 // RGB(24,32,32) 蒙层底
	alertFsBrushCard = 0x002E2E2E // RGB(46,46,46) 中央卡片
	alertFsPenLine   = 0x00FE6B4D // RGB(77,107,254) #4d6bfe 卡片顶线
	alertFsClrText   = 0x00EBE8E6 // RGB(230,232,235) #e6e8eb 正文
	alertFsClrSub    = 0x00FFAFA0 // RGB(160,175,255) 提示行
)

type alertFsPoint struct{ X, Y int32 }

type alertFsRect struct{ L, T, R, B int32 }

type alertFsMonitorInfo struct {
	CbSize    uint32
	RcMonitor alertFsRect
	RcWork    alertFsRect
	SzDevice  [32]uint16
}

type alertFsPaintStruct struct {
	Hdc        uintptr
	FErase     int32
	RcPaint    alertFsRect
	FRestore   int32
	FIncUpdate int32
	Reserved   [32]byte
}

type alertFsWndClass struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
}

type alertFsMsg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      alertFsPoint
}

// 共享状态：任意线程写（show/hide 请求），窗口线程读（绘制/点击回调）。
var (
	alertFsMu      sync.Mutex
	alertFsTitle   string
	alertFsText    string
	alertFsQid     string // 当前这轮提醒的 id（questionId/approvalId），关闭请求要匹配它
	alertFsDismiss func()
	alertFsHwnd    uintptr
	alertFsOnce    sync.Once
	alertFsReady   = make(chan struct{})
)

// alertScreenShow 显示（或更新）全屏提醒蒙层。qid 用于关闭请求防串轮；
// onDismiss 在用户点击这层时被调用（fireAskAlert 注入广播 ask_alert_dismiss）。
func alertScreenShow(title, text, qid string, onDismiss func()) {
	alertFsOnce.Do(func() { go alertFsLoop() })
	select {
	case <-alertFsReady:
	default:
		<-alertFsReady // 首次调用等窗口线程就绪；线程挂了（ready 已关）也直接通过
	}
	alertFsMu.Lock()
	alertFsTitle, alertFsText, alertFsQid = title, text, qid
	if onDismiss != nil {
		alertFsDismiss = onDismiss
	}
	alertFsMu.Unlock()
	if alertFsHwnd != 0 {
		alertFsPostMessage(alertFsHwnd, alertFsWmShow, 0, 0)
	}
}

// alertScreenHideIf 关闭当前蒙层——但只当它显示的正是 qid 这一轮（迟到的旧请求
// 不许把刚弹出的新一轮关掉）。qid 为空表示无条件关（进程收尾等场景）。
func alertScreenHideIf(qid string) {
	alertFsMu.Lock()
	hwnd := alertFsHwnd
	match := qid == "" || qid == alertFsQid
	alertFsMu.Unlock()
	if hwnd != 0 && match {
		alertFsPostMessage(hwnd, alertFsWmHide, 0, 0)
	}
}

func alertFsPostMessage(hwnd uintptr, msg uint32, wp, lp uintptr) {
	alertFsPostMessageW.Call(hwnd, uintptr(msg), wp, lp)
}

// alertFsLoop 专用线程：注册类、建隐藏窗口、跑消息循环（窗口必须留在建它的
// 线程上，LockOSThread 防调度器挪线程——与 voicetype 的钩子线程同一模式）。
func alertFsLoop() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	cls16, _ := syscall.UTF16PtrFromString("JXAskAlertFullScreen")
	cls := alertFsWndClass{
		CbSize:        uint32(unsafe.Sizeof(alertFsWndClass{})),
		LpfnWndProc:   syscall.NewCallback(alertFsWndProc),
		HCursor:       alertFsLoadCursor(0, alertFsIdcArrow),
		LpszClassName: cls16,
	}
	if r, _, err := alertFsRegisterClassEx.Call(uintptr(unsafe.Pointer(&cls))); r == 0 {
		log.Printf("[提醒] 全屏蒙层窗口类注册失败：%v", err)
		close(alertFsReady)
		return
	}
	inst, _, _ := alertFsGetModuleHandle.Call(0)
	// 建一次隐藏窗口复用：显示只是 SetWindowPos + ShowWindow，不反复建毁。
	hwnd, _, err := alertFsCreateWindowEx.Call(
		alertFsExLayered|alertFsExTopmost|alertFsExToolWindow|alertFsExNoActivate,
		uintptr(unsafe.Pointer(cls16)), 0, alertFsWsPopup,
		0, 0, 100, 100, 0, 0, inst, 0)
	if hwnd == 0 {
		log.Printf("[提醒] 全屏蒙层窗口创建失败：%v", err)
		close(alertFsReady)
		return
	}
	alertFsMu.Lock()
	alertFsHwnd = hwnd
	alertFsMu.Unlock()
	close(alertFsReady)

	var m alertFsMsg
	for {
		r, _, _ := alertFsGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if r == 0 || r == ^uintptr(0) {
			break
		}
		switch m.Message {
		case alertFsWmShow:
			alertFsApplyShow(m.Hwnd)
		case alertFsWmHide:
			alertFsShowWindow.Call(m.Hwnd, alertFsSwHide)
		}
	}
}

func alertFsLoadCursor(inst, id uintptr) uintptr {
	h, _, _ := alertFsLoadCursorW.Call(inst, id)
	return h
}

// alertFsApplyShow 把窗口挪到鼠标所在显示器并显示（不激活）。
func alertFsApplyShow(hwnd uintptr) {
	x, y, w, h := alertFsCursorMonitorRect()
	alertFsSetLayeredAlpha.Call(hwnd, 0, alertFsAlpha, alertFsLwaAlpha)
	alertFsSetWindowPos.Call(hwnd, ^uintptr(0) /*HWND_TOPMOST*/, uintptr(x), uintptr(y),
		uintptr(w), uintptr(h), alertFsSwpNoActivate)
	alertFsShowWindow.Call(hwnd, alertFsSwShowNoAct)
	alertFsInvalidateRect.Call(hwnd, 0, 1)
}

// alertFsCursorMonitorRect 鼠标所在显示器的矩形（找不到就退 1920x1080）。
func alertFsCursorMonitorRect() (int32, int32, int32, int32) {
	var pt alertFsPoint
	if r, _, _ := alertFsGetCursorPos.Call(uintptr(unsafe.Pointer(&pt))); r != 0 {
		mon, _, _ := alertFsMonitorFromPt.Call(uintptr(pt.X), uintptr(pt.Y), alertFsMonitorNearest)
		if mon != 0 {
			mi := alertFsMonitorInfo{CbSize: uint32(unsafe.Sizeof(alertFsMonitorInfo{}))}
			if r, _, _ := alertFsGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi))); r != 0 {
				return mi.RcMonitor.L, mi.RcMonitor.T, mi.RcMonitor.R - mi.RcMonitor.L, mi.RcMonitor.B - mi.RcMonitor.T
			}
		}
	}
	return 0, 0, 1920, 1080
}

// alertFsWndProc 窗口过程：绘制 + 点击关闭（跑在窗口线程，panic 就地兜住）。
func alertFsWndProc(hwnd, msg, wp, lp uintptr) uintptr {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[提醒] 全屏蒙层窗口过程 panic（已兜住）: %v", r)
		}
	}()
	switch msg {
	case alertFsWmLButtonDown:
		alertFsMu.Lock()
		dismiss := alertFsDismiss
		alertFsDismiss = nil
		alertFsMu.Unlock()
		alertFsShowWindow.Call(hwnd, alertFsSwHide)
		if dismiss != nil {
			dismiss() // 广播 ask_alert_dismiss：前端把主窗口的提醒层一起收起
		}
		return 0
	case alertFsWmPaint:
		var ps alertFsPaintStruct
		hdc, _, _ := alertFsBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		if hdc != 0 {
			alertFsDraw(hwnd, hdc)
			alertFsEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		}
		return 0
	}
	r, _, _ := alertFsDefWindowProc.Call(hwnd, msg, wp, lp)
	return r
}

// alertFsDraw 蒙层自绘：深色底 + 中央卡片（标题/摘要/提示行），尺寸按 DPI 缩放。
func alertFsDraw(hwnd, hdc uintptr) {
	alertFsMu.Lock()
	title, text := alertFsTitle, alertFsText
	alertFsMu.Unlock()
	if title == "" {
		title = "坚祥数字助理 · 有个请求在等你处理"
	}
	if text == "" {
		text = "有个请求需要你确认一下"
	}

	var rc alertFsRect
	alertFsGetClientRectW.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	w, h := rc.R-rc.L, rc.B-rc.T
	if w <= 0 || h <= 0 {
		return
	}

	dpi := alertFsDpi(hdc)
	px := func(v int32) int32 { return v * dpi / 96 }
	fill := func(color uint32, l, t, r, b int32) {
		br, _, _ := alertFsCreateSolidBrsh.Call(uintptr(color))
		if br != 0 {
			rr := alertFsRect{L: l, T: t, R: r, B: b}
			alertFsFillRect.Call(hdc, uintptr(unsafe.Pointer(&rr)), br)
			alertFsDeleteObject.Call(br)
		}
	}
	fill(alertFsBrushDim, rc.L, rc.T, rc.R, rc.B)

	font := func(pt int32) uintptr {
		name, _ := syscall.UTF16PtrFromString("Microsoft YaHei")
		f, _, _ := alertFsCreateFontW.Call(uintptr(^uintptr(px(pt)-1)) /*负值=字符高度*/, 0, 0, 0,
			600 /*FW_SEMIBOLD*/, 0, 0, 0, 1 /*DEFAULT_CHARSET*/, 0, 0, 5 /*CLEARTYPE_QUALITY*/, 0,
			uintptr(unsafe.Pointer(name)))
		return f
	}
	draw := func(s string, color uint32, f uintptr, l, t, r, b int32, flags uint32) {
		p16, err := syscall.UTF16PtrFromString(s)
		if err != nil {
			return
		}
		old, _, _ := alertFsSelectObject.Call(hdc, f)
		alertFsSetTextColor.Call(hdc, uintptr(color))
		alertFsSetBkMode.Call(hdc, alertFsBkTransparent)
		rr := alertFsRect{L: l, T: t, R: r, B: b}
		alertFsDrawTextW.Call(hdc, uintptr(unsafe.Pointer(p16)), ^uintptr(0) /*-1=按 NUL 结尾*/,
			uintptr(unsafe.Pointer(&rr)), uintptr(flags))
		alertFsSelectObject.Call(hdc, old)
	}

	// 中央卡片：宽 min(屏宽*62%, 900px)，高按内容自适应（固定 280px 基准 + DPI）
	cardW := w * 62 / 100
	if maxW := px(900); cardW > maxW {
		cardW = maxW
	}
	cardH := px(280)
	cl, ct := rc.L+(w-cardW)/2, rc.T+(h-cardH)/2
	cr, cb := cl+cardW, ct+cardH
	fill(alertFsBrushCard, cl, ct, cr, cb)
	fill(alertFsPenLine, cl, ct, cr, ct+px(4)) // 卡片顶边一条主题色细线

	fTitle, fBody, fSub := font(26), font(15), font(12)
	defer func() {
		alertFsDeleteObject.Call(fTitle)
		alertFsDeleteObject.Call(fBody)
		alertFsDeleteObject.Call(fSub)
	}()
	pad := px(28)
	draw(title, alertFsClrText, fTitle, cl+pad, ct+pad, cr-pad, ct+pad+px(48),
		alertFsDtCenter|alertFsDtVCenter|alertFsDtSingleLine|alertFsDtEndEllipsis|alertFsDtNoPrefix)
	draw(text, alertFsClrText, fBody, cl+pad, ct+pad+px(64), cr-pad, cb-pad-px(44),
		alertFsDtCenter|alertFsDtWordBreak|alertFsDtEndEllipsis|alertFsDtNoPrefix)
	draw("点击任意位置关闭全屏提醒（主窗口的提醒会一起收起）", alertFsClrSub, fSub,
		cl+pad, cb-pad-px(28), cr-pad, cb-pad,
		alertFsDtCenter|alertFsDtVCenter|alertFsDtSingleLine|alertFsDtNoPrefix)
}

// alertFsDpi 当前屏的 DPI（96 基准；拿不到按 96）。
func alertFsDpi(hdc uintptr) int32 {
	if r, _, _ := alertFsGetDeviceCaps.Call(hdc, alertFsLogPixelsX); r != 0 {
		return int32(r)
	}
	return 96
}
