package main

// 分块静默更新（update_blocks.go）的单测：清单解析、快照比对（落后多版一步
// 补齐 / 已是最新不动）、附件寻址、engine 包结构校验、状态记账往返。
// engine 的原地交换与重启接力不进单测（会动真身 exe/进程，走端到端手测）。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseUpdateBlocks(t *testing.T) {
	raw := `{
	  "engine": {"version": "20261012V1", "file": "engine-20261012V1.zip", "sha256": "aa"},
	  "components": {"voice": {"version": "2.0", "file": "voice-2.0.zip", "sha256": "bb"}}
	}`
	var b updateBlocks
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if b.Engine == nil || b.Engine.Version != "20261012V1" || b.Engine.File != "engine-20261012V1.zip" {
		t.Fatalf("engine 块解析不对: %+v", b.Engine)
	}
	if v, ok := b.Components["voice"]; !ok || v.Version != "2.0" {
		t.Fatalf("voice 块解析不对: %+v", b.Components)
	}
	// 空/仅组件的合法形态
	var only updateBlocks
	if err := json.Unmarshal([]byte(`{"components":{}}`), &only); err != nil || only.Engine != nil {
		t.Fatalf("仅组件清单应合法且 engine 为空: %+v %v", only, err)
	}
}

// findReleaseAssetURL 按附件名找直链；私网直链按发布源公网化（复用 update.go 规则）。
func TestFindReleaseAssetURL(t *testing.T) {
	rel := releaseInfo{Assets: []releaseAsset{
		{Name: "安装包.exe", BrowserDownloadURL: "https://pub.example/dl/%E5%AE%89%E8%A3%85%E5%8C%85.exe"},
		{Name: "blocks.json", BrowserDownloadURL: "https://pub.example/dl/blocks.json"},
	}}
	if got := findReleaseAssetURL(rel.Assets, "blocks.json", "https://pub.example/api"); got != "https://pub.example/dl/blocks.json" {
		t.Fatalf("blocks.json 直链不对: %s", got)
	}
	if got := findReleaseAssetURL(rel.Assets, "没有.exe", "https://pub.example/api"); got != "" {
		t.Fatalf("不存在的附件应返回空串: %s", got)
	}
}

// findEngineExe：zip 解压目录里应有且仅有一个 exe。
func TestFindEngineExe(t *testing.T) {
	dir := t.TempDir()
	if _, err := findEngineExe(dir); err == nil {
		t.Fatal("空目录应报错（没有 exe）")
	}
	if err := os.WriteFile(filepath.Join(dir, "a.exe"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.exe"), []byte("y"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := findEngineExe(dir); err == nil || !strings.Contains(err.Error(), "多个") {
		t.Fatalf("多个 exe 应报错: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "b.exe")); err != nil {
		t.Fatal(err)
	}
	got, err := findEngineExe(dir)
	if err != nil || !strings.HasSuffix(got, "a.exe") {
		t.Fatalf("唯一 exe 应命中: %s %v", got, err)
	}
}

// mkTestRegistry 建一个指向临时目录的组件注册表，并把 versions 落成
// 组件/<id>/manifest.json（id: version）。
func mkTestRegistry(t *testing.T, versions map[string]string) *ComponentRegistry {
	t.Helper()
	dir := t.TempDir()
	for id, ver := range versions {
		sub := filepath.Join(dir, id)
		if err := os.MkdirAll(sub, 0755); err != nil {
			t.Fatal(err)
		}
		m := `{"id":"` + id + `","name":"` + id + `","version":"` + ver + `","client":{}}`
		if err := os.WriteFile(filepath.Join(sub, "manifest.json"), []byte(m), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return InitComponents(dir, filepath.Join(t.TempDir(), "components.json"))
}

// 快照式清单的两类典型场景（用户原始需求）：
//   - V1 用户落后多版：V3 清单里 voice 沿用 V2 发的 2.0 → 一次补齐 engine+voice；
//   - V2 用户只落后主程序：voice 已是清单版本 → 组件不动，只更 engine。
func TestPlanComponentBlocks(t *testing.T) {
	a := &App{Components: mkTestRegistry(t, map[string]string{
		"voice":     "1.0", // V1 装机时的语音组件
		"git-panel": "1.0",
	})}
	// V3 的快照清单：voice 沿用 V2 发的 2.0，git-panel 服务器不管（不在清单里）
	v3 := map[string]updateBlock{
		"voice":     {Version: "2.0", File: "voice-2.0.zip"},
		"git-panel": {Version: "1.0", File: "git-panel-1.0.zip"}, // 与本地相同 → 不动
	}
	if got := a.planComponentBlocks(v3); !reflect.DeepEqual(got, []string{"voice"}) {
		t.Fatalf("V3 快照应只更新 voice: %v", got)
	}
	// V2 装机（voice 已 2.0）再对同一份 V3 清单：组件全不动
	b := &App{Components: mkTestRegistry(t, map[string]string{
		"voice":     "2.0",
		"git-panel": "1.0",
	})}
	if got := b.planComponentBlocks(v3); len(got) != 0 {
		t.Fatalf("V2 用户对 V3 清单应无组件更新: %v", got)
	}
	// 清单里出现本机没装的组件：补装（2026-11-06 客户事故——内置功能组件化后
	// 老客户升级 engine，入口靠组件注入，静默更新必须能把新内置组件带到场，
	// 否则入口消失且市场入口同失，死锁）；版本为空的块防呆跳过
	c := &App{Components: mkTestRegistry(t, map[string]string{"voice": "1.0"})}
	got := c.planComponentBlocks(map[string]updateBlock{
		"voice":    {Version: "2.0"},
		"new-comp": {Version: "1.0"},
		"broken":   {Version: ""},
	})
	if !reflect.DeepEqual(got, []string{"new-comp", "voice"}) {
		t.Fatalf("未装组件应补装、空版本跳过: %v", got)
	}
	// 本地比清单还新（用户手动装了更新版）：不回退
	d := &App{Components: mkTestRegistry(t, map[string]string{"voice": "2.1"})}
	if got := d.planComponentBlocks(map[string]updateBlock{"voice": {Version: "2.0"}}); len(got) != 0 {
		t.Fatalf("本地更新时不应回退: %v", got)
	}
	// 本机那份是**用户组件**（组件/user/）：不进内置热更新——版本与去留归用户，
	// 清单里即使有同名块也不覆盖它
	root := t.TempDir()
	writeComp(t, root, "keep", "keep", "1.0")
	reg := InitComponents(root, filepath.Join(t.TempDir(), "components.json"))
	writeComp(t, filepath.Join(root, userComponentsSubdir), "plug", "plug", "0.1")
	reg.Rescan()
	if reg.IsBuiltin("plug") {
		t.Fatal("组件/user/ 下的应识别为用户组件")
	}
	e := &App{Components: reg}
	if got := e.planComponentBlocks(map[string]updateBlock{"plug": {Version: "9.0"}}); len(got) != 0 {
		t.Fatalf("用户组件不应参与内置热更新: %v", got)
	}
	if got := e.planComponentBlocks(map[string]updateBlock{"keep": {Version: "2.0"}}); len(got) != 1 {
		t.Fatalf("内置组件落后时仍应热更新: %v", got)
	}
}

// 状态记账往返：EnginePending/Argv/Applied 落盘再读回。
func TestUpdateBlockStateRoundtrip(t *testing.T) {
	a := &App{Cfg: &Config{DataDir: t.TempDir()}}
	// pending 版本用远期日期：syncPendingToState 只认「比 appVersion 新」的
	// pending，写死近期日期会在版本号追上当天后过期翻车
	st := updateBlockState{
		EnginePending: "20991231-1",
		EngineArgv:    []string{"app.exe", "-config", "config.json"},
		SwappedAt:     "2026-10-12T10:00:00+08:00",
		Applied:       map[string]string{"voice": "2.0"},
	}
	if err := a.saveUpdateBlockState(st); err != nil {
		t.Fatal(err)
	}
	got := a.loadUpdateBlockState()
	if got.EnginePending != st.EnginePending || !reflect.DeepEqual(got.EngineArgv, st.EngineArgv) ||
		got.Applied["voice"] != "2.0" {
		t.Fatalf("记账往返不一致: %+v", got)
	}
	// syncPendingToState 同步进横幅状态
	a.syncPendingToState()
	snap := updateSnapshot()
	if !snap.PendingRestart || snap.PendingVersion != "20991231-1" {
		t.Fatalf("pending 应同步进快照: %+v", snap)
	}
}
