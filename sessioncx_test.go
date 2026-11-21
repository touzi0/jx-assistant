package main

// 会话级组件/插件开关覆盖（sessioncx.go）的行为钉子：
//   1. 覆盖优先于全局：会话把全局开着的关掉、把全局关着的打开，都要成立；
//   2. 与全局拨成一致的值＝覆盖记录删除（恢复跟随全局，存储不膨胀）；
//   3. cxoComponentLive（worker/端点存活口径）：全局关了但还有会话要它 → 活；
//   4. 持久化：覆盖写进会话附属数据，重启（重新 loadSessionCX）仍在；
//      会话删除 → forget 摘除，custom 归假。

import (
	"os"
	"path/filepath"
	"testing"
)

// 造一个带两个组件的测试 App：voice 全局开、git-panel 全局关（写状态文件）。
func newSessionCXTestApp(t *testing.T) (*App, string, string) {
	t.Helper()
	dir := t.TempDir()
	compDir := filepath.Join(dir, "组件")
	for _, id := range []string{"voice", "git-panel"} {
		sub := filepath.Join(compDir, id)
		if err := os.MkdirAll(sub, 0755); err != nil {
			t.Fatal(err)
		}
		m := `{"id":"` + id + `","name":"` + id + `","version":"1.0.0","client":{}}`
		if err := os.WriteFile(filepath.Join(sub, "manifest.json"), []byte(m), 0644); err != nil {
			t.Fatal(err)
		}
	}
	dataDir := t.TempDir()
	// git-panel 显式关（未记录=默认开的读时解释）
	state := `{"git-panel":false}` + "\n"
	if err := os.WriteFile(filepath.Join(dataDir, "components.json"), []byte(state), 0644); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	a := &App{
		Store:      store,
		Components: InitComponents(compDir, filepath.Join(dataDir, "components.json")),
		Layers:     &PluginLayerRegistry{},
	}
	a.SessionCX = loadSessionCX(store)
	// 两个会话：A、B
	mk := func(id string) *SessionMeta {
		m := &SessionMeta{ID: id, Mode: "task"}
		if err := store.Create(m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	return a, mk("sess-a").ID, mk("sess-b").ID
}

func TestSessionCXOverridePriority(t *testing.T) {
	a, sidA, sidB := newSessionCXTestApp(t)

	// 基线：voice 全局开、git-panel 全局关
	if !a.cxoComponentEnabled(sidA, "voice") || a.cxoComponentEnabled(sidA, "git-panel") {
		t.Fatal("基线：无覆盖时应等于全局开关")
	}

	// A 会话关 voice（全局开着）→ A 关、B 与全局不受影响
	a.cxoSetComponent(sidA, "voice", false)
	if a.cxoComponentEnabled(sidA, "voice") {
		t.Fatal("A 覆盖关 voice 后，A 里应停用")
	}
	if !a.cxoComponentEnabled(sidB, "voice") || !a.Components.IsEnabled("voice") {
		t.Fatal("A 的覆盖不应影响 B 与全局开关")
	}
	if !a.SessionCX.custom(sidA) || a.SessionCX.custom(sidB) {
		t.Fatal("sessionCustom 只应标在 A")
	}

	// A 会话开 git-panel（全局关着）→ A 开
	a.cxoSetComponent(sidA, "git-panel", true)
	if !a.cxoComponentEnabled(sidA, "git-panel") || a.Components.IsEnabled("git-panel") {
		t.Fatal("A 覆盖开 git-panel 后：A 生效、全局仍关")
	}

	// cxoComponentLive：voice 全局仍开 → 活；git-panel 全局关但有会话要 → 活；
	// 拨回去后（无任何会话要）→ 不活
	if !a.cxoComponentLive("voice") || !a.cxoComponentLive("git-panel") {
		t.Fatal("全局开着或有会话要着的组件应视为活")
	}
	a.cxoSetComponent(sidA, "git-panel", false)
	if a.cxoComponentLive("git-panel") {
		t.Fatal("全局关且无会话要的组件应视为不活")
	}
}

func TestSessionCXOverrideNormalizesToGlobal(t *testing.T) {
	a, sidA, _ := newSessionCXTestApp(t)
	a.cxoSetComponent(sidA, "voice", false) // 覆盖：关
	if !a.SessionCX.custom(sidA) {
		t.Fatal("写入覆盖后应标记定制")
	}
	a.cxoSetComponent(sidA, "voice", true) // 拨回与全局一致 → 覆盖删除
	if a.SessionCX.custom(sidA) {
		t.Fatal("拨回全局值后覆盖应删除（恢复跟随全局）")
	}
	if !a.cxoComponentEnabled(sidA, "voice") {
		t.Fatal("跟随全局＝开")
	}
}

func TestSessionCXPersistAndForget(t *testing.T) {
	a, sidA, sidB := newSessionCXTestApp(t)
	a.cxoSetComponent(sidA, "voice", false)
	a.cxoSetComponent(sidA, "git-panel", true)
	a.cxoSetPlugin(sidA, "claude", "demo@market", true)

	// 重启（重新 load）：覆盖仍在
	r2 := loadSessionCX(a.Store)
	if !r2.custom(sidA) || r2.custom(sidB) {
		t.Fatal("重启后 A 的覆盖应仍在、B 应仍无覆盖")
	}
	if r2.of(sidA) == nil || r2.of(sidA).Components["voice"] != false || r2.of(sidA).Components["git-panel"] != true {
		t.Fatal("重启后覆盖内容应完整")
	}
	if r2.of(sidA).Plugins["claude/demo@market"] != true {
		t.Fatal("插件覆盖键应为 层/插件id")
	}

	// 会话删除 → forget + 附属数据行一并清
	a.SessionCX.forget(sidA)
	if a.SessionCX.custom(sidA) {
		t.Fatal("forget 后不应再标记定制")
	}
}

// 会话级组件配置合并层（定稿 2026-11-08）：会话独立时读写落会话覆盖、
// 读取按「会话覆盖 > 全局值」合并；行级切回跟随＝删该组件记录；总开关关掉
// ＝整行完全回归全局（配置覆盖与启停覆盖一起清空）。
func TestSessionCXComponentCfg(t *testing.T) {
	a, sidA, sidB := newSessionCXTestApp(t)

	// 基线：总开关关着 → 没有组件是会话独立，视图里也没有覆盖值
	v := a.cxoComponentCfgView(sidA)
	if v["scope"].(bool) {
		t.Fatal("基线总开关应为关")
	}
	if a.cxoComponentCfgOn(sidA, "voice") {
		t.Fatal("总开关关着时不应有组件的配置会话独立")
	}

	// 打开总开关：缺省每个组件都独立（一键切全场）
	a.cxoSetComponentCfgScope(sidA, true)
	if !a.cxoComponentCfgOn(sidA, "voice") || !a.cxoComponentCfgOn(sidA, "git-panel") {
		t.Fatal("总开关开着时组件缺省会话独立")
	}
	if a.cxoComponentCfgOn(sidB, "voice") {
		t.Fatal("A 的总开关不该影响 B")
	}

	// 写一条覆盖：落 Cfg[组件id][键]，视图带出来
	a.cxoSetComponentCfgValue(sidA, "voice", "cx:voice:rate", `1.5`)
	vals := a.cxoComponentCfgView(sidA)["components"].(map[string]any)["voice"].(map[string]any)["values"].(map[string]string)
	if vals["cx:voice:rate"] != `1.5` {
		t.Fatalf("覆盖值应出现在视图里: %v", vals)
	}
	if !a.SessionCX.custom(sidA) {
		t.Fatal("有配置覆盖应标记定制")
	}

	// 行级切回跟随全局＝删该组件记录（恢复语义）
	a.cxoSetComponentCfgMode(sidA, "voice", false)
	if a.cxoComponentCfgOn(sidA, "voice") {
		t.Fatal("切回跟随全局后不该再是会话独立")
	}
	if o := a.SessionCX.of(sidA); o != nil && len(o.Cfg["voice"]) > 0 {
		t.Fatalf("切回跟随全局应删掉该组件的覆盖记录: %v", o.Cfg)
	}
	if !a.cxoComponentCfgOn(sidA, "git-panel") {
		t.Fatal("别的组件不该被行级切换波及")
	}

	// 总开关关掉＝整行完全回归全局：配置与启停覆盖一起清空
	a.cxoSetComponentCfgValue(sidA, "git-panel", "cx:git-panel:branch", `"dev"`)
	a.cxoSetComponent(sidA, "git-panel", true) // 启停也覆盖成与全局不同（全局关）
	if !a.cxoComponentEnabled(sidA, "git-panel") {
		t.Fatal("启停覆盖应生效")
	}
	a.cxoSetComponentCfgScope(sidA, false)
	if o := a.SessionCX.of(sidA); o != nil && (o.CfgScope || len(o.Cfg) > 0 || len(o.Components) > 0) {
		t.Fatalf("总开关关掉应清空配置与启停覆盖: %+v", o)
	}
	if a.cxoComponentEnabled(sidA, "git-panel") {
		t.Fatal("总开关关掉后启停应回落到全局（全局关）")
	}
	if a.SessionCX.custom(sidA) {
		t.Fatal("全清后不应再标记定制")
	}

	// 持久化：重启后覆盖仍在
	a.cxoSetComponentCfgScope(sidA, true)
	a.cxoSetComponentCfgValue(sidA, "voice", "cx:voice:rate", `2`)
	r2 := loadSessionCX(a.Store)
	o := r2.of(sidA)
	if o == nil || !o.CfgScope || o.Cfg["voice"]["cx:voice:rate"] != `2` {
		t.Fatalf("重启后会话配置覆盖应完整: %+v", o)
	}
}

// 接口层：GET 视图 / POST 写键与删键 / mode 与 scope（含在「跟随全局」时拒写）。
func TestSessionCXComponentCfgHTTP(t *testing.T) {
	a, sidA, _ := newSessionCXTestApp(t)

	code, resp := callHandler(t, a.handleComponentCfgGet, "GET", "/api/components/cfg?s="+sidA, "")
	if code != 200 || resp["cfg"] == nil {
		t.Fatalf("读视图失败: %d %v", code, resp)
	}
	// 总开关没开时写键应被拒（提示先切会话独立）
	code, _ = callHandler(t, a.handleComponentCfgSet, "POST", "/api/components/cfg",
		`{"session":"`+sidA+`","id":"voice","key":"cx:voice:x","value":"1"}`)
	if code == 200 {
		t.Fatal("跟随全局时不应允许写会话配置")
	}
	code, _ = callHandler(t, a.handleComponentCfgScope, "POST", "/api/components/cfg/scope",
		`{"session":"`+sidA+`","on":true}`)
	if code != 200 {
		t.Fatalf("开总开关失败: %d", code)
	}
	code, resp = callHandler(t, a.handleComponentCfgSet, "POST", "/api/components/cfg",
		`{"session":"`+sidA+`","id":"voice","key":"cx:voice:x","value":"1"}`)
	if code != 200 {
		t.Fatalf("写配置失败: %d %v", code, resp)
	}
	// value 缺省＝删这条（回落到全局值）
	code, resp = callHandler(t, a.handleComponentCfgSet, "POST", "/api/components/cfg",
		`{"session":"`+sidA+`","id":"voice","key":"cx:voice:x"}`)
	if code != 200 {
		t.Fatalf("删配置失败: %d %v", code, resp)
	}
	cv := a.cxoComponentCfgView(sidA)["components"].(map[string]any)["voice"].(map[string]any)
	if len(cv["values"].(map[string]string)) != 0 {
		t.Fatalf("删键后不应还有覆盖: %v", cv)
	}
	// 不存在的组件/会话一律 404
	if code, _ := callHandler(t, a.handleComponentCfgSet, "POST", "/api/components/cfg",
		`{"session":"`+sidA+`","id":"nope","key":"k","value":"v"}`); code != 404 {
		t.Fatalf("未知组件应 404，实得 %d", code)
	}
	if code, _ := callHandler(t, a.handleComponentCfgGet, "GET", "/api/components/cfg?s=nope", ""); code != 404 {
		t.Fatalf("未知会话应 404，实得 %d", code)
	}
}
