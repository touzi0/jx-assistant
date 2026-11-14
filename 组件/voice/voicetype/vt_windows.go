//go:build windows

package main

// VoiceType Win32 层：PTT 键盘钩子 + waveIn 录音 + 可编辑检测/注入 + 悬浮窗壳。
//
// 骨架与纪律照抄主项目 recorder_windows.go（已趟平的坑）：
//   · 低级钩子回调只做「判键 → 无阻塞投递 channel」，慢活全在别处
//     （回调超时会被 Windows 静默摘钩——这是全局热键的头号坑）
//   · 回调 panic 就地 recover（原生栈上冒出去＝进程无声死亡）
//   · INJECTED 事件过滤（自家 Ctrl+V 不许触发自己）
//   · waveIn 缓冲回收只清 DONE 保留 PREPARED（全清＝音频静默中断，实测坑）
// 悬浮窗：CreateWindowExW(POPUP+NOACTIVATE+TOPMOST+TOOLWINDOW) + WebView2 附着，
// 胶囊即窗口、点击即结束——免去点击穿透切换的复杂度。

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

var (
	u32 = syscall.NewLazyDLL("user32.dll")
	k32 = syscall.NewLazyDLL("kernel32.dll")

	procSetWindowsHookEx         = u32.NewProc("SetWindowsHookExW")
	procUnhookWindowsHookEx      = u32.NewProc("UnhookWindowsHookEx")
	procCallNextHookEx           = u32.NewProc("CallNextHookEx")
	procGetMessageW              = u32.NewProc("GetMessageW")
	procPostThreadMessageW       = u32.NewProc("PostThreadMessageW")
	procGetForegroundWindow      = u32.NewProc("GetForegroundWindow")
	procGetCurrentThreadID       = k32.NewProc("GetCurrentThreadId")
	procCreateWindowExW          = u32.NewProc("CreateWindowExW")
	procDefWindowProcW           = u32.NewProc("DefWindowProcW")
	procRegisterClassExW         = u32.NewProc("RegisterClassExW")
	procShowWindow               = u32.NewProc("ShowWindow")
	procIsWindowVisible          = u32.NewProc("IsWindowVisible")
	procSetWindowPos             = u32.NewProc("SetWindowPos")
	procSetWindowRgn             = u32.NewProc("SetWindowRgn")
	procGetSystemMetrics         = u32.NewProc("GetSystemMetrics")
	procSystemParametersInfoW    = u32.NewProc("SystemParametersInfoW")
	procGetWindowThreadProcessId = u32.NewProc("GetWindowThreadProcessId")
	procGetClassNameW            = u32.NewProc("GetClassNameW")
	// 前台窗口进程名查询（宿主识别用；独立版靠它认「当前是不是主程序在模拟焦点」）
	procOpenProcess                = k32.NewProc("OpenProcess")
	procQueryFullProcessImageNameW = k32.NewProc("QueryFullProcessImageNameW")
	procCloseHandle                = k32.NewProc("CloseHandle")
	procGetGUIThreadInfo           = u32.NewProc("GetGUIThreadInfo")
	procGetAsyncKeyState           = u32.NewProc("GetAsyncKeyState")
	procGetLastInputInfo           = u32.NewProc("GetLastInputInfo")
	procGetTickCount64             = k32.NewProc("GetTickCount64")
	procMapVirtualKeyW             = u32.NewProc("MapVirtualKeyW")
	procGetClipboardSeq            = u32.NewProc("GetClipboardSequenceNumber")
	procSendInput                  = u32.NewProc("SendInput")
	procOpenClipboard              = u32.NewProc("OpenClipboard")
	procCloseClipboard             = u32.NewProc("CloseClipboard")
	procEmptyClipboard             = u32.NewProc("EmptyClipboard")
	procSetClipboardData           = u32.NewProc("SetClipboardData")
	procGetClipboardData           = u32.NewProc("GetClipboardData")
	procGlobalAlloc                = k32.NewProc("GlobalAlloc")
	procGlobalLock                 = k32.NewProc("GlobalLock")
	procGlobalUnlock               = k32.NewProc("GlobalUnlock")

	winmmDLL                  = syscall.NewLazyDLL("winmm.dll")
	procWaveInOpen            = winmmDLL.NewProc("waveInOpen")
	procWaveInGetNumDevs      = winmmDLL.NewProc("waveInGetNumDevs")
	procWaveInGetDevCapsW     = winmmDLL.NewProc("waveInGetDevCapsW")
	procWaveInStart           = winmmDLL.NewProc("waveInStart")
	procWaveInReset           = winmmDLL.NewProc("waveInReset")
	procWaveInClose           = winmmDLL.NewProc("waveInClose")
	procWaveInPrepareHeader   = winmmDLL.NewProc("waveInPrepareHeader")
	procWaveInUnprepareHeader = winmmDLL.NewProc("waveInUnprepareHeader")
	procWaveInAddBuffer       = winmmDLL.NewProc("waveInAddBuffer")
)

const (
	whKeyboardLL  = 13
	wmQuit        = 0x0012
	wmKeydown     = 0x0100
	wmKeyup       = 0x0101
	wmSysKeydown  = 0x0104
	wmSysKeyup    = 0x0105
	llkhfUp       = 0x0080
	llkhfInjected = 0x0010
	llkhfAltDown  = 0x0020 // 事件携带"此刻 Alt 按住"的标志（裸按判定要用）
	vkRControl    = 0xA3   // 按住说话（PTT）
	vkRMenu       = 0xA5   // 免提：按一下开始、再按结束

	whdrDone     = 0x00000001
	whdrPrepared = 0x00000002

	wsPopup          = 0x80000000
	wsExTopMost      = 0x00000008
	wsExNoActivate   = 0x08000000
	wsExToolWindow   = 0x00000080
	swHide           = 0
	swShownoActivate = 4
	swpNoActivate    = 0x0010
	swpShowWindow    = 0x0040
	swpHideWindow    = 0x0080
	cfUnicodeText    = 13
	gmemMoveable     = 0x0002
	spiGetWorkArea   = 0x0030
)

type kbdllHookStruct struct {
	VkCode    uint32
	ScanCode  uint32
	Flags     uint32
	Time      uint32
	ExtraInfo uintptr
}

type point struct{ X, Y int32 }

type rect struct{ L, T, R, B int32 }

type msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

type tWAVEFORMATEX struct {
	FormatTag, Channels           uint16
	SamplesPerSec, AvgBytesPerSec uint32
	BlockAlign, BitsPerSample     uint16
	CbSize                        uint16
}

type tWAVEHDR struct {
	LpData        uintptr
	BufferLength  uint32
	BytesRecorded uint32
	User          uintptr
	Flags         uint32
	Loops         uint32
	Next          uintptr
	Reserved      uintptr
}

// ---------- 快捷键配置（组件目录 hotkeys.json，与前端设置页共用权威源） ----------

type vtHotkeysCfg struct {
	Main []string `json:"main"` // 键名数组：["ralt"] / ["ctrl","f2"] / ["mouse-side"]…
	Sub  []string `json:"sub"`
	Mode string   `json:"mode"` // tap=点按切换 | hold=按住说话

	// TapMs 点按上限（毫秒）：tap 模式下，按住不超过它就按"点按切换"处理
	// （开麦/结束各一次）；超过也照样切换，只是日志会标注"按住较久"，不留死区。
	// 0 或未配 = 默认 1000。
	TapMs int `json:"tapMs,omitempty"`
	// HoldMs 长按阈值（毫秒）：hold 模式下按住超过它才真正开始录音；小于它算"点按"。
	// 目的是防误触——point 一下热键不该真的开麦，尤其是在用裸修饰键当热键时。
	// 0 或未配 = 默认 300。
	HoldMs int `json:"holdMs,omitempty"`
	// RightOnlyModifiers 裸修饰键只认**右侧**（alt→ralt / ctrl→rctrl / shift→rshift）。
	// 缺省 true：左 Alt 是菜单栏与 Alt+Tab 的公共入口，一旦被全局钩子接管就必然
	// 与系统抢键（实测：Alt+Tab 松手后切换器不关）；只接管右侧是常见折中。
	RightOnlyModifiers *bool `json:"rightOnlyModifiers,omitempty"`
	// MicDevice 指定采集设备（名称子串，大小写不敏感），如 "Realtek"、"tranScreen"。
	// 空=自动：优先挑"像真实麦克风"的设备，跳过虚拟设备（Oculus/tranScreen/
	// VB-Audio/Voicemeeter 等）——这些虚拟设备的默认输入常常是**纯静音**，
	// 录进去识别必然是 0 字（实测踩过：热键有反应、识别全空）。
	MicDevice string `json:"micDevice,omitempty"`
	// ReplayShortTap 点按（未达 HoldMs）时，把这次按键原样回放给前台程序。
	// 默认 true：热键吞键之后，"点一下左/右 Alt"这类原生操作不至于失效。
	ReplayShortTap *bool `json:"replayShortTap,omitempty"`

	// AvoidHost 前台是宿主程序时不响应（把听写让给宿主自己的按钮），nil=默认开。
	// 独立运行与组件运行共用这一套判定，见 vtForegroundIsHost。
	AvoidHost *bool `json:"avoidHost,omitempty"`
	// AvoidHostNames 宿主进程名名单（大小写不敏感）。**只用于独立运行**——
	// 组件模式下由组件桥注入宿主 PID，与进程名无关，产品改名不受影响。
	// 产品改名（坚祥数字助理 → AI数字助理）时改这里或改 hotkeys.json，无需改代码。
	AvoidHostNames []string `json:"avoidHostNames,omitempty"`
}

// vtDefaultAvoidHostNames 宿主进程名默认名单（hotkeys.json 未配 avoidHostNames 时用）。
// 改名过渡期两个名字都留着：老版本装的叫坚祥数字助理GO，新版本改叫 AI数字助理。
var vtDefaultAvoidHostNames = []string{"坚祥数字助理GO.exe", "AI数字助理.exe"}

// vtDefaultHoldMs 默认长按阈值（毫秒）。
const vtDefaultHoldMs = 300

var (
	vtHKMu sync.RWMutex
	// 默认：Alt 按住说话。**两侧都算**（用户的习惯就是不分左右），吞键只作用于
	// 实际被按下的那一侧；300ms 内若又按了别的键（Alt+Tab 这类组合），
	// 会把已吞的 Alt 原样还给前台程序，组合键照常生效（见 vtHookConsume 与
	// vtArming 的处理）。
	vtHK = vtHotkeysCfg{Main: []string{"alt"}, Sub: []string{}, Mode: "hold"}
)

// vtHotkeys 取一份配置快照（锁内拷贝，调用方不持锁）。
func vtHotkeys() vtHotkeysCfg {
	vtHKMu.RLock()
	defer vtHKMu.RUnlock()
	return vtHK
}

// vtRightOnlyModifiers 裸修饰键是否只认右侧（缺省 true）。
func vtRightOnlyModifiers() bool {
	if v := vtHotkeys().RightOnlyModifiers; v != nil {
		return *v
	}
	return true
}

// vtRightOnlyKeys 把裸修饰键改写成右侧：alt→ralt / ctrl→rctrl / shift→rshift。
// 组合里的裸修饰键同样改写（["ctrl","f2"]→["rctrl","f2"]）；非修饰键原样保留。
func vtRightOnlyKeys(keys []string) []string {
	if len(keys) == 0 {
		return keys
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		switch k {
		case "alt":
			k = "ralt"
		case "ctrl":
			k = "rctrl"
		case "shift":
			k = "rshift"
		}
		out = append(out, k)
	}
	return out
}

// vtHoldThreshold 长按阈值，含默认与下限保护。
func vtHoldThreshold() time.Duration {
	ms := vtHotkeys().HoldMs
	if ms <= 0 {
		ms = vtDefaultHoldMs
	}
	return time.Duration(ms) * time.Millisecond
}

// vtTapThreshold 点按上限（tap 模式下"点一下"的时长上限），默认 1 秒。
func vtTapThreshold() time.Duration {
	ms := vtHotkeys().TapMs
	if ms <= 0 {
		ms = 1000
	}
	return time.Duration(ms) * time.Millisecond
}

// vtReplayShortTapEnabled 点按是否回放原按键（缺省开）。
func vtReplayShortTapEnabled() bool {
	hk := vtHotkeys()
	if hk.ReplayShortTap == nil {
		return true
	}
	return *hk.ReplayShortTap
}

func vtHotkeysPath() string { return filepath.Join(vtDir, "hotkeys.json") }

// vtLoadHotkeys 重新装载热键与主题，来源优先级：
//
//	联动配置（主程序 voicetype.json / 环境变量 VT_HOTKEY）  >  本目录 hotkeys.json  >  内置默认
//
// 「快捷键从主程序取」就落在第一级：主程序把用户设的听写快捷键写进联动文件，
// 输入法每秒热加载，用户在设置页改完这边就跟着变，不用两处维护。
func vtLoadHotkeys() {
	link := vtLoadLink()

	// 1) 基线：内置默认（Alt 两侧都算，按住说话）
	c := vtHotkeysCfg{Main: []string{"alt"}, Sub: []string{}, Mode: "hold"}

	// 2) 本目录 hotkeys.json（手工兜底：长按阈值、点按回放、退避名单等）
	if raw, err := os.ReadFile(vtHotkeysPath()); err == nil {
		raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF}) // 容错 UTF-8 BOM（PS5 Set-Content 等）
		var f vtHotkeysCfg
		if json.Unmarshal(raw, &f) == nil {
			if len(f.Main) > 0 || len(f.Sub) > 0 {
				c.Main = append([]string{}, f.Main...)
				if f.Sub == nil {
					c.Sub = []string{}
				} else {
					c.Sub = append([]string{}, f.Sub...)
				}
			}
			if f.Mode != "" {
				c.Mode = f.Mode
			}
			c.HoldMs = f.HoldMs
			c.TapMs = f.TapMs
			c.ReplayShortTap = f.ReplayShortTap
			c.AvoidHost = f.AvoidHost
			c.AvoidHostNames = f.AvoidHostNames
		}
	}

	// 3) 联动覆盖：主程序说的算（热键 + 触发方式 + 长按阈值）
	//
	// 判据用 Main != nil 而不是非空：主程序写的是 `[]` 时是**用户显式停用**
	//（设置页把主热键叉掉），必须照办——按空数组回退到 hotkeys.json 会让旧键
	// 死而复生（实测 bug：叉掉鼠标侧键后输入法还在用侧键）。字段缺失/null 才算
	// 联动没管热键（只有颜色的手写文件），这时才留给下一级。
	src := "本目录 hotkeys.json"
	if link.Cfg.Hotkeys.Main != nil {
		c.Main = append([]string{}, link.Cfg.Hotkeys.Main...)
		if link.Cfg.Hotkeys.Sub == nil {
			c.Sub = []string{}
		} else {
			c.Sub = append([]string{}, link.Cfg.Hotkeys.Sub...)
		}
		src = "主程序联动（" + vtSourceDesc() + "）"
	}
	// 触发方式也跟着主程序：主程序设置里选"点按切换"或"长按说话"，输入法不该另有一套
	if m := strings.ToLower(strings.TrimSpace(link.Cfg.Hotkeys.Mode)); m == "tap" || m == "hold" {
		c.Mode = m
	}
	if link.Cfg.HoldMs > 0 {
		c.HoldMs = link.Cfg.HoldMs
	}
	if link.Cfg.TapMs > 0 {
		c.TapMs = link.Cfg.TapMs
	}
	if c.Mode != "tap" {
		c.Mode = "hold"
	}
	// 裸修饰键只认右侧（缺省开）：左 Alt/左 Ctrl/左 Shift 完全还给系统。
	// 这条必须在"联动覆盖"之后做——主程序设置里写的是 alt（不分侧），
	// 落到本机由这里决定只接管右侧。
	if vtRightOnlyModifiers() {
		before := append(append([]string{}, c.Main...), c.Sub...)
		c.Main = vtRightOnlyKeys(c.Main)
		c.Sub = vtRightOnlyKeys(c.Sub)
		after := append(append([]string{}, c.Main...), c.Sub...)
		if strings.Join(before, "+") != strings.Join(after, "+") {
			vtLog("裸修饰键只认右侧：%v → main=%v sub=%v（左 Alt/Ctrl/Shift 不接管，菜单与 Alt+Tab 不受影响）",
				before, c.Main, c.Sub)
		}
	}

	vtHKMu.Lock()
	vtHK = c
	vtHKMu.Unlock()
	vtLog("快捷键: main=%v sub=%v mode=%s 长按阈值=%dms 点按上限=%dms 点按回放=%v 退避宿主=%v 名单=%v 来源=%s",
		c.Main, c.Sub, c.Mode, vtHoldThreshold().Milliseconds(), vtTapThreshold().Milliseconds(), vtReplayShortTapEnabled(),
		vtAvoidHostEnabled(), vtAvoidHostNames(), src)
	vtApplyTheme(true)
	vtLog("主题: %s", vtThemeDesc())
}

// vtHotkeysWatch 每秒看两个文件的 mtime：本目录 hotkeys.json 与主程序的联动配置。
// 任一变化就整体重载（主程序改完点缀色/快捷键，这边秒级跟上）。
func vtHotkeysWatch() {
	var lastHK, lastLink time.Time
	for {
		time.Sleep(time.Second)
		changed := false
		if fi, err := os.Stat(vtHotkeysPath()); err == nil && fi.ModTime() != lastHK {
			lastHK, changed = fi.ModTime(), true
		}
		for _, p := range vtLinkCandidatePaths() {
			fi, err := os.Stat(p)
			if err != nil {
				continue
			}
			if fi.ModTime() != lastLink {
				lastLink, changed = fi.ModTime(), true
			}
			break
		}
		if changed {
			vtLoadHotkeys()
		}
	}
}

// ---------- 按键匹配：键名域（修饰键区分左右）+ 组合匹配 ----------
//
// 为什么要区分左右：热键一旦"吞键"（见 vtHookConsume），就不能再让 `alt` 这种
// 不分侧的写法同时吞掉两侧 Alt —— 左 Alt 是菜单栏/Alt+Tab 的入口，吞了就废。
// 默认热键因此改成右 Alt（ralt），左 Alt 留给系统和其它程序。

// vtKeyName vk → 规范键名。修饰键区分左右，其余复用原域。
func vtKeyName(vk uint32) string {
	switch vk {
	case 0xA0:
		return "lshift"
	case 0xA1:
		return "rshift"
	case 0xA2:
		return "lctrl"
	case 0xA3:
		return "rctrl"
	case 0xA4:
		return "lalt"
	case 0xA5:
		return "ralt"
	case 0x5B:
		return "lmeta"
	case 0x5C:
		return "rmeta"
	case 0x20:
		return "space"
	}
	if vk >= 'A' && vk <= 'Z' {
		return string(rune('a' + vk - 'A'))
	}
	if vk >= '0' && vk <= '9' {
		return string(rune(vk))
	}
	if vk >= 0x70 && vk <= 0x7B {
		return fmt.Sprintf("f%d", vk-0x6F)
	}
	return ""
}

// vtKeyMatches 配置键名 vs 实际按下的键名：`alt`/`ctrl`/`shift`/`meta` 匹配左右任一侧，
// 配了 `ralt` 这类具体一侧的就只匹配那一侧。旧配置（`["alt"]`）保持能用。
// 鼠标侧键同理：`mouse-side` 两枚都算（旧写法），`mouse-side-1`/`mouse-side-2`
// 只认对应那一枚（XBUTTON1=侧键1·后退位 / XBUTTON2=侧键2·前进位）。
func vtKeyMatches(configured, actual string) bool {
	if configured == actual {
		return true
	}
	switch configured {
	case "alt":
		return actual == "lalt" || actual == "ralt"
	case "ctrl":
		return actual == "lctrl" || actual == "rctrl"
	case "shift":
		return actual == "lshift" || actual == "rshift"
	case "meta":
		return actual == "lmeta" || actual == "rmeta"
	case "mouse-side":
		return actual == "mouse-side-1" || actual == "mouse-side-2"
	}
	return false
}

// vtIsModifierName 是不是修饰键（含不分侧的写法）。
func vtIsModifierName(name string) bool {
	switch name {
	case "lshift", "rshift", "lctrl", "rctrl", "lalt", "ralt", "lmeta", "rmeta",
		"shift", "ctrl", "alt", "meta":
		return true
	}
	return false
}

// vtIsAltName Alt 类键：它们的抬起必须无条件吞掉（见 vtHookConsume 的说明）。
func vtIsAltName(name string) bool {
	return name == "lalt" || name == "ralt" || name == "alt"
}

// vtModifierVKs 全部修饰键的 vk（与 SayIt MODIFIER_VK_CODES 同一份口径）。
var vtModifierVKs = []uint32{0xA0, 0xA1, 0xA2, 0xA3, 0xA4, 0xA5, 0x5B, 0x5C}

var (
	vtHeld     = map[string]bool{} // 当前物理按住的键名（左右分开；鼠标侧键为 "mouse-side-1"/"mouse-side-2"）
	vtConsumed = map[string]bool{} // 已被我们吞掉 down 的键名 → 配对 up 也要吞
	vtComboOn  [2]bool             // main/sub 组合的匹配态（边沿检测）
)

// 「组合快捷键介入」状态：按住热键期间（判定窗口乃至已经开麦之后）用户又按了
// 别的键，说明他要的是组合快捷键（Alt+Tab / Ctrl+C …），本轮应当让路，且必须把
// 已经吞掉的那个键**原样还给前台**——否则前台只收到 Tab 收不到 Alt，组合就废了。
//
// 典型场景（用户实测反馈）：按住 Alt 不放、连续按 Tab 切窗口。
// 第一次 Tab 就要把 Alt 还原；之后 Alt 的 repeat down 与抬起全部放行，
// 系统看到的是完整的 Alt+Tab，反复按 Tab 照常循环；同时本轮语音作废。
//
// 这些量在钩子回调里读写（回调是单线程），用原子量是为了不持锁、不阻塞回调。
var (
	vtArming      atomic.Bool            // 正处判定窗口（已吞下热键，还没开始录音）
	vtArmVK       atomic.Uint32          // 当前这一轮被吞的热键 vk（判定窗口→录音期间都记着）
	vtArmCancel   = make(chan string, 4) // 回调 → 事件循环：组合键介入，本轮作废/取消
	vtPassthrough = map[string]bool{}    // down 已还给前台的键：配对 up（含 repeat）也要放行
	vtRecordingOn atomic.Bool            // 正在录音（回调据此判断"是否要取消本轮"）
	// vtInjected 记"我们替用户补发过 down、真实 up 还没到"的键。
	//
	// 为什么必须记：补发 down 之后系统认为这个键**按着**。此时真实抬起若还按
	// "孤立 Alt up 一律吞"处理，系统就永远收不到 up —— 表现正是 Alt+Tab 的切换器
	// 松手不关、得多按一次 Alt 才消失（用户实测反馈）。有了这份账，抬起优先放行。
	vtInjected = map[string]bool{}
	// vtHotVKDown 当前被我们吞住的热键 vk（0=没按着）。
	// **不能用 GetAsyncKeyState 判断"热键还按着吗"**：低级钩子吞掉一个键之后，
	// 事件不进输入队列，GetAsyncKeyState 会报告它没按下（实测坑：每次长按都被
	// 自己的"漏 up"复核取消掉，长按永远起不来）。这里自己记账。
	vtHotVKDown atomic.Uint32
)

// vtReplayHeldDown 把已经吞掉的按键 down 还给前台（只发 down，up 由真实事件放行）。
// 在钩子回调线程调用：清掉 consumed 位、记 passthrough 与 injected，保证配对 up
// 一定放行（passthrough 管"下一个 up 放行"，injected 管"万一 up 丢了要补发"）。
func vtReplayHeldDown(vk uint32) {
	name := vtKeyName(vk)
	if name != "" {
		delete(vtConsumed, name)
		vtPassthrough[name] = true
		vtInjected[name] = true
	}
	vtSendKeyEvent(vk, true, false)
}

// vtHeldAny 某个配置键名当前是否按住（`alt` 认两侧）。
func vtHeldAny(configured string) bool {
	for name, on := range vtHeld {
		if on && vtKeyMatches(configured, name) {
			return true
		}
	}
	return false
}

// vtComboSatisfied 组合是否成立；except 用于"主键按下前"的判定（排除自身）。
func vtComboSatisfied(keys []string, except string) bool {
	for _, k := range keys {
		if except != "" && vtKeyMatches(k, except) {
			continue
		}
		if !vtHeldAny(k) {
			return false
		}
	}
	return true
}

// vtKeyIsHotkeyMember 这个键是否属于某个已配置的热键（组合成员也算）。
//
// 用途：判定窗口里要区分"用户按了别的键（组合快捷键介入）"与"用户还在按我们自己的
// 热键"。后者包括修饰键成员（配 `ctrl+f2` 时按住 Ctrl 会不停发 repeat）——
// 不排除它们的话，每 30ms 就会把本轮作废一次。
func vtKeyIsHotkeyMember(name string) bool {
	if name == "" {
		return false
	}
	hk := vtHotkeys()
	for _, arr := range [2][]string{hk.Main, hk.Sub} {
		for _, k := range arr {
			if vtKeyMatches(k, name) {
				return true
			}
		}
	}
	return false
}

// vtAsyncKeyDown 物理键状态（漏 up 自愈用；kb_flags 之外唯一可靠的来源）。
func vtAsyncKeyDown(vk uint32) bool {
	r, _, _ := procGetAsyncKeyState.Call(uintptr(vk))
	return int16(uint16(r)) < 0 // 最高位=当前按下
}

// vtOtherModifierDown 除 self 之外还有别的修饰键按着吗（裸按判定）。
// self 是本键的 vk：AltGr 场景（左Ctrl+右Alt）里"自己带自己"不能算违规。
func vtOtherModifierDown(self uint32) bool {
	for _, vk := range vtModifierVKs {
		if vk == self {
			continue
		}
		if vtAsyncKeyDown(vk) {
			return true
		}
	}
	return false
}

// vtReconcileHeld 用物理键状态校正键集：漏掉的 up 会让组合永久"按住"，
// 之后热键就再也不响应了（SayIt 用同一招，见 reconcile_stale_ptt_members）。
func vtReconcileHeld() {
	// 补发过 down 但真实抬起丢了（钩子被摘/事件丢失）→ 系统会认为那个键一直按着，
	// 表现是 Alt+Tab 切换器卡住、菜单一直亮。这里用物理状态判定"人早松手了"，
	// 直接补发一次 up 把系统状态闭合。
	for name := range vtInjected {
		vk := vtVKByName(name)
		if vk == 0 {
			delete(vtInjected, name)
			continue
		}
		if !vtAsyncKeyDown(vk) {
			vtSendKeyEvent(vk, false, true)
			delete(vtInjected, name)
			delete(vtPassthrough, name)
			vtLog("补发抬起：%s 的 down 是我们补的但抬起丢了，已补 up（防止系统认为它一直按着）", name)
		}
	}
	for name, on := range vtHeld {
		if !on || strings.HasPrefix(name, "mouse-side") {
			continue // 鼠标侧键没有 vk，不在物理键自愈的范围（鼠标钩子自己记账）
		}
		// 被我们吞住的键不能拿 GetAsyncKeyState 作依据：吞掉的键事件不进输入队列，
		// 物理状态在系统眼里就是"没按下"，照这个自愈会把热键键位误清掉。
		if vtConsumed[name] {
			continue
		}
		vk := vtVKByName(name)
		if vk != 0 && !vtAsyncKeyDown(vk) {
			delete(vtHeld, name)
			delete(vtConsumed, name)
			vtComboOn[0], vtComboOn[1] = false, false
			vtLog("键集自愈：%s 已物理松开但位图仍按着（漏 up），已清", name)
		}
	}
}

// vtVKByName 规范键名 → vk（自愈与回放用）。
func vtVKByName(name string) uint32 {
	for _, vk := range []uint32{0xA0, 0xA1, 0xA2, 0xA3, 0xA4, 0xA5, 0x5B, 0x5C, 0x20} {
		if vtKeyName(vk) == name {
			return vk
		}
	}
	if len(name) == 1 {
		c := name[0]
		if c >= 'a' && c <= 'z' {
			return uint32('A' + c - 'a')
		}
		if c >= '0' && c <= '9' {
			return uint32(c)
		}
	}
	if strings.HasPrefix(name, "f") {
		var n int
		if _, err := fmt.Sscanf(name, "f%d", &n); err == nil && n >= 1 && n <= 12 {
			return uint32(0x6F + n)
		}
	}
	return 0
}

// ---------- 钩子事件与吞键判定 ----------

// vtHotkeyEvent 一次热键边沿（组合成立/断开）。
type vtHotkeyEvent struct {
	Down bool
	Slot int    // 0=main 1=sub
	VK   uint32 // 实际按下的那个 vk（点按回放用）
	Name string
	// Synthetic 由 trigger.txt 后门合成的假按键：不做物理键状态校验、也不回放。
	Synthetic bool
}

var vtHKEvents = make(chan vtHotkeyEvent, 8) // 回调只投递，绝不阻塞

// 钩子看门狗用的私有消息（WM_APP + 0x52）：让钩子线程重装钩子。
const vtWmReloadHooks = 0x8000 + 0x52

// 上次钩子回调时间（毫秒，看门狗判断"是否被系统静默摘掉"）。
var vtLastHookCbMs atomic.Int64

// vtHookHostForeground 宿主（本产品主程序）是否在前台。
// 钩子回调里查进程名太慢（会超时被摘钩），所以由 vtHostWatch 每 150ms 轮询一次，
// 回调只读这个原子量。语义与原来的"热键让路"完全一致。
var vtHookHostForeground atomic.Bool

// vtHostWatch 轮询「前台是不是宿主」，只在状态变化时写日志（100ms 一次不能刷屏）。
// 轮询要够密：钩子回调是拿这个缓存值当"壁障"的，间隔太长会出现"刚切到主程序就按
// Alt，缓存里还是旧值"的窄窗口（那一下仍会 arm → 松手回放 → 把主程序的听写 toggle 掉）。
func vtHostWatch() {
	last := false
	for {
		cur := vtAvoidHostEnabled() && vtForegroundIsHost()
		if cur != last {
			vtHookHostForeground.Store(cur)
			if cur {
				vtLog("前台切到宿主程序：钩子层彻底让路（不吞键、不 arm、不回放）")
			} else {
				vtLog("前台离开宿主程序：热键恢复接管")
			}
			last = cur
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// vtOtherModifierDownFn 单测可替换的"是否有别的修饰键按着"。
//
// 生产实现读物理键状态（GetAsyncKeyState），但单测里这依赖**运行测试那一刻用户
// 手上按着什么**——用户正按着 Alt 时，所有"裸按应接管"的用例都会红（实测踩过）。
// 因此这里留一个可替换点，测试统一替换成"没有修饰键"。
var vtOtherModifierDownFn = vtOtherModifierDown

// vtHookConsume 决定这个键事件要不要**吞掉**（true=不调 CallNextHookEx，前台程序收不到）。
//
// 这是本轮修复的核心：老版本无论命中与否都 CallNextHookEx 放行，按住的 Alt 被前台
// 程序收到，Windows 把它判成"点了一下 Alt"→ 激活菜单栏 → 焦点从输入框跳到菜单，
// 随后注入的 Ctrl+V 落在菜单上（SendInput 照样报成功，假成功）。规则照 SayIt：
//
//	· 单键热键：裸按才接管（按下时其它修饰键也按着 → 整条放行，Alt+F4 等系统快捷键不受影响）
//	· 吞了 down 就吞配对的 up；**Alt 类键的 up 无条件吞**（否则抬起仍会被判成"点了 Alt"）
//	· 组合热键：只吞主键，且要求修饰键先按住；修饰键自身永不吞（不制造孤立 up）
//	· 宿主在前台 → 全放行（避让逻辑不变）
//
// 返回 (consume, hotkey)：consume=本次事件吞掉；hotkey=这是配置的热键（要投递事件）。
func vtHookConsume(name string, vk, flags uint32, down bool) (bool, bool) {
	if name == "" {
		return false, false
	}
	hk := vtHotkeys()
	slot, keys := -1, []string(nil)
	for i, arr := range [2][]string{hk.Main, hk.Sub} {
		for _, k := range arr {
			if vtKeyMatches(k, name) {
				slot, keys = i, arr
				break
			}
		}
		if slot >= 0 {
			break
		}
	}
	if slot < 0 {
		return false, false
	}
	if vtHookHostForeground.Load() {
		return false, false // 宿主在前台：让路（与原先一致，只是现在连键也不吞）
	}
	// 这个键的 down 已经还给前台了（组合快捷键介入过）：配对的 up 也放行，
	// 否则前台会看到一个"按下去没人抬起"的修饰键。
	if vtPassthrough[name] {
		if !down {
			delete(vtPassthrough, name)
			// 真实抬起已经到了，补发账一起清：否则自愈逻辑会再补一次抬起，
			// 多出来的孤立 Alt up 又会被前台当成"点了一下 Alt"（弹菜单）。
			delete(vtInjected, name)
		}
		return false, false
	}

	if len(keys) == 1 {
		// 单键热键：要求裸按。AltGr（左Ctrl+右Alt）与 Alt+F4 这类组合一律放行。
		blocked := vtOtherModifierDownFn(vk)
		if flags&llkhfAltDown != 0 && !vtIsAltName(name) {
			blocked = true
		}
		if blocked {
			return false, false // 带其它修饰键：整条放行，也不触发录音
		}
		if down {
			if vtConsumed[name] {
				// 按住不放产生的 repeat：仍然是**我们的热键**（只是不再派发新边沿）。
				// hotkey 必须为 true —— 否则回调会把 repeat 当成"用户按了别的键"，
				// 触发组合键介入、把还没到 300ms 的这一轮作废掉（实测坑：长按永远
				// 起不来，日志里每 30ms 一条"判定期内按下其它键 ralt"）。
				return true, true
			}
			vtConsumed[name] = true
			vtHotVKDown.Store(vk) // 自己记账：吞掉的键在 GetAsyncKeyState 里是"没按下"
			return true, true
		}
		if vtConsumed[name] {
			delete(vtConsumed, name)
			if vtHotVKDown.Load() == vk {
				vtHotVKDown.Store(0)
			}
			return true, true
		}
		// 我们替用户补发过 down（系统认为它按着）→ 这次真实抬起**必须放行**，
		// 否则系统状态闭合不了：Alt+Tab 切换器松手不关、得多按一次 Alt。
		if vtInjected[name] {
			delete(vtInjected, name)
			return false, false
		}
		// 绝不无条件吞 Alt 的抬起！
		// 历史 bug（用户实测："粘不了/按一下左 Alt 才恢复"）：左 Alt 不在我们的热键里，
		// 它的按下被放行给前台，抬起却被这条"孤立 Alt up 一律吞"吃掉——系统就认为
		// 左 Alt 一直按着（stuck LALT），于是下一次 Ctrl+V 变成 Ctrl+Alt+V 粘不出来。
		// 只有"我们自己吞过 down 的键"才吞它的 up（上面 vtConsumed 分支已覆盖）。
		return false, false
	}

	// 组合热键：只接管主键（非修饰键那一枚）
	if vtIsModifierName(name) {
		return false, false // 修饰键永不吞，避免给前台留下孤立事件
	}
	if down {
		if vtConsumed[name] {
			// 同单键：repeat 仍是我们的热键（不重复派发边沿，但也不能被当成"别的键"）
			return true, true
		}
		if !vtComboSatisfied(keys, name) {
			return false, false // 修饰键还没到位（K→Ctrl 顺序）：这条按键照常送给前台
		}
		vtConsumed[name] = true
		vtHotVKDown.Store(vk)
		return true, true
	}
	if vtConsumed[name] {
		delete(vtConsumed, name)
		if vtHotVKDown.Load() == vk {
			vtHotVKDown.Store(0)
		}
		return true, true
	}
	return false, false
}

// vtEvalCombos 键集变化后评估两个组合：全按住=匹配；边沿投递事件。
func vtEvalCombos(name string, vk uint32) {
	hk := vtHotkeys()
	slots := [2][]string{hk.Main, hk.Sub}
	for i, keys := range slots {
		if len(keys) == 0 {
			vtComboOn[i] = false
			continue
		}
		m := vtComboSatisfied(keys, "")
		if m && !vtComboOn[i] {
			vtComboOn[i] = true
			select {
			case vtHKEvents <- vtHotkeyEvent{Down: true, Slot: i, VK: vk, Name: name}:
			default:
			}
		} else if !m && vtComboOn[i] {
			vtComboOn[i] = false
			select {
			case vtHKEvents <- vtHotkeyEvent{Down: false, Slot: i, VK: vk, Name: name}:
			default:
			}
		}
	}
}

// ---------- PTT 键盘/鼠标钩子 ----------

var (
	vtKbdCbOnce bool
	vtKbdCb     uintptr
	vtMouseCb   uintptr
	vtHookTID   uint32
	vtHkKbd     uintptr // 当前键盘钩子句柄（看门狗重装时替换）
	vtHkMouse   uintptr
)

func vtKbdCallback() uintptr {
	if !vtKbdCbOnce {
		vtKbdCbOnce = true
		vtKbdCb = syscall.NewCallback(func(nCode int32, wParam uintptr, lParam unsafe.Pointer) uintptr {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[voicetype] 键盘钩子回调 panic（已兜住）: %v", r)
				}
			}()
			if nCode >= 0 && (wParam == wmKeydown || wParam == wmKeyup || wParam == wmSysKeydown || wParam == wmSysKeyup) {
				k := (*kbdllHookStruct)(lParam)
				// 记账放在**注入过滤之前**：自检探针是注入事件，必须也被记到，
				// 否则"钩子还活着吗"这个判断没有可靠信号（实测：钩子死了却以为活着）。
				if k.Flags&llkhfInjected == 0 { // 自家 Ctrl+V / 短按回放不算"用户输入"
					vtLastHookCbMs.Store(time.Now().UnixMilli())
				}
				if k.Flags&llkhfInjected == 0 { // 自家 Ctrl+V / 短按回放不许触发自己
					// 壁障：宿主（本产品主程序）在前台时**彻底让路**——连键集维护、
					// 组合评估、arm 都不做，直接把键原样放行。
					// 为什么要这么彻底：以前只是"不吞键"但照常 arm，松手时点按逻辑会
					// 把 Alt **回放**一遍，主程序收到"第二下 Alt"，它自己的听写就被
					// toggle 掉了（现象：按一下 Alt 主程序开始听写，零点几秒就退出）。
					// 避让必须在钩子最前面，不能等到 vtStartSession 才判断。
					if vtHookHostForeground.Load() {
						r, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, uintptr(lParam))
						return r
					}
					kn := vtKeyName(k.VkCode)
					down := k.Flags&llkhfUp == 0
					consume, hotkey := vtHookConsume(kn, k.VkCode, k.Flags, down)
					// 同步记下"这一轮按住的是哪一侧的热键"。必须在这里、而不是事件循环里：
					// 组合键介入（判定窗口内按了别的键）会在本回调里**立刻**补发这个键的
					// down，若那时 vtArmVK 还是上一轮的旧值（例如左 Alt 0xA4），就会把
					// "右 Alt 按住"补成"左 Alt 按下"——真实抬起只清右侧，左 Alt 永久卡住。
					// 这正是"两边被当成一边"的根源。
					if hotkey && down && !vtConsumed[kn] {
						vtArmVK.Store(k.VkCode)
						vtArming.Store(true)
					}
					// 修饰键事件追踪：排查"Alt/Ctrl 被占住、粘贴失灵"时，这三行日志能定位
					// 到底是"我们吞了它"还是"系统/别的程序拿走了它"。量很小（只在修饰键上打）。
					switch kn {
					case "lalt", "ralt", "lctrl", "rctrl", "lshift", "rshift":
						vtLog("修饰键事件: %s down=%v → 吞=%v 是热键=%v | consumed=%v injected=%v passthrough=%v",
							kn, down, consume, hotkey, vtConsumed[kn], vtInjected[kn], vtPassthrough[kn])
					}
					// 按住热键期间又按了"别的键" → 用户要的是组合快捷键
					//（典型：按住 Alt 连按 Tab 切窗口）。两种时机都要让路：
					//   · 还在长按判定窗口里 → 本轮作废（不录音）
					//   · 已经在录音           → 取消本轮，绝不把语音灌进正在操作的窗口
					// 两种情况都要把已吞掉的热键还原给前台，否则组合只剩半截。
					// 「别的键」= 不属于任何已配置热键的键：热键自己的成员（含修饰键
					// 的 repeat）绝不能算进来，否则长按/组合热键会被自己的重复事件作废。
					if kn != "" && down && !vtKeyIsHotkeyMember(kn) {
						intervene := vtArming.CompareAndSwap(true, false) ||
							(vtRecordingOn.Load() && !vtPassthrough[vtKeyName(vtArmVK.Load())])
						if intervene {
							if vk := vtArmVK.Load(); vk != 0 {
								vtReplayHeldDown(vk)
							}
							select {
							case vtArmCancel <- kn:
							default:
							}
						}
					}
					if kn != "" {
						// 键集维护放在吞键判定之后：组合判定要的是"按下前"的状态
						if down {
							if !vtHeld[kn] {
								vtReconcileHeld()
								vtHeld[kn] = true
								vtEvalCombos(kn, k.VkCode)
							} else if hotkey {
								// 按住产生的 repeat：不重复派发（组合边沿已在上次置位）
							}
						} else if vtHeld[kn] {
							delete(vtHeld, kn)
							vtEvalCombos(kn, k.VkCode)
						}
					}
					if consume {
						return 1 // 吞掉：不调 CallNextHookEx，前台程序收不到这个键
					}
				}
			}
			r, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, uintptr(lParam))
			return r
		})
	}
	return vtKbdCb
}

// 鼠标侧键：WH_MOUSE_LL 只认 XBUTTON1/2，分别映射成 mouse-side-1 / mouse-side-2
// （配置写 `mouse-side` 则两枚都算，旧配置不变；只配某一枚时，另一枚完全放行，
// 保留它原生的后退/前进功能）。绑了侧键的那枚 down/up 都吞——否则每次说话
// 都等于给浏览器一次"后退"导航；没绑的那枚不吞不记，前台行为分毫不差。
func vtMouseCallback() uintptr {
	if vtMouseCb == 0 {
		vtMouseCb = syscall.NewCallback(func(nCode int32, wParam uintptr, lParam unsafe.Pointer) uintptr {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[voicetype] 鼠标钩子回调 panic（已兜住）: %v", r)
				}
			}()
			const wmXButtonDown, wmXButtonUp = 0x020B, 0x020C
			if nCode >= 0 && (wParam == wmXButtonDown || wParam == wmXButtonUp) {
				m := (*msllHookStruct)(lParam)
				if m.Flags&0x0001 == 0 { // LLMHF_INJECTED 过滤
					if vtHookHostForeground.Load() { // 宿主在前台：鼠标键同样彻底让路
						r, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, uintptr(lParam))
						return r
					}
					if name := vtMouseKeyName(m.MouseData >> 16); name != "" {
						down := wParam == wmXButtonDown
						// 只接管热键绑到**这一枚**侧键的情形：绑 mouse-side（两枚都算）
						// 时两枚都吞（与旧行为一致）；只绑 mouse-side-1 时侧键 2 整条放行。
						consume := vtMouseInHotkeys(vtHotkeys(), name)
						if down && !vtHeld[name] {
							vtHeld[name] = true
							vtEvalCombos(name, 0)
						} else if !down && vtHeld[name] {
							delete(vtHeld, name)
							vtEvalCombos(name, 0)
						}
						if consume {
							return 1
						}
					}
				}
			}
			r, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, uintptr(lParam))
			return r
		})
	}
	return vtMouseCb
}

// vtMouseKeyName XBUTTON 编号（MouseData 高字）→ 键名；1=侧键1（后退位）2=侧键2（前进位）。
func vtMouseKeyName(xb uint32) string {
	switch xb {
	case 1:
		return "mouse-side-1"
	case 2:
		return "mouse-side-2"
	}
	return ""
}

// vtMouseInHotkeys 这枚侧键（mouse-side-1 / mouse-side-2）有没有热键绑在它上面：
// 配了 `mouse-side` 的两枚都算；配了具体一枚的只算那一枚。
func vtMouseInHotkeys(hk vtHotkeysCfg, name string) bool {
	for _, arr := range [2][]string{hk.Main, hk.Sub} {
		for _, k := range arr {
			if vtKeyMatches(k, name) {
				return true
			}
		}
	}
	return false
}

// msllHookStruct WH_MOUSE_LL 的 lParam 结构（只用到 MouseData 高字的 XBUTTON）。
type msllHookStruct struct {
	Pt        point
	MouseData uint32
	Flags     uint32
	Time      uint32
	ExtraInfo uintptr
}

// vtHookLoop 专用线程：装键盘+鼠标两个低级钩子跑消息循环（低级钩子对安装
// 线程的硬性要求）。同时处理看门狗的重装请求。
func vtHookLoop() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	h, _, err := procSetWindowsHookEx.Call(whKeyboardLL, vtKbdCallback(), 0, 0)
	if h == 0 {
		log.Printf("[voicetype] 键盘钩子安装失败: %v", err)
		return
	}
	hm, _, _ := procSetWindowsHookEx.Call(14 /*WH_MOUSE_LL*/, vtMouseCallback(), 0, 0)
	tid, _, _ := procGetCurrentThreadID.Call()
	vtHookTID = uint32(tid)
	vtHkKbd, vtHkMouse = h, hm
	log.Printf("[voicetype] 键盘+鼠标钩子已安装 hKbd=%d hMouse=%d", h, hm)
	go vtHookWatchdog() // 低级钩子会被系统静默摘除 → 定期重装

	var m msg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if r == 0 || r == ^uintptr(0) {
			break
		}
		if m.Message == vtWmReloadHooks {
			// 先装新的、成功了再卸旧的：中间不留"没有钩子"的空窗
			nh, _, nerr := procSetWindowsHookEx.Call(whKeyboardLL, vtKbdCallback(), 0, 0)
			if nh != 0 {
				procUnhookWindowsHookEx.Call(vtHkKbd)
				vtHkKbd = nh
			} else {
				log.Printf("[voicetype] 看门狗重装键盘钩子失败: %v", nerr)
			}
			nhm, _, _ := procSetWindowsHookEx.Call(14, vtMouseCallback(), 0, 0)
			if nhm != 0 {
				procUnhookWindowsHookEx.Call(vtHkMouse)
				vtHkMouse = nhm
			}
		}
	}
	procUnhookWindowsHookEx.Call(h)
	if hm != 0 {
		procUnhookWindowsHookEx.Call(hm)
	}
}

// vtHookHealth 钩子健康度探针（每 3 秒）。
//
// 为什么必须有：低级钩子被系统静默摘除时（回调超时/线程被挂住），SetWindowsHookEx
// 的句柄仍然有效、日志毫无异常，但**再也收不到任何按键**。实测抓到过：录音会话
// 一直开着、麦克风图标亮着、按键全部失灵；看门狗虽然每分钟 PostThreadMessage 请求
// 重装，但那次重装排在已经卡住的线程消息队列里，等于没装。
//
// 做法：注入一个无害的 F24 按下去探路（带 INJECTED 标志，自家钩子只当"我还活着"，
// 不参与热键判定）；400ms 内时间戳没动就判定失效，**在新线程上重新装一套钩子**
// （不依赖那个可能已卡死的线程），并强制结束卡住的录音会话。
var (
	vtHookDead      atomic.Bool
	vtHookRestartAt atomic.Int64 // 上次重启时间（冷却用，防狂刷线程）
)

func vtHookHealth() {
	for {
		time.Sleep(2 * time.Second)
		lastCb := vtLastHookCbMs.Load()
		if lastCb == 0 {
			continue // 还没收到过任何按键，无从判断
		}
		// 系统级"最后输入时间"（键鼠都算，单位=毫秒 tick）
		var lii struct {
			CbSize uint32
			DwTime uint32
		}
		lii.CbSize = uint32(unsafe.Sizeof(lii))
		if r, _, _ := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&lii))); r == 0 {
			continue
		}
		nowTick, _, _ := procGetTickCount64.Call()
		idle := int64(uint32(nowTick)&0xFFFFFFFF) - int64(lii.DwTime)
		if idle < 0 {
			idle += 1 << 32
		}
		// 用户刚有输入（1.5 秒内），我们的回调却超过 3 秒没动静 → 钩子失效
		if idle < 1500 && time.Now().UnixMilli()-lastCb > 3000 {
			if vtHookDead.CompareAndSwap(false, true) {
				vtLog("钩子疑似失效：系统 1.5 秒内有输入，但钩子 3 秒没收到任何按键")
			}
			if vtIsRecording() {
				vtStopSession("钩子失效，强制结束本轮（避免麦克风一直开着）")
			}
			if time.Since(time.UnixMilli(vtHookRestartAt.Load())) > 5*time.Second {
				vtHookRestartAt.Store(time.Now().UnixMilli())
				vtLog("在新线程上重新安装钩子（旧线程可能已卡死）")
				go vtHookLoop()
			}
		} else if vtHookDead.CompareAndSwap(true, false) {
			vtLog("钩子恢复正常")
		}
	}
}

// vtHookWatchdog 每分钟让钩子线程重装一次钩子。
//
// WH_KEYBOARD_LL 的回调一旦超时（系统卡顿、休眠唤醒、调试器挂起）就被 Windows
// 静默摘除，不发任何通知——之后热键全部失灵，只能重启程序。重装是几微秒的纯内核
// 操作，代价极低，所以无条件定期做（SayIt 也是每分钟最多一次）。
func vtHookWatchdog() {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for range t.C {
		tid := vtHookTID
		if tid == 0 {
			continue
		}
		silent := int64(-1)
		if last := vtLastHookCbMs.Load(); last > 0 {
			silent = (time.Now().UnixMilli() - last) / 1000
		}
		procPostThreadMessageW.Call(uintptr(tid), vtWmReloadHooks, 0, 0)
		vtLog("钩子看门狗：已请求重装（上次按键回调 %d 秒前）", silent)
	}
}

// ---------- waveIn 录音会话（16k/mono/16bit，100ms×6 缓冲轮转） ----------

const (
	vtSampleRate = 16000
	vtFrameBytes = vtSampleRate * 2 / 20 // 50ms：波形更新频率（SayIt 级顺滑的底线）
	vtBufCount   = 10
)

type vtCapture struct {
	hWave    uintptr
	hdrs     [vtBufCount]tWAVEHDR
	bufs     [vtBufCount][]byte
	stopFlag atomic.Bool
	loopDone chan struct{}
	pcm      []byte // 全量 PCM（loop 结束后只读）

	gen int32 // 本轮的会话代数（悬浮窗异步回调据此判"是否已过期"）

	// ---- 实时切段器（伪流式）：能量门限状态机，切点=静音中点 ----
	segStart  int // 当前段起始字节（含 0.2s 前置余量），-1=积累中无段
	lastEmit  int // 已发送段的截止字节（尾段起点）
	speechMs  int // 当前段累计语音毫秒
	silenceMs int // 连续静音毫秒
}

// 切段器参数（规划文档 §4；门限与前端噪声门同量级）
const (
	vtSegTH    = 0.015 // 语音门限（帧 RMS）
	vtSegSilMs = 400   // 静音判停（与 python VAD 一致）
	vtSegMinMs = 800   // 最短语音段（"嗯"不单独成段）
	vtSegMaxMs = 15000 // 段上限（超时主动切，保证中途有反馈）
	vtSegPadMs = 200   // 切点余量：从静音中点切，发送范围多带此余量，
	// python VAD 在余量内自行精确定位字头字尾（防漏字关键）
)

var (
	vtMidChGlobal = make(chan vtMidSeg, 4) // 中途段 → 识别 goroutine（串行消费）
	vtPending     atomic.Int32             // 在途中途识别数（finish 等它清零）
)

// segStep 每帧（50ms）推进切段状态机；frameRms 为本帧 12 柱 RMS 最大值。
func (s *vtCapture) segStep(frameStart, frameEnd int, frameRms float32) {
	padB := vtSampleRate * 2 * vtSegPadMs / 1000
	if frameRms >= vtSegTH {
		s.silenceMs = 0
		if s.segStart < 0 {
			s.segStart = frameStart - padB // 前置余量
			if s.segStart < s.lastEmit {
				s.segStart = s.lastEmit
			}
			s.speechMs = 0
		}
		s.speechMs += 50
	} else {
		s.silenceMs += 50
	}
	if s.segStart < 0 {
		return
	}
	// 停顿成段：静音≥400ms 且语音≥800ms → 切点=静音中点（当前帧尾回退 0.2s）
	if s.silenceMs >= vtSegSilMs && s.speechMs >= vtSegMinMs {
		cut := frameEnd - padB
		s.emitSeg(s.segStart, cut)
		s.segStart, s.speechMs, s.silenceMs = -1, 0, 0
		return
	}
	// 段上限主动切（一口气说不停）：同样回退余量，新段紧接
	if s.speechMs >= vtSegMaxMs {
		cut := frameEnd - padB
		s.emitSeg(s.segStart, cut)
		s.segStart, s.speechMs, s.silenceMs = cut, 0, 0
	}
}

// emitSeg 发送一段 [a,b) 给中途识别（channel 满则丢弃该段的流字，不影响最终文本）。
func (s *vtCapture) emitSeg(a, b int) {
	minB := vtSampleRate * 2 * vtSegMinMs / 1000
	if b-a < minB {
		return
	}
	seg := append([]byte{}, s.pcm[a:b]...)
	s.lastEmit = b
	vtPending.Add(1)
	select {
	case vtMidChGlobal <- vtMidSeg{Gen: s.gen, PCM: seg}:
	default:
		vtPending.Add(-1) // 队列满：放弃中途展示（最终路径有兜底）
	}
}

// vtStartCapture 打开麦克风开始采集；波形经全局 vtBarsChGlobal 投递。
// ---------- 采集设备选择（避免抓到虚拟设备录到静音） ----------

// tWAVEINCAPSW waveInGetDevCapsW 的结构（与 waveIncaps 对齐：32 个 WCHAR 名称）
type tWAVEINCAPSW struct {
	WMid           uint16
	WPid           uint16
	VDriverVersion uint32
	SzPname        [32]uint16
	DwFormats      uint32
	WChannels      uint16
	WReserved1     uint16
}

type vtMicDev struct {
	Index uint32
	Name  string
}

// vtVirtualMicMarkers 虚拟/串流设备的关键字：默认输入落到这些设备上基本等于静音。
var vtVirtualMicMarkers = []string{"oculus", "transcreen", "virtual", "vb-audio", "voicemeeter",
	"cable output", "steam streaming", "nvidia broadcast", "虚拟", "droidcam"}

func vtMicList() []vtMicDev {
	n, _, _ := procWaveInGetNumDevs.Call()
	out := make([]vtMicDev, 0, n)
	for i := uintptr(0); i < n; i++ {
		var caps tWAVEINCAPSW
		r, _, _ := procWaveInGetDevCapsW.Call(i, uintptr(unsafe.Pointer(&caps)), unsafe.Sizeof(caps))
		if r != 0 {
			continue
		}
		out = append(out, vtMicDev{Index: uint32(i), Name: syscall.UTF16ToString(caps.SzPname[:])})
	}
	return out
}

func vtMicIsVirtual(name string) bool {
	l := strings.ToLower(name)
	for _, m := range vtVirtualMicMarkers {
		if strings.Contains(l, m) {
			return true
		}
	}
	return false
}

// vtPickMic 决定用哪个采集设备：显式配置 > 第一个非虚拟设备 > 系统默认。
func vtPickMic() (uint32, string) {
	devs := vtMicList()
	want := strings.ToLower(strings.TrimSpace(vtHotkeys().MicDevice))
	if want != "" {
		for _, d := range devs {
			if strings.Contains(strings.ToLower(d.Name), want) {
				return d.Index, d.Name
			}
		}
		vtLog("micDevice=%q 没匹配到设备，回退自动选择（可选：%s）", want, vtMicNames(devs))
	}
	for _, d := range devs {
		if !vtMicIsVirtual(d.Name) {
			return d.Index, d.Name
		}
	}
	if len(devs) > 0 {
		return devs[0].Index, devs[0].Name
	}
	return ^uint32(0), "系统默认"
}

func vtMicNames(devs []vtMicDev) string {
	parts := make([]string, 0, len(devs))
	for _, d := range devs {
		parts = append(parts, fmt.Sprintf("#%d %s", d.Index, d.Name))
	}
	return strings.Join(parts, " / ")
}

// vtMicSelftest 录 350ms 报一次电平：一眼看出"设备静音/抓错设备"。
// 真机踩过：热键、识别全正常，就是录进来 -50dBFS 的底噪 → 识别必然 0 字。
func vtMicSelftest() {
	cap, err := vtStartCapture()
	if err != nil {
		vtLog("麦克风自检失败（打不开设备）：%v", err)
		return
	}
	time.Sleep(350 * time.Millisecond)
	pcm := cap.stop()
	if len(pcm) < 3200 {
		vtLog("麦克风自检：只录到 %d 字节，设备可能没在产出数据", len(pcm))
		return
	}
	peak := 0
	for i := 0; i+1 < len(pcm); i += 2 {
		v := int(int16(uint16(pcm[i]) | uint16(pcm[i+1])<<8))
		if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
	}
	dbfs := 20 * math.Log10(float64(peak)/32768.0+1e-9)
	hint := ""
	if dbfs < -45 {
		hint = " ← 近乎静音：多半抓到了虚拟设备或麦克风被静音，可用 hotkeys.json 的 micDevice 指定设备名"
	}
	vtLog("麦克风自检：设备=%s 峰值=%.1fdBFS%s", vtMicCurName, dbfs, hint)
}

func vtStartCapture() (*vtCapture, error) {
	s := &vtCapture{loopDone: make(chan struct{})}
	fmtX := tWAVEFORMATEX{FormatTag: 1, Channels: 1, SamplesPerSec: vtSampleRate,
		AvgBytesPerSec: vtSampleRate * 2, BlockAlign: 2, BitsPerSample: 16}
	dev, name := vtPickMic()
	vtMicCurName = name
	ret, _, err := procWaveInOpen.Call(uintptr(unsafe.Pointer(&s.hWave)), uintptr(dev),
		uintptr(unsafe.Pointer(&fmtX)), 0, 0, 0)
	if ret != 0 {
		return nil, err
	}
	sz := unsafe.Sizeof(s.hdrs[0])
	for i := 0; i < vtBufCount; i++ {
		s.bufs[i] = make([]byte, vtFrameBytes)
		s.hdrs[i].LpData = uintptr(unsafe.Pointer(&s.bufs[i][0]))
		s.hdrs[i].BufferLength = vtFrameBytes
		if r, _, e := procWaveInPrepareHeader.Call(s.hWave, uintptr(unsafe.Pointer(&s.hdrs[i])), sz); r != 0 {
			return nil, e
		}
		procWaveInAddBuffer.Call(s.hWave, uintptr(unsafe.Pointer(&s.hdrs[i])), sz)
	}
	procWaveInStart.Call(s.hWave)
	go s.loop()
	return s, nil
}

func (s *vtCapture) loop() {
	defer close(s.loopDone)
	sz := unsafe.Sizeof(s.hdrs[0])
	for {
		progressed := false
		for i := range s.hdrs {
			if s.hdrs[i].Flags&whdrDone == 0 {
				continue
			}
			data := s.bufs[i][:s.hdrs[i].BytesRecorded]
			frameStart := len(s.pcm)
			s.pcm = append(s.pcm, data...)
			frameEnd := len(s.pcm)
			// 波形：12 柱 RMS（同时取帧最大值喂切段器——伪流式状态机）
			bars := vtBarsOf(data)
			var fr float32
			for _, v := range bars {
				if v > fr {
					fr = v
				}
			}
			s.segStep(frameStart, frameEnd, fr)
			select {
			case vtBarsChGlobal <- bars:
			default:
			}
			// 回收纪律：只清 DONE、保留 PREPARED（全清＝回队列被拒、音频静默中断）
			s.hdrs[i].Flags = whdrPrepared
			s.hdrs[i].BytesRecorded = 0
			if !s.stopFlag.Load() {
				procWaveInAddBuffer.Call(s.hWave, uintptr(unsafe.Pointer(&s.hdrs[i])), sz)
			}
			progressed = true
		}
		if s.stopFlag.Load() && !progressed {
			return
		}
		if !progressed {
			sleepMs(15)
		}
	}
}

// vtStopCapture 停采并等 loop 收尾，返回全量 PCM。
func (s *vtCapture) stop() []byte {
	s.stopFlag.Store(true)
	procWaveInReset.Call(s.hWave) // 在途缓冲全部标 DONE，由 loop 吃掉
	<-s.loopDone
	sz := unsafe.Sizeof(s.hdrs[0])
	for i := range s.hdrs {
		procWaveInUnprepareHeader.Call(s.hWave, uintptr(unsafe.Pointer(&s.hdrs[i])), sz)
	}
	procWaveInClose.Call(s.hWave)
	return s.pcm
}

func vtBarsOf(b []byte) []float32 {
	const N = 12 // 与前端 BARS 一致（132px 紧凑胶囊）
	out := make([]float32, N)
	step := len(b) / 2 / N
	if step == 0 {
		return out
	}
	for i := 0; i < N; i++ {
		var sum float64
		base := i * step * 2
		for j := 0; j < step; j++ {
			v := float64(int16(uint16(b[base+j*2]) | uint16(b[base+j*2+1])<<8))
			sum += v * v
		}
		rms := math.Sqrt(sum/float64(step)) / 32768
		out[i] = float32(rms) // 原始 RMS：AGC/软限幅/平滑全在前端做（照搬 SayIt waveform.ts）
	}
	return out
}

func sleepMs(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }

// ---------- 单实例 ----------

var (
	procCreateMutexW = k32.NewProc("CreateMutexW")
	vtMutexHandle    uintptr // 常驻持有：句柄一关，锁就没了
)

// vtMutexName 单实例互斥体名。默认产品名；同机跑隔离验证实例时用
// -ldflags "-X main.vtMutexName=..." 改名与正在运行的用户实例并存
//（asrPort 同理，见 main.go 的 var 块——两者只服务测试隔离，产品路径不变）。
var vtMutexName = "VoiceTypeGo_SingleInstance"

// vtSingleInstanceGuard 只允许一个实例运行。
//
// 为什么要它：两个实例＝两套 WH_KEYBOARD_LL 钩子 + 两个悬浮窗 + 两份录音，
// 症状是「同一句话被粘两遍」或「一个实例绑住了 ASR 端口 8320，另一个识别永远
// 不就绪」——都会被误当成"判定坏了"。交付给用户的产品更不能出现这种叠加。
func vtSingleInstanceGuard() bool {
	name, err := syscall.UTF16PtrFromString(vtMutexName)
	if err != nil {
		return true
	}
	h, _, callErr := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if h == 0 {
		return true // 建锁都失败：别因此拦住用户使用
	}
	vtMutexHandle = h
	const errorAlreadyExists = 183
	if errno, ok := callErr.(syscall.Errno); ok && errno == errorAlreadyExists {
		return false
	}
	return true
}

// ---------- 可编辑检测 + 注入 + 剪贴板 ----------

func vtForegroundWindow() uintptr {
	h, _, _ := procGetForegroundWindow.Call()
	return h
}

// vtForegroundIsHost 判断「当前前台窗口是不是宿主程序（数字助理）」。
// 是 → 本轮热键让路，交给宿主自己的按钮听写（避免两套听写同时上屏）。
//
// 三层判定，从最可靠到最通用：
//  1. 组件桥注入的宿主 PID（环境变量 VT_HOST_PID）：组件模式下主程序拉起 worker
//     时写入 —— 精确，且**与进程名无关，产品改名不受影响**；
//  2. 父进程 PID：主程序直接 exec 拉起 worker 的经典形态；
//  3. 进程名名单：独立运行（双击启动）时没有父子关系，只能按名字认；
//     名单来自 hotkeys.json 的 avoidHostNames，代码里只有 vtDefaultAvoidHostNames 兜底。
func vtForegroundIsHost() bool {
	fg := vtForegroundWindow()
	if fg == 0 {
		return false
	}
	var pid uint32
	procGetWindowThreadProcessId.Call(fg, uintptr(unsafe.Pointer(&pid)))
	if pid == 0 {
		return false
	}

	// 1) 组件桥注入的宿主 PID（最准）
	if hostPID := vtHostPIDFromEnv(); hostPID != 0 && pid == hostPID {
		return true
	}
	// 2) 父进程即宿主的旧规则已删除。
	//    原因：os.Getppid() 得到的 PID 在父进程退出后会被系统复用，一旦复用给
	//    别的程序（实测见过 msedge），那个程序就被误判成"宿主在前台"，于是永久让路、
	//    热键全无反应。宿主身份改由主程序显式注入 VT_HOST_PID（见 voicetype_spawn.go），
	//    再配合下面的进程名名单，两条都不受 PID 复用影响。
	// 3) 按进程名名单（独立运行走这里；改名只需改配置）
	name := vtProcessName(pid)
	if name == "" {
		return false
	}
	for _, h := range vtAvoidHostNames() {
		if strings.EqualFold(strings.TrimSpace(h), name) {
			return true
		}
	}
	return false
}

// vtHostPIDFromEnv 读组件桥注入的宿主 PID（未注入或格式非法返回 0）。
func vtHostPIDFromEnv() uint32 {
	v := strings.TrimSpace(os.Getenv("VT_HOST_PID"))
	if v == "" {
		return 0
	}
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		return 0
	}
	return uint32(n)
}

// vtProcessName 取进程可执行文件名（如 "AI数字助理.exe"），失败返回空串。
func vtProcessName(pid uint32) string {
	const processQueryLimitedInformation = 0x1000
	h, _, _ := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if h == 0 {
		return ""
	}
	defer procCloseHandle.Call(h)
	buf := make([]uint16, 1024)
	size := uint32(len(buf))
	r, _, _ := procQueryFullProcessImageNameW.Call(h, 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if r == 0 {
		return ""
	}
	return filepath.Base(syscall.UTF16ToString(buf[:size]))
}

// vtAvoidHostNames 生效的宿主进程名名单：hotkeys.json 配了就用它，否则用默认名单。
func vtAvoidHostNames() []string {
	vtHKMu.RLock()
	defer vtHKMu.RUnlock()
	if len(vtHK.AvoidHostNames) > 0 {
		return vtHK.AvoidHostNames
	}
	return vtDefaultAvoidHostNames
}

// vtAvoidHostEnabled 退避总开关（hotkeys.json 的 avoidHost，缺省开）。
func vtAvoidHostEnabled() bool {
	vtHKMu.RLock()
	defer vtHKMu.RUnlock()
	if vtHK.AvoidHost == nil {
		return true
	}
	return *vtHK.AvoidHost
}

func vtWindowClass(hwnd uintptr) string {
	buf := make([]uint16, 256)
	n, _, _ := procGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), 256)
	return syscall.UTF16ToString(buf[:n])
}

// vtEditableAt 的历史实现已删除：可编辑判定统一走 vt_target_windows.go 的
// vtEditabilityGate（caret/原生类名/UIA/进程兜底四层）。这里曾经有一份
// 「类名前缀白名单」的粗判，把 Chrome_WidgetWin_*、webview、ATL: 一律当可编辑，
// 结果「没有输入框也 Ctrl+V」——判定逻辑只能有一份，别再在这里加回来。

func vtClipboardSetText(text string) bool {
	utf16, _ := syscall.UTF16FromString(text)
	size := (len(utf16)) * 2
	h, _, _ := procGlobalAlloc.Call(gmemMoveable, uintptr(size))
	if h == 0 {
		return false
	}
	p, _, _ := procGlobalLock.Call(h)
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(p)), len(utf16)), utf16)
	procGlobalUnlock.Call(h)
	return vtClipboardSetHandle(h)
}

// vtClipboardSeq 剪贴板序号（每次内容变化 system 自增）。用来判断"我们写入之后
// 有没有别人改过剪贴板"，从而决定要不要恢复原内容。
func vtClipboardSeq() uint32 {
	r, _, _ := procGetClipboardSeq.Call()
	return uint32(r)
}

func vtClipboardSetHandle(h uintptr) bool {
	for i := 0; i < 5; i++ {
		if r, _, _ := procOpenClipboard.Call(0); r != 0 {
			procEmptyClipboard.Call()
			procSetClipboardData.Call(cfUnicodeText, h) // 成功后系统接管 h，勿再释放
			procCloseClipboard.Call()
			return true
		}
		sleepMs(10)
	}
	return false
}

// vtClipboardGetText 读回剪贴板文本（注入前保存原内容，粘贴后恢复——
// 用户要求：能成功上屏的结果不留在剪贴板里）。
func vtClipboardGetText() (string, bool) {
	procGlobalSize := k32.NewProc("GlobalSize")
	for i := 0; i < 5; i++ {
		if r, _, _ := procOpenClipboard.Call(0); r != 0 {
			h, _, _ := procGetClipboardData.Call(cfUnicodeText)
			var out string
			if h != 0 {
				if p, _, _ := procGlobalLock.Call(h); p != 0 {
					// 用 GlobalSize 定界，绝不越界读（越界读进未提交页=直接崩）
					sz, _, _ := procGlobalSize.Call(h)
					n := int(sz) / 2
					if n > 0 {
						u16 := unsafe.Slice((*uint16)(unsafe.Pointer(p)), n)
						out = syscall.UTF16ToString(u16)
					}
					procGlobalUnlock.Call(h)
				}
			}
			procCloseClipboard.Call()
			return out, h != 0
		}
		sleepMs(10)
	}
	return "", false
}

// vtSendCtrlV 四事件序列一次 SendInput 提交。
// INPUT 布局(x64)：type@0 wVk@8 wScan@10 dwFlags@12 time@16 dwExtraInfo@24
// ——dwFlags 写错位置＝抬起变按下，Ctrl 会永远卡在按下状态（实测坑）。
func vtSendCtrlV() {
	const (
		inputKeyboard = 1
		keyeventfUp   = 0x0002
		vkControl     = 0x11
		vkV           = 0x56
	)
	mk := func(vk uint16, flags uint16) []byte {
		b := make([]byte, 40)
		binary.LittleEndian.PutUint32(b, inputKeyboard)
		binary.LittleEndian.PutUint16(b[8:], vk)
		// wScan 也填上（同 vtSendKeyEvent 的教训：缺扫描码时状态不干净）
		if r, _, _ := procMapVirtualKeyW.Call(uintptr(vk), 0); r != 0 {
			binary.LittleEndian.PutUint16(b[10:], uint16(r))
		}
		binary.LittleEndian.PutUint16(b[12:], flags)
		return b
	}
	var seq []byte
	seq = append(seq, mk(vkControl, 0)...)
	seq = append(seq, mk(vkV, 0)...)
	seq = append(seq, mk(vkV, keyeventfUp)...)
	seq = append(seq, mk(vkControl, keyeventfUp)...)
	procSendInput.Call(4, uintptr(unsafe.Pointer(&seq[0])), 40)
}

// vtSendKeyEvent 发一个按键事件（down / up 可分别选择）。
//
// 为什么不用 keybd_event：SendInput 一次提交、原子性好；带上的 INJECTED 标志会被
// 我们自己的钩子过滤掉，所以"回放/还原"不会自我递归。
func vtSendKeyEvent(vk uint32, down, up bool) {
	if vk == 0 || (!down && !up) {
		return
	}
	const (
		inputKeyboard = 1
		keyeventfUp   = 0x0002
		keyeventfExt  = 0x0001 // 右 Alt/右 Ctrl/方向键等扩展键必须带，否则会变成另一枚键
	)
	// wScan 必须填！实测教训：wScan=0 的注入事件**清不掉修饰键状态**——
	// 左 Alt 卡住时发一次"没有扫描码的抬起"完全无效，补上 MapVirtualKey 的扫描码
	// 后立刻恢复（True→False）。前端程序里也有不少只认扫描码的老控件。
	scan := uint16(0)
	if r, _, _ := procMapVirtualKeyW.Call(uintptr(vk), 0); r != 0 {
		scan = uint16(r)
	}
	mk := func(flags uint16) []byte {
		b := make([]byte, 40)
		binary.LittleEndian.PutUint32(b, inputKeyboard)
		binary.LittleEndian.PutUint16(b[8:], uint16(vk)) // wVk
		binary.LittleEndian.PutUint16(b[10:], scan)      // wScan（原来漏了）
		binary.LittleEndian.PutUint16(b[12:], flags)     // dwFlags
		return b
	}
	ext := uint16(0)
	switch vk {
	case 0xA3, 0xA5, 0x5C, 0x2D, 0x2E, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, 0x90, 0x91:
		ext = keyeventfExt
	}
	var seq []byte
	n := uintptr(0)
	if down {
		seq = append(seq, mk(ext)...)
		n++
	}
	if up {
		seq = append(seq, mk(ext|keyeventfUp)...)
		n++
	}
	procSendInput.Call(n, uintptr(unsafe.Pointer(&seq[0])), 40)
}

// vtSendKeyTap 把一次按键原样回放给前台程序（点按没达到长按阈值时用）。
// 热键被我们吞掉后，用户"点一下 Alt"这类原生操作就靠它还原。
func vtSendKeyTap(vk uint32) { vtSendKeyEvent(vk, true, true) }

// ---------- 悬浮窗（jchv 库窗口改造而来，见 vtRestyleOverlay） ----------

// 悬浮窗几何：窗口恒定 200×88（WebView bounds 恒定 → HTML 布局永远居中，
// 不动态 resize——动态改尺寸时 WebView 内容区跟不上，气泡/文字会错位、
// 露窗口背景，实测坑）。可见区完全由 region 控制（region 外窗口不存在、
// 点击自然穿透）：平时只有胶囊，气泡/无声提示时 region 并集扩出上方元素。
const (
	vtWinW  = 200
	vtWinH  = 88
	vtPillW = 132 // 胶囊：x34~166 y52~88（36 高，沉底居中）
	vtPillH = 36
	vtPillX = 34
	vtPillY = 52
	vtBubX  = 5 // 气泡：x5~195 y8~48（190×40，流字与无声提示共用）
	vtBubY  = 8
	vtBubW  = 190
	vtBubH  = 40
)

const vtOverlayH = 36

var vtOverlayHwnd uintptr

// vtMicCurName 最近一次打开采集设备的名字（日志/自检用）。
var vtMicCurName = "未打开"

// vtRestyleOverlay 把 jchv 库自建的 640x480 普通窗口（NewWindow 的 window
// 参数被库忽略、它自己 CreateWindowExW+ShowWindow+SetFocus）改造成胶囊：
// 去边框、不抢焦点、置顶、不进任务栏、圆角、隐藏待用。
func vtRestyleOverlay(hwnd uintptr) {
	procSetWindowLongW = u32.NewProc("SetWindowLongW")
	// GWL_STYLE=-16 GWL_EXSTYLE=-20
	procSetWindowLongW.Call(hwnd, ^uintptr(15), wsPopup) // style = WS_POPUP（去标题栏边框）
	procSetWindowLongW.Call(hwnd, ^uintptr(19), wsExTopMost|wsExNoActivate|wsExToolWindow)
	vtForceOverlayGeom(hwnd, false)
	procShowWindow.Call(hwnd, swHide) // 初始隐藏，录音时才显示
	vtOverlayHwnd = hwnd
}

// vtForceOverlayGeom 窗口几何恒定 200×88（工作区底部居中，距底 72px）。
func vtForceOverlayGeom(hwnd uintptr, show bool) {
	x, y := vtOverlayPos()
	flags := uintptr(swpNoActivate | swpShowWindow)
	if !show {
		flags = uintptr(swpNoActivate | 0x0020)
	}
	r1, _, err := procSetWindowPos.Call(hwnd, 0, uintptr(x), uintptr(y), vtWinW, vtWinH, flags)
	vtLog("geom矫正 show=%v ret=%d err=%v -> %s", show, r1, err, vtWindowRectStr(hwnd))
}

// vtWindowRectStr 读窗口实际矩形（诊断几何是否被覆盖）。
func vtWindowRectStr(hwnd uintptr) string {
	var rc rect
	procGetWindowRect := u32.NewProc("GetWindowRect")
	r, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	if r == 0 {
		return "GetWindowRect失败"
	}
	return fmt.Sprintf("rect(%d,%d %dx%d)", rc.L, rc.T, rc.R-rc.L, rc.B-rc.T)
}

var procSetWindowLongW *syscall.LazyProc

func vtApplyCapsuleRgn(hwnd uintptr) {
	// 兼容入口：等价于仅胶囊模式
	vtOverlayModeRegion(0)
}

// vtOverlayPos 窗口（恒定 200×88）位置：工作区底部水平居中、距底 72px。
// 胶囊位于窗口底部（y52~88），视觉中心仍是胶囊。
func vtOverlayPos() (int, int) {
	var wa rect
	procSystemParametersInfoW.Call(spiGetWorkArea, 0, uintptr(unsafe.Pointer(&wa)), 0)
	x := int(wa.L) + (int(wa.R-wa.L)-vtWinW)/2
	y := int(wa.B) - vtWinH - 72
	return x, y
}

// vtOverlayShow 显示悬浮窗。只重建几何与 region，不动尺寸（避免 resize 黑边）。
func vtOverlayShow() {
	if vtOverlayHwnd == 0 {
		return
	}
	vtForceOverlayGeom(vtOverlayHwnd, true)
	vtOverlayModeRegion(0)
	// 把 z 序再顶一次：首次呼出时窗口已经"显示"了，但偶尔会被浏览器窗口压住，
	// 过一两百毫秒才浮上来（用户现象："先被浏览器挡住一会儿，然后才正式出来"）。
	// 只调 z 序，不改大小/位置、不抢焦点，所以不会打断用户正在打字。
	go func() {
		for _, d := range []time.Duration{40 * time.Millisecond, 140 * time.Millisecond} {
			time.Sleep(d)
			vtOverlayRaise()
		}
	}()
}

// vtOverlayRaise 把悬浮窗顶到最前（HWND_TOPMOST），不动几何、不激活。
func vtOverlayRaise() {
	if vtOverlayHwnd == 0 {
		return
	}
	const (
		hwndTopmost       = ^uintptr(0) // (HWND)-1
		swpNoSize         = 0x0001
		swpNoMove         = 0x0002
		swpNoActivateFlag = 0x0010
	)
	procSetWindowPos.Call(vtOverlayHwnd, hwndTopmost, 0, 0, 0, 0,
		uintptr(swpNoSize|swpNoMove|swpNoActivateFlag))
}

func vtOverlayHide() {
	if vtOverlayHwnd == 0 {
		return
	}
	procShowWindow.Call(vtOverlayHwnd, swHide)
	vtOverlayModeRegion(0) // 复位仅胶囊
}

// vtOverlayWiden 可见区切换：0=仅胶囊 1=胶囊+气泡（流字与无声提示共用）。
// 窗口尺寸/位置恒定不动，只换 region（无 resize、无 bounds 滞后、无黑边）。
func vtOverlayWiden(mode int) {
	if vtOverlayHwnd == 0 {
		return
	}
	vtOverlayModeRegion(mode)
}

// vtOverlayModeRegion 按模式设置窗口 region（圆角矩形并集，坐标见常量表）。
func vtOverlayModeRegion(mode int) {
	dll := syscall.NewLazyDLL("gdi32.dll")
	combine := dll.NewProc("CombineRgn")
	rr := dll.NewProc("CreateRoundRectRgn")
	py := vtWinH - vtPillH
	rPill, _, _ := rr.Call(vtPillX, uintptr(py), uintptr(vtPillX+vtPillW), vtWinH, vtPillH, vtPillH)
	rgn := rPill
	if mode == 1 {
		rBub, _, _ := rr.Call(vtBubX, vtBubY, uintptr(vtBubX+vtBubW), uintptr(vtBubY+vtBubH), 14, 14)
		out, _, _ := dll.NewProc("CreateRectRgn").Call(0, 0, 0, 0)
		combine.Call(out, rPill, rBub, 2 /*RGN_OR*/)
		rgn = out
	}
	procSetWindowRgn.Call(vtOverlayHwnd, rgn, 1)
}

// vtModifierSnapshot 逻辑修饰键状态 + 内部账本，一条日志写完（排障用）。
// 触发：目录里放 trigger.txt 内容 `state`。
func vtModifierSnapshot() {
	type m struct {
		n  string
		vk uint32
	}
	mods := []m{{"lalt", 0xA4}, {"ralt", 0xA5}, {"lctrl", 0xA2}, {"rctrl", 0xA3},
		{"lshift", 0xA0}, {"rshift", 0xA1}, {"ctrl(通用)", 0x11}, {"alt(通用)", 0x12}}
	var parts []string
	for _, it := range mods {
		if vtAsyncKeyDown(it.vk) {
			parts = append(parts, it.n)
		}
	}
	vtLog("状态快照: 按住中的修饰键=[%s] consumed=%v injected=%v passthrough=%v arming=%v armVK=%#x hotVKDown=%#x 录音=%v",
		strings.Join(parts, ","), vtConsumed, vtInjected, vtPassthrough,
		vtArming.Load(), vtArmVK.Load(), vtHotVKDown.Load(), vtIsRecording())
}

// vtInjectFence 注入按键的"状态围栏"：保证我们注入的键**绝不留在按下状态**。
//
// 这不是补丁，是注入模块本身该有的正确性保证：SendInput 一次提交四个事件
// （Ctrl↓ V↓ V↑ Ctrl↑），如果目标程序正忙、或其中某个事件被系统丢弃，
// 残留的修饰键会污染用户接下来的所有按键（典型症状：之后按 Ctrl+V 粘不出来）。
//
// 用法：注入前用 vtAsyncKeyDown 记下物理状态，注入后调用本函数——
// 只有当"注入前没按、现在却按着"时才补一次抬起（带扫描码），
// 用户自己按着的不碰（否则会把人家真实的按键吃掉）。
func vtInjectFence(preCtrl, preV bool) {
	if !preCtrl {
		for _, vk := range []uint32{0xA2, 0xA3, 0x11} { // 左Ctrl/右Ctrl/通用Ctrl
			if vtAsyncKeyDown(vk) {
				vtSendKeyEvent(vk, false, true)
				vtLog("注入围栏：补抬起 Ctrl(vk=%#x)，防止残留污染后续按键", vk)
			}
		}
	}
	if !preV && vtAsyncKeyDown(0x56) {
		vtSendKeyEvent(0x56, false, true)
		vtLog("注入围栏：补抬起 V，防止残留")
	}
}
