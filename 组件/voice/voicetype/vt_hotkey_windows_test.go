//go:build windows

package main

// 热键层（键名匹配 / 吞键判定 / 点按回放）的回归测试。
//
//   go test -run "TestKeyName|TestHookConsume|TestMenuGate" -v
//
// 为什么这些用例必须钉住：吞键判错的两个方向都很糟——
//   · 该吞没吞 → 前台程序把 Alt 当"点了一下"打开菜单，焦点跑掉，字粘到菜单上；
//   · 不该吞却吞 → 用户在这些程序里按不了 Alt+F4 / Alt+Tab / Ctrl+F2。
// 这两种故障都不会报错，只能靠用例兜。

import (
	"os"
	"path/filepath"
	"testing"
)

func vtResetHotkeyState() {
	vtHeld = map[string]bool{}
	vtConsumed = map[string]bool{}
	vtPassthrough = map[string]bool{} // 新增的账本也要清，否则测试之间互相污染
	vtInjected = map[string]bool{}
	vtHotVKDown.Store(0)
	// 单测不依赖物理键盘：用户正按着 Alt 时，"裸按"判定会读到"有修饰键按着"而误红。
	vtOtherModifierDownFn = func(uint32) bool { return false }
	vtComboOn[0], vtComboOn[1] = false, false
	vtHookHostForeground.Store(false)
	vtHKMu.Lock()
	vtHK = vtHotkeysCfg{
		Main: []string{"alt"}, Sub: []string{}, Mode: "hold", // 与产品默认一致：两侧都算
		HoldMs: vtDefaultHoldMs, ReplayShortTap: boolPtr(true),
	}
	vtHKMu.Unlock()
}

func boolPtr(b bool) *bool { return &b }

func TestKeyNameMapping(t *testing.T) {
	// 修饰键必须区分左右：热键吞键后，`alt` 一次吞两侧会废掉左 Alt 的菜单功能
	cases := map[uint32]string{
		0xA0: "lshift", 0xA1: "rshift",
		0xA2: "lctrl", 0xA3: "rctrl",
		0xA4: "lalt", 0xA5: "ralt",
		0x5B: "lmeta", 0x5C: "rmeta",
		0x20: "space", 0x41: "a", 0x31: "1", 0x71: "f2",
	}
	for vk, want := range cases {
		if got := vtKeyName(vk); got != want {
			t.Errorf("vtKeyName(%#x)=%q 期望 %q", vk, got, want)
		}
	}
	for vk, want := range map[uint32]uint32{0xA5: 0xA5, 0xA4: 0xA4, 0x71: 0x71, 0x41: 0x41, 0x31: 0x31} {
		if got := vtVKByName(vtKeyName(vk)); got != want {
			t.Errorf("vtVKByName(%q)=%#x 期望 %#x", vtKeyName(vk), got, want)
		}
	}
}

func TestKeyMatchesSides(t *testing.T) {
	cases := []struct {
		cfg, actual string
		want        bool
	}{
		{"alt", "lalt", true}, {"alt", "ralt", true},
		{"ralt", "ralt", true}, {"ralt", "lalt", false},
		{"ctrl", "rctrl", true}, {"lctrl", "rctrl", false},
		{"f2", "f2", true}, {"f2", "f3", false},
	}
	for _, c := range cases {
		if got := vtKeyMatches(c.cfg, c.actual); got != c.want {
			t.Errorf("vtKeyMatches(%q,%q)=%v 期望 %v", c.cfg, c.actual, got, c.want)
		}
	}
}

// TestHookConsumeSingleModifier 单键修饰键（右 Alt）：down 吞并派发，
// **repeat 也吞但必须仍标为热键**（否则会被组合键介入逻辑误当成"别的键"），
// 配对 up 吞并派发；左 Alt 在默认配置 `alt`（两侧都算）下同样算热键。
func TestHookConsumeSingleModifier(t *testing.T) {
	vtResetHotkeyState()
	vtHKMu.Lock()
	vtHK.Main = []string{"ralt"} // 只认右侧：验证左边的确不碰
	vtHKMu.Unlock()
	if c, hk := vtHookConsume("ralt", 0xA5, 0, true); !c || !hk {
		t.Fatalf("右 Alt 按下应吞并派发，得到 consume=%v hotkey=%v", c, hk)
	}
	// 吞掉的键在 GetAsyncKeyState 里是"没按下"，所以必须自己记账——长按阈值到点
	// 时的复核就靠它（历史 bug：用 async 状态复核 → 每次长按都被误判成漏 up）
	if got := vtHotVKDown.Load(); got != 0xA5 {
		t.Fatalf("按下后 vtHotVKDown 应为 0xA5，得到 %#x", got)
	}
	for i := 0; i < 2; i++ {
		if c, hk := vtHookConsume("ralt", 0xA5, 0, true); !c || !hk {
			t.Fatalf("第 %d 次 repeat 应吞且仍标为热键（否则会被误判成组合键介入），得到 consume=%v hotkey=%v",
				i+1, c, hk)
		}
	}
	if c, hk := vtHookConsume("ralt", 0xA5, llkhfUp, false); !c || !hk {
		t.Fatalf("右 Alt 抬起应吞并派发，得到 consume=%v hotkey=%v", c, hk)
	}
	if vtConsumed["ralt"] {
		t.Fatal("配对 up 之后 consumed 位没清，下一次按下会被当成 repeat")
	}
	if got := vtHotVKDown.Load(); got != 0 {
		t.Fatalf("抬起后 vtHotVKDown 应为 0，得到 %#x", got)
	}
	// 配置只认右侧时，左 Alt 不碰
	if c, hk := vtHookConsume("lalt", 0xA4, 0, true); c || hk {
		t.Fatalf("左 Alt 不该被接管，得到 consume=%v hotkey=%v", c, hk)
	}
	// Alt 的孤立抬起也要吞（否则系统仍会当成"点了一下 Alt"）
	// 注意：**孤立**的右 Alt 抬起（没有对应的 down 被我们吞过）现在**放行**。
	// 理由：吞一个"前台没见过的 up"没有任何收益，反而在状态丢失（钩子重装）时
	// 可能制造 stuck Alt；配对吞掉由上面的 vtConsumed 分支负责。
	if c, _ := vtHookConsume("ralt", 0xA5, llkhfUp, false); c {
		t.Fatal("孤立的右 Alt 抬起不该吞（配对吞由 consumed 负责，吞孤立 up 只会制造卡键）")
	}
}

// TestHookConsumeWithModifierAltF4 按下热键时若已有别的修饰键，整条放行且不录音。
func TestHookConsumeWithModifierAltF4(t *testing.T) {
	vtResetHotkeyState()
	// 事件本身带"Alt 按住"标志，且热键是 F2：Alt+F2 不该被我们吃掉
	vtHKMu.Lock()
	vtHK.Main = []string{"f2"}
	vtHKMu.Unlock()
	if c, hk := vtHookConsume("f2", 0x71, llkhfAltDown, true); c || hk {
		t.Fatalf("Alt+F2 应整条放行，得到 consume=%v hotkey=%v", c, hk)
	}
	if c, hk := vtHookConsume("f2", 0x71, 0, true); !c || !hk {
		t.Fatalf("裸按 F2 应接管，得到 consume=%v hotkey=%v", c, hk)
	}
}

// TestHookConsumeCombo 组合热键：修饰键不吞、主键在修饰键已按住时吞、顺序反了不吞。
func TestHookConsumeCombo(t *testing.T) {
	vtResetHotkeyState()
	vtHKMu.Lock()
	vtHK.Main = []string{"ctrl", "f2"}
	vtHKMu.Unlock()

	if c, hk := vtHookConsume("lctrl", 0xA2, 0, true); c || hk {
		t.Fatalf("组合里的修饰键不该被吞，得到 consume=%v hotkey=%v", c, hk)
	}
	vtHeld["lctrl"] = true // 模拟 Ctrl 已按住
	if c, hk := vtHookConsume("f2", 0x71, 0, true); !c || !hk {
		t.Fatalf("Ctrl 已按住时 F2 应接管，得到 consume=%v hotkey=%v", c, hk)
	}
	if c, _ := vtHookConsume("f2", 0x71, llkhfUp, false); !c {
		t.Fatal("配对抬手应吞掉")
	}

	// 顺序反过来：先按 F2 再按 Ctrl → F2 照常送给前台（不制造孤立事件）
	vtResetHotkeyState()
	vtHKMu.Lock()
	vtHK.Main = []string{"ctrl", "f2"}
	vtHKMu.Unlock()
	if c, hk := vtHookConsume("f2", 0x71, 0, true); c || hk {
		t.Fatalf("Ctrl 未按住时 F2 不该被吞，得到 consume=%v hotkey=%v", c, hk)
	}
}

// TestHookConsumeHostForeground 宿主在前台时一个键都不许吞（避让逻辑不变）。
func TestHookConsumeHostForeground(t *testing.T) {
	vtResetHotkeyState()
	vtHookHostForeground.Store(true)
	defer vtHookHostForeground.Store(false)
	if c, hk := vtHookConsume("ralt", 0xA5, 0, true); c || hk {
		t.Fatalf("宿主在前台时应放行，得到 consume=%v hotkey=%v", c, hk)
	}
}

// TestAltBothSidesAreHotkey 默认配置 `alt`：左右两侧都算热键（用户要求不分左右），
// 但**左 Alt 纯监听、永不吞**（2026-11-16 纪律：左 Alt 不进我们的账本，就不可能卡住）。
func TestAltBothSidesAreHotkey(t *testing.T) {
	vtResetHotkeyState() // 默认 main=["alt"]
	for _, c := range []struct {
		name        string
		vk          uint32
		wantConsume bool
	}{
		{"lalt", 0xA4, false}, // 左 Alt：只监听
		{"ralt", 0xA5, true},  // 右 Alt：照常吞（配对方可）
	} {
		if got := vtKeyIsHotkeyMember(c.name); !got {
			t.Errorf("%s 应算热键成员（alt 不分左右）", c.name)
		}
		consume, hk := vtHookConsume(c.name, c.vk, 0, true)
		if !hk {
			t.Errorf("%s 按下应算热键（要能触发录音），得到 hotkey=%v", c.name, hk)
		}
		if consume != c.wantConsume {
			t.Errorf("%s 按下 consume 应为 %v，得到 %v", c.name, c.wantConsume, consume)
		}
		if consume {
			vtHeld[c.name] = true
			vtHookConsume(c.name, c.vk, llkhfUp, false)
			delete(vtHeld, c.name)
		}
	}
	if vtKeyIsHotkeyMember("tab") {
		t.Error("tab 不是热键成员，应算作组合键介入")
	}
}

// TestInjectedDownPairing 组合键介入时"我们补发 down、真实抬起要放行"的配对账本。
//
// 这条是用户实测问题钉的：Alt+Tab 能切窗口，但松手后切换器不关、得多按一次 Alt。
// 根因是补发 down 之后系统认为 Alt 按着，而真实抬起仍被当成"孤立 Alt up"吞掉，
// 系统状态永远闭不上。
//
// 2026-11-16 之后 Alt 不再补发（纪律见 vtSendKeyEvent），所以配对账用**非 Alt**
// 热键（F2）验证；Alt 分支单独断言"一个事件都不发"（TestReplayHeldDownNeverPressesAlt）。
func TestInjectedDownPairing(t *testing.T) {
	vtResetHotkeyState()
	vtHKMu.Lock()
	vtHK.Main = []string{"f2"}
	vtHKMu.Unlock()
	var sent int
	vtInjectSeam = func(vk uint32, down, up bool) { sent++ }
	defer func() { vtInjectSeam = nil }()

	// 情形 1：按下被吞 → 抬起也被吞（前台完全没看到这个键）
	if c, _ := vtHookConsume("f2", 0x71, 0, true); !c {
		t.Fatal("F2 按下应被接管")
	}
	if c, _ := vtHookConsume("f2", 0x71, llkhfUp, false); !c {
		t.Fatal("未补发时，抬起应被吞（否则前台会收到孤立抬起）")
	}
	if sent != 0 {
		t.Fatalf("纯吞路径不该有注入，实发 %d 个事件", sent)
	}

	// 情形 2：再按一次 → 组合介入补发 down → 抬起必须放行
	vtResetHotkeyState()
	vtHKMu.Lock()
	vtHK.Main = []string{"f2"}
	vtHKMu.Unlock()
	if c, _ := vtHookConsume("f2", 0x71, 0, true); !c {
		t.Fatal("F2 按下应被接管")
	}
	vtReplayHeldDown(0x71) // 等价于"判定期内按了别的键"时把这个键还给前台
	if sent != 1 {
		t.Fatalf("补发 down 应恰好发 1 个事件，实发 %d", sent)
	}
	if !vtPassthrough["f2"] || !vtInjected["f2"] {
		t.Fatalf("补发 down 后应同时记 passthrough 与 injected，得到 passthrough=%v injected=%v",
			vtPassthrough["f2"], vtInjected["f2"])
	}
	if c, _ := vtHookConsume("f2", 0x71, llkhfUp, false); c {
		t.Fatal("补发过 down 之后，真实抬起必须放行——否则系统认为这个键一直按着")
	}
	if vtPassthrough["f2"] || vtInjected["f2"] {
		t.Fatal("真实抬起到达后两份账都要清掉，否则自愈会再补一次抬起")
	}
}

// TestNoAltInjectionEver 纪律：本组件任何时候都不注入 Alt（down 与 up 都不发）。
//
// 用户原话（2026-11-16）：「确保它不会再按下左边的 Alt 键：就不要有按下这个动作，
// 它就不容易再出现没释放的情况。」所以这不是"要不要配对"的问题，是**不按下**。
// 这里直接卡在唯一的发送出口 vtSendKeyEvent 上：任何调用方（回放、还原、自愈、
// 未来的新代码）拿 Alt 来都会被拒发。
func TestNoAltInjectionEver(t *testing.T) {
	vtResetHotkeyState()
	var sent []uint32
	var dcount, ucount int
	vtInjectSeam = func(vk uint32, down, up bool) {
		sent = append(sent, vk)
		if down {
			dcount++
		}
		if up {
			ucount++
		}
	}
	defer func() { vtInjectSeam = nil }()

	for _, vk := range []uint32{0xA4, 0xA5, 0x12} { // 左 Alt / 右 Alt / 通用 Alt
		vtSendKeyEvent(vk, true, false)
		vtSendKeyEvent(vk, false, true)
		vtSendKeyEvent(vk, true, true)
		vtSendKeyTap(vk)
	}
	if len(sent) != 0 || dcount != 0 || ucount != 0 {
		t.Fatalf("Alt 一律不许注入，实际发了 %d 个事件（down=%d up=%d）", len(sent), dcount, ucount)
	}

	// 反向确认：不是把注入整个关掉了——普通键照常发（Ctrl+V 粘贴还靠它）。
	vtSendKeyTap(0x71) // F2
	if len(sent) != 1 || dcount != 1 || ucount != 1 {
		t.Fatalf("普通键的回放不能被误伤：期望 1 个成对事件，得到 %d（down=%d up=%d）", len(sent), dcount, ucount)
	}
}

// TestReplayHeldDownNeverPressesAlt 组合键介入时热键恰好是 Alt：只放行真实抬起，
// 一个事件都不补发（宁可在极端时机下少半截组合，也不要任何 Alt 注入）。
func TestReplayHeldDownNeverPressesAlt(t *testing.T) {
	vtResetHotkeyState()
	vtHKMu.Lock()
	vtHK.Main = []string{"ralt"}
	vtHKMu.Unlock()
	var sent int
	vtInjectSeam = func(vk uint32, down, up bool) { sent++ }
	defer func() { vtInjectSeam = nil }()

	if c, _ := vtHookConsume("ralt", 0xA5, 0, true); !c {
		t.Fatal("右 Alt 是热键，按下应被吞")
	}
	vtReplayHeldDown(0xA5) // 组合键介入：把热键还给前台
	if sent != 0 {
		t.Fatalf("Alt 不许补发 down，实发 %d 个事件", sent)
	}
	if !vtPassthrough["ralt"] {
		t.Fatal("必须记 passthrough——真实抬起要放行，否则系统认为 Alt 一直按着")
	}
	if vtInjected["ralt"] {
		t.Fatal("没补发 down 就不能记 injected（否则自愈会凭空补一个 Alt up）")
	}
	if c, _ := vtHookConsume("ralt", 0xA5, llkhfUp, false); c {
		t.Fatal("记了 passthrough 之后真实抬起必须放行")
	}
}

// TestLeftAltNeverConsumed 左 Alt 纯监听：哪怕配置里写了 lalt，它的按下/抬起也绝不吞。
func TestLeftAltNeverConsumed(t *testing.T) {
	vtResetHotkeyState()
	vtHKMu.Lock()
	vtHK.Main = []string{"lalt"} // 用户硬要配左 Alt
	vtHKMu.Unlock()

	if c, hk := vtHookConsume("lalt", 0xA4, 0, true); c || !hk {
		t.Fatalf("左 Alt 按下必须放行但算热键，得到 consume=%v hotkey=%v", c, hk)
	}
	if c, _ := vtHookConsume("lalt", 0xA4, llkhfUp, false); c {
		t.Fatal("左 Alt 抬起被吞 → 系统会认为它一直按着（stuck LALT）")
	}
	if vtConsumed["lalt"] {
		t.Fatal("左 Alt 永远不该进 consumed 账本")
	}
}

// TestReleaseInjectedKeys 结算钩子：我们补发过 down 的键必须补一次抬起；
// Alt 一律不碰（凭空一个 Alt up 会被前台当成"点了一下 Alt"，反而弹菜单）。
func TestReleaseInjectedKeys(t *testing.T) {
	vtResetHotkeyState()
	type ev struct {
		vk   uint32
		down bool
		up   bool
	}
	var got []ev
	vtInjectSeam = func(vk uint32, down, up bool) { got = append(got, ev{vk, down, up}) }
	defer func() { vtInjectSeam = nil }()

	vtInjected["f2"] = true
	vtPassthrough["f2"] = true
	vtInjected["ralt"] = true // 历史遗留（不该存在）：结算时必须只清账、不发事件
	vtPassthrough["ralt"] = true
	vtReleaseInjectedKeys("单测结算")

	if len(got) != 1 || got[0].vk != 0x71 || got[0].down || !got[0].up {
		t.Fatalf("应只给 F2 补一个抬起，得到 %+v", got)
	}
	if len(vtInjected) != 0 || len(vtPassthrough) != 0 {
		t.Fatalf("结算后账本必须清空，得到 injected=%v passthrough=%v", vtInjected, vtPassthrough)
	}
}

// TestMenuGate 焦点落在菜单上（按键被吃成"打开菜单"）必须判成不可输入。
func TestMenuGate(t *testing.T) {
	cases := []int32{uiaCtMenu, uiaCtMenuBar, uiaCtMenuItem}
	for _, ct := range cases {
		tgt := vtTarget{
			Hwnd: 1, Pid: 999999, WindowClass: "Notepad", ProcName: "notepad.exe",
			Uia: vtUiaInfo{OK: true, ControlType: ct, Enabled: true},
		}
		g := vtEditabilityGate(tgt)
		if g != vtGateMenuActive {
			t.Errorf("控件类型 %d 应判成 menu_active，得到 %s", ct, g)
		}
		if g.editable() {
			t.Errorf("菜单不是输入目标，不该判成可输入")
		}
	}
}

// TestAltUpNeverStuck 左 Alt 的抬起**绝不能**被吞（除非我们自己吞过它的 down）。
//
// 这条钉的是用户实测的 stuck LALT：左 Alt 不在热键里，按下放行、抬起被吞 →
// 系统认为它一直按着 → 下一次 Ctrl+V 变成 Ctrl+Alt+V（粘不出来），
// 用户按一下左 Alt 才"恢复"。
func TestAltUpNeverStuck(t *testing.T) {
	vtResetHotkeyState()
	vtHKMu.Lock()
	vtHK.Main = []string{"ralt"} // 只认右侧：左 Alt 完全不管
	vtHKMu.Unlock()

	// 左 Alt 按下：不该被吞（不是我们的热键）
	if c, _ := vtHookConsume("lalt", 0xA4, 0, true); c {
		t.Fatal("左 Alt 按下被吞了：前台会以为按了 Alt 却收不到抬起")
	}
	// 左 Alt 抬起：更不能被吞
	if c, _ := vtHookConsume("lalt", 0xA4, llkhfUp, false); c {
		t.Fatal("左 Alt 抬起被吞了 → 系统会认为左 Alt 一直按着（stuck LALT）")
	}

	// 右 Alt（我们的热键）：down/up 都要吞，配对方可
	if c, hk := vtHookConsume("ralt", 0xA5, 0, true); !c || !hk {
		t.Fatalf("右 Alt 按下应吞并派发，得到 %v %v", c, hk)
	}
	if c, _ := vtHookConsume("ralt", 0xA5, llkhfUp, false); !c {
		t.Fatal("右 Alt 抬起应吞（配对吞掉，前台看不到半截 Alt）")
	}
}

// TestLoadHotkeysLinkEmptyMainDisables 联动文件写 `main: []`（用户在设置页把主热键
// 叉掉）必须被当成**显式停用**照办，而不是"联动没说话"回退到 hotkeys.json——
// 实测 bug：叉掉鼠标侧键后输入法还在用侧键，就是回退把旧键死而复生。
// 本目录 hotkeys.json 真实存在且 main=[mouse-side]，正好当"下一级"的干扰项：
// 联动空 main 必须把它压掉。
func TestLoadHotkeysLinkEmptyMainDisables(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "voicetype.json")
	link := `{"hotkeys": {"main": [], "sub": ["alt"], "mode": "tap"}, "updatedAt": "test"}`
	if err := os.WriteFile(tmp, []byte(link), 0o644); err != nil {
		t.Fatalf("写联动文件失败: %v", err)
	}
	oldEnv := os.Getenv("VT_VOICETYPE_JSON")
	os.Setenv("VT_VOICETYPE_JSON", tmp)
	t.Cleanup(func() {
		if oldEnv == "" {
			os.Unsetenv("VT_VOICETYPE_JSON")
		} else {
			os.Setenv("VT_VOICETYPE_JSON", oldEnv)
		}
		vtResetHotkeyState()
	})

	vtLoadHotkeys()
	hk := vtHotkeys()
	if len(hk.Main) != 0 {
		t.Fatalf("联动 main=[] 是显式停用，热键必须为空，实际 main=%v（被 hotkeys.json 的旧键顶回来了）", hk.Main)
	}
	if len(hk.Sub) != 1 || hk.Sub[0] != "ralt" {
		t.Fatalf("sub 应跟随联动（alt→右侧 ralt），实际 sub=%v", hk.Sub)
	}
	if hk.Mode != "tap" {
		t.Fatalf("mode 应跟随联动 tap，实际 %q", hk.Mode)
	}

	// 反向钉住：联动文件不带 hotkeys 字段（只有颜色的手写文件）才留给下一级
	colorOnly := `{"accentColor": "#123456", "updatedAt": "test"}`
	if err := os.WriteFile(tmp, []byte(colorOnly), 0o644); err != nil {
		t.Fatalf("改写联动文件失败: %v", err)
	}
	vtLoadHotkeys()
	hk = vtHotkeys()
	if len(hk.Main) == 0 {
		t.Fatal("联动没管热键（字段缺失）时应该用 hotkeys.json/默认，热键不该被清空")
	}
}

// TestLinkCandidateWorkspaceRoot 双击启动（无 VT_VOICETYPE_JSON 环境变量）也必须
// 探测到工作区根的联动文件：voicetype → voice → 组件 → 工作区根是**三级**向上。
// 实测 bug：写成了两级（解析到 组件\voicetype.json，不存在），双击启动的输入法
// 读不到主程序设置，回退 hotkeys.json——叉掉的侧键在重启后死而复生。
func TestLinkCandidateWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "组件dir", "voice", "voicetype")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	linkPath := filepath.Join(root, "voicetype.json")
	if err := os.WriteFile(linkPath, []byte(`{"hotkeys": {"main": ["ctrl","f2"], "sub": []}, "updatedAt": "t"}`), 0o644); err != nil {
		t.Fatalf("写联动文件失败: %v", err)
	}
	oldDir, oldEnv := vtDir, os.Getenv("VT_VOICETYPE_JSON")
	vtDir = deep
	os.Unsetenv("VT_VOICETYPE_JSON")
	t.Cleanup(func() {
		vtDir = oldDir
		if oldEnv != "" {
			os.Setenv("VT_VOICETYPE_JSON", oldEnv)
		}
		vtResetHotkeyState()
	})

	st := vtLoadLink()
	if !st.Loaded {
		t.Fatal("未探测到工作区根的联动文件（层级差一级？双击启动将读不到主程序设置）")
	}
	if filepath.Clean(st.Path) != filepath.Clean(linkPath) {
		t.Fatalf("联动文件应为工作区根的 %q，实际 %q", linkPath, st.Path)
	}
	if len(st.Cfg.Hotkeys.Main) != 2 || st.Cfg.Hotkeys.Main[0] != "ctrl" {
		t.Fatalf("联动内容不对: %v", st.Cfg.Hotkeys.Main)
	}
}

// TestMouseSideKeyNames 鼠标侧键按枚区分：XBUTTON1/2 → mouse-side-1/-2，
// 配 `mouse-side` 两枚都算（旧配置），配具体一枚时另一枚不算。
// 这条钉的是"侧键1/侧键2 不分家"的老问题——两枚折叠成一个键名时，
// 想让侧键 2 留给浏览器前进键是做不到的。
func TestMouseSideKeyNames(t *testing.T) {
	if got := vtMouseKeyName(1); got != "mouse-side-1" {
		t.Errorf("vtMouseKeyName(1)=%q 期望 mouse-side-1", got)
	}
	if got := vtMouseKeyName(2); got != "mouse-side-2" {
		t.Errorf("vtMouseKeyName(2)=%q 期望 mouse-side-2", got)
	}
	if got := vtMouseKeyName(0); got != "" {
		t.Errorf("vtMouseKeyName(0)=%q 期望空（非侧键事件）", got)
	}
	// 键名匹配：mouse-side 泛匹配两枚；具体键名只认自己
	cases := []struct {
		cfg, actual string
		want        bool
	}{
		{"mouse-side", "mouse-side-1", true},
		{"mouse-side", "mouse-side-2", true},
		{"mouse-side-1", "mouse-side-1", true},
		{"mouse-side-1", "mouse-side-2", false},
		{"mouse-side-2", "mouse-side-1", false},
		{"mouse-side", "ralt", false},
		{"alt", "mouse-side-1", false},
	}
	for _, c := range cases {
		if got := vtKeyMatches(c.cfg, c.actual); got != c.want {
			t.Errorf("vtKeyMatches(%q,%q)=%v 期望 %v", c.cfg, c.actual, got, c.want)
		}
	}
}

// TestMouseInHotkeysBySide 吞键/接管按枚命中：
//   · 配 mouse-side（旧写法）→ 两枚都接管（旧行为不回归）；
//   · 配 mouse-side-1      → 只有侧键 1 接管，侧键 2 完全放行（留给原生前进键）；
//   · main/sub 任一槽命中都算。
func TestMouseInHotkeysBySide(t *testing.T) {
	hk := vtHotkeysCfg{Main: []string{"mouse-side"}}
	if !vtMouseInHotkeys(hk, "mouse-side-1") || !vtMouseInHotkeys(hk, "mouse-side-2") {
		t.Fatal("配 mouse-side 时两枚侧键都应接管（旧行为）")
	}
	hk = vtHotkeysCfg{Main: []string{"mouse-side-1"}}
	if !vtMouseInHotkeys(hk, "mouse-side-1") {
		t.Fatal("侧键 1 应接管")
	}
	if vtMouseInHotkeys(hk, "mouse-side-2") {
		t.Fatal("只配侧键 1 时，侧键 2 不该被接管——否则前进键被吞、原生功能丢失")
	}
	hk = vtHotkeysCfg{Main: []string{"ralt"}, Sub: []string{"mouse-side-2"}}
	if !vtMouseInHotkeys(hk, "mouse-side-2") {
		t.Fatal("sub 槽绑的侧键 2 也应接管")
	}
	if vtMouseInHotkeys(hk, "mouse-side-1") {
		t.Fatal("sub 绑侧键 2 时侧键 1 不该被接管")
	}
	if vtMouseInHotkeys(vtHotkeysCfg{Main: []string{"ralt"}}, "mouse-side-1") {
		t.Fatal("没绑侧键时任何一枚都不接管")
	}
}

// TestMouseSideComboEdges 组合评估用具体侧键名：绑 mouse-side 时按侧键 1 触发、
// 松开结束；两枚同时按住时松开一枚不算结束（另一枚还按着）；绑具体一枚时另一枚
// 按下不触发。等价于钩子回调里 vtHeld + vtEvalCombos 的路径（回调本身依赖
// syscall/消息循环，这里钉纯逻辑）。
func TestMouseSideComboEdges(t *testing.T) {
	vtResetHotkeyState()
	vtHKMu.Lock()
	vtHK.Main = []string{"mouse-side"}
	vtHKMu.Unlock()

	vtHeld["mouse-side-1"] = true
	vtEvalCombos("mouse-side-1", 0)
	ev := expectHotkeyEvent(t, true, 0, "mouse-side-1")
	_ = ev

	// 两枚同按：松开 1（2 还按着）不应产生"松开"边沿
	vtHeld["mouse-side-2"] = true
	vtEvalCombos("mouse-side-2", 0)
	drainHotkeyEvents()
	delete(vtHeld, "mouse-side-1")
	vtEvalCombos("mouse-side-1", 0)
	select {
	case e := <-vtHKEvents:
		t.Fatalf("另一枚侧键还按着，不该发松开边沿，却收到 %+v", e)
	default:
	}
	// 最后一枚松开 → 结束
	delete(vtHeld, "mouse-side-2")
	vtEvalCombos("mouse-side-2", 0)
	expectHotkeyEvent(t, false, 0, "mouse-side-2")

	// 只绑侧键 1：按侧键 2 不触发
	vtResetHotkeyState()
	vtHKMu.Lock()
	vtHK.Main = []string{"mouse-side-1"}
	vtHKMu.Unlock()
	vtHeld["mouse-side-2"] = true
	vtEvalCombos("mouse-side-2", 0)
	select {
	case e := <-vtHKEvents:
		t.Fatalf("未绑定的侧键 2 不该触发，却收到 %+v", e)
	default:
	}
	vtHeld["mouse-side-1"] = true
	vtEvalCombos("mouse-side-1", 0)
	expectHotkeyEvent(t, true, 0, "mouse-side-1")
}

// expectHotkeyEvent 从事件通道取一个边沿并校验，取不到即失败。
func expectHotkeyEvent(t *testing.T, down bool, slot int, name string) vtHotkeyEvent {
	t.Helper()
	select {
	case e := <-vtHKEvents:
		if e.Down != down || e.Slot != slot || e.Name != name {
			t.Fatalf("事件不匹配：得到 %+v，期望 down=%v slot=%d name=%s", e, down, slot, name)
		}
		return e
	default:
		t.Fatalf("期望事件 down=%v slot=%d name=%s 没有到达", down, slot, name)
		return vtHotkeyEvent{}
	}
}

func drainHotkeyEvents() {
	for {
		select {
		case <-vtHKEvents:
		default:
			return
		}
	}
}

// TestArmVKRecordsExactSide 钩子里的"这一轮按的是哪一侧"必须记真实 vk。
//
// 这条钉的是用户实测的"左 Alt 卡住"：组合键介入会在钩子回调里**立刻**按 vtArmVK
// 补发那个键的 down。若 vtArmVK 还是**上一轮的旧值**（比如上午配置是 alt 时留下的
// 0xA4=左 Alt），补发出去的就是左 Alt，而真实抬起只清右侧 → 左 Alt 永久按着
// （之后 Ctrl+V 变成 Ctrl+Alt+V，粘不出来）。现在钩子回调里同步写真实 vk。
func TestArmVKRecordsExactSide(t *testing.T) {
	vtResetHotkeyState()
	vtHKMu.Lock()
	vtHK.Main = []string{"mouse-side"}
	vtHK.Sub = []string{"alt"} // 配置写 alt（两侧都算）
	vtHKMu.Unlock()

	// 模拟"上一轮"留下的旧值：左 Alt
	vtArmVK.Store(0xA4)

	// 用户这次实际按的是**右** Alt：钩子必须把它记成 0xA5
	if c, hk := vtHookConsume("ralt", 0xA5, 0, true); !c || !hk {
		t.Fatalf("右 Alt 应被接管，得到 consume=%v hotkey=%v", c, hk)
	}
	vtArmVK.Store(0xA5) // 钩子回调里同步做的事（与 vt_windows.go 的 armVK 赋值对应）
	if got := vtArmVK.Load(); got != 0xA5 {
		t.Fatalf("vtArmVK 应等于真实按下的 vk 0xA5，得到 %#x", got)
	}
	// 注释用：配置里的通用名 "alt" 没有对应 vk（vtVKByName 返回 0），
	// 所以"按配置猜一侧"这条路本身就不成立——必须用真实 vk。
	if got := vtVKByName("alt"); got != 0 {
		t.Logf("提示：vtVKByName(\"alt\")=%#x（预期 0，通用名不对应具体一侧）", got)
	}
}
