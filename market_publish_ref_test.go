package main

// 组件 / 插件发布（market_publish.go 的三类推广）单测：
//   · zipDirForPublish 剔除运行时产物（缓存 / 用户数据 / 日志），内容不丢；
//   · prepareComponentPublish / preparePluginPublish 的元信息推导与拒绝路径
//     （内置组件拒收、未安装拒收、清单缺 client 半边拒收）；
//   · handleMarketPublish 组件/插件分支：id 与版本锁定为包内清单值（表单
//     改写不生效）、同版本重传拦截、待审 pending+message 透传。
// 假市场与请求装配复用 market_publish_test.go 的 newFakePublishMarket /
// newPublishTestApp / publishReq。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

/* ---------- 造件小工具 ---------- */

// writeFileForTest 建父目录并写文件。
func writeFileForTest(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// installTestComponent 往 组件/user/<id>/ 放一个用户组件（含运行时垃圾），
// 返回组件 id。compDir 是 App 的组件根（组件/）。
func installTestComponent(t *testing.T, a *App, id, manifest string) string {
	t.Helper()
	dir := filepath.Join(a.Components.UserDir(), id)
	writeFileForTest(t, filepath.Join(dir, "manifest.json"), manifest)
	writeFileForTest(t, filepath.Join(dir, "client.js"), "// client\n")
	writeFileForTest(t, filepath.Join(dir, "client.css"), "body{}\n")
	writeFileForTest(t, filepath.Join(dir, "README.md"), "说明\n")
	// 运行时垃圾：打包时必须剔除
	writeFileForTest(t, filepath.Join(dir, "config.json"), `{"token":"secret"}`)
	writeFileForTest(t, filepath.Join(dir, "runtime.json"), `{"cursor":"x"}`)
	writeFileForTest(t, filepath.Join(dir, "mappings.json"), `{"a":1}`)
	writeFileForTest(t, filepath.Join(dir, "run.log"), "log\n")
	writeFileForTest(t, filepath.Join(dir, "app.exe~"), "backup\n")
	writeFileForTest(t, filepath.Join(dir, "webview2-profile", "Cache", "f_001"), "cache\n")
	writeFileForTest(t, filepath.Join(dir, "__pycache__", "w.pyc"), "pyc\n")
	writeFileForTest(t, filepath.Join(dir, "node_modules", "x", "index.js"), "x\n")
	writeFileForTest(t, filepath.Join(dir, "history", "h.json"), "[]")
	writeFileForTest(t, filepath.Join(dir, "history.json"), "[]")
	a.Components.Rescan()
	return id
}

// installTestPlugin 往 claude-plugins 缓存里装一个 Claude Code 插件（手写
// 安装记录后重初始化注册表，等价于装好的状态），返回插件 key（name@市场）。
func installTestPlugin(t *testing.T, a *App, name, version string) string {
	t.Helper()
	safe := ccSanitizeName(name)
	root := filepath.Join(a.CCPlugins.root, "cache", "local", safe, ccSanitizeName(version))
	writeFileForTest(t, filepath.Join(root, ".claude-plugin", "plugin.json"),
		`{"name":"`+name+`","displayName":"演示插件","version":"`+version+`","description":"测试用"}`)
	writeFileForTest(t, filepath.Join(root, "skills", "demo", "SKILL.md"), "---\nname: demo\n---\n正文\n")
	writeFileForTest(t, filepath.Join(root, "debug.log"), "log\n")
	writeFileForTest(t, filepath.Join(root, "config.json"), `{"key":"secret"}`)
	state := map[string]any{
		"version":      1,
		"marketplaces": map[string]any{},
		"plugins": map[string]any{
			safe + "@local": map[string]any{
				"marketplace": "local", "version": version,
				"path": "cache/local/" + safe + "/" + ccSanitizeName(version),
			},
		},
	}
	b, _ := json.Marshal(state)
	writeFileForTest(t, a.CCPlugins.statePath, string(b))
	a.CCPlugins = InitCCPlugins(nil, a.Cfg.DataDir) // Reload 只读内存态，落盘后重初始化
	return safe + "@local"
}

const okComponentManifest = `{"id":"demo-comp","name":"演示组件","version":"0.2.0","description":"测试组件",` +
	`"client":{"js":"client.js","css":"client.css"}}`

/* ---------- zipDirForPublish：剔除运行时产物，内容不丢 ---------- */

func TestZipDirForPublishExcludesRuntime(t *testing.T) {
	fm := newFakePublishMarket(t)
	a := newPublishTestApp(t, fm.srv.URL+"/market/manifest.json")
	installTestComponent(t, a, "demo-comp", okComponentManifest)
	data, excluded, err := zipDirForPublish(filepath.Join(a.Components.UserDir(), "demo-comp"))
	if err != nil {
		t.Fatal(err)
	}
	names := zipNamesForTest(t, data)
	for _, keep := range []string{"manifest.json", "client.js", "client.css", "README.md"} {
		if !hasZipEntry(names, keep) {
			t.Errorf("内容文件 %s 不该被剔除: %v", keep, names)
		}
	}
	for _, junk := range []string{"config.json", "runtime.json", "mappings.json", "run.log",
		"app.exe~", "webview2-profile/Cache/f_001", "__pycache__/w.pyc",
		"node_modules/x/index.js", "history/h.json", "history.json"} {
		if hasZipEntry(names, junk) {
			t.Errorf("运行时产物 %s 不该进包: %v", junk, names)
		}
	}
	if len(excluded) == 0 || !strings.Contains(strings.Join(excluded, "|"), "config.json") {
		t.Fatalf("剔除清单该包含 config.json 等: %v", excluded)
	}
}

/* ---------- prepareComponentPublish / preparePluginPublish ---------- */

func TestPrepareComponentPublishMeta(t *testing.T) {
	fm := newFakePublishMarket(t)
	a := newPublishTestApp(t, fm.srv.URL+"/market/manifest.json")
	installTestComponent(t, a, "demo-comp", okComponentManifest)
	data, meta, excluded, err := a.prepareComponentPublish("demo-comp")
	if err != nil {
		t.Fatal(err)
	}
	if meta.ID != "demo-comp" || meta.Name != "演示组件" || meta.Version != "0.2.0" || meta.Description != "测试组件" {
		t.Fatalf("元信息不对: %#v", meta)
	}
	if len(data) == 0 || len(excluded) == 0 {
		t.Fatal("包体与剔除清单不该为空")
	}
	// 拒绝路径：内置组件、未安装
	installTestComponent(t, a, "builtin-x", `{"id":"builtin-x","client":{"js":"client.js"}}`)
	// builtin-x 落在 user 区仍是用户组件；造真内置要放 组件/ 根
	writeFileForTest(t, filepath.Join(a.Components.BuiltinDir(), "real-builtin", "manifest.json"),
		`{"id":"real-builtin","client":{"js":"client.js"}}`)
	a.Components.Rescan()
	if _, _, _, err := a.prepareComponentPublish("real-builtin"); err == nil || !strings.Contains(err.Error(), "内置组件") {
		t.Fatalf("内置组件该拒收，实得 %v", err)
	}
	if _, _, _, err := a.prepareComponentPublish("没装的组件"); err == nil || !strings.Contains(err.Error(), "没有安装") {
		t.Fatalf("未安装组件该拒收，实得 %v", err)
	}
	// 清单缺浏览器半边：服务端同款校验在本机就拦
	installTestComponent(t, a, "no-client", `{"id":"no-client","name":"x"}`)
	if _, _, _, err := a.prepareComponentPublish("no-client"); err == nil || !strings.Contains(err.Error(), "client.js / client.css") {
		t.Fatalf("缺 client 半边该拒收，实得 %v", err)
	}
}

func TestPreparePluginPublishMeta(t *testing.T) {
	fm := newFakePublishMarket(t)
	a := newPublishTestApp(t, fm.srv.URL+"/market/manifest.json")
	key := installTestPlugin(t, a, "Demo Plugin!", "0.1.0")
	if key != "Demo-Plugin@local" { // ccSanitizeName 把空格/叹号折成中划线
		t.Fatalf("key 应为清洗后的形态，实得 %q", key)
	}
	data, meta, excluded, err := a.preparePluginPublish(key)
	if err != nil {
		t.Fatal(err)
	}
	if meta.ID != "Demo-Plugin" {
		t.Fatalf("插件 id 应为 ccSanitizeName(name)，实得 %q", meta.ID)
	}
	if meta.Name != "演示插件" || meta.Version != "0.1.0" || meta.Description != "测试用" {
		t.Fatalf("元信息不对: %#v", meta)
	}
	names := zipNamesForTest(t, data)
	if !hasZipEntry(names, ".claude-plugin/plugin.json") || !hasZipEntry(names, "skills/demo/SKILL.md") {
		t.Fatalf("包内容不对: %v", names)
	}
	if hasZipEntry(names, "debug.log") || hasZipEntry(names, "config.json") {
		t.Fatalf("运行时产物不该进包: %v", names)
	}
	if len(excluded) == 0 {
		t.Fatal("剔除清单不该为空")
	}
	if _, _, _, err := a.preparePluginPublish("nope@local"); err == nil || !strings.Contains(err.Error(), "没有安装") {
		t.Fatalf("未安装插件该拒收，实得 %v", err)
	}
}

/* ---------- handleMarketPublish 的组件 / 插件分支 ---------- */

func TestPublishComponentLocksManifestValues(t *testing.T) {
	fm := newFakePublishMarket(t)
	a := newPublishTestApp(t, fm.srv.URL+"/market/manifest.json")
	installTestComponent(t, a, "demo-comp", okComponentManifest)
	// 表单想改 id/版本：都不生效（锁定为 manifest.json 的值）
	code, out := publishReq(t, a, map[string]any{
		"type": "component", "componentId": "demo-comp",
		"id": "other-id", "version": "9.9.9", "name": "手填名",
	})
	if code != 200 {
		t.Fatalf("期待 200，收到 %d：%v", code, out)
	}
	if fm.fields["type"] != "component" || fm.fields["id"] != "demo-comp" || fm.fields["version"] != "0.2.0" {
		t.Fatalf("组件 id/版本应锁定为 manifest 值，实得 %#v", fm.fields)
	}
	if fm.fields["name"] != "手填名" {
		t.Fatalf("显示名可由表单覆盖，实得 %q", fm.fields["name"])
	}
	if fm.file != "demo-comp-0.2.0.zip" {
		t.Fatalf("发布包名不对: %s", fm.file)
	}
	names := zipNamesForTest(t, fm.zip)
	if !hasZipEntry(names, "manifest.json") || hasZipEntry(names, "config.json") {
		t.Fatalf("包内容不对（manifest 在、凭据不在）: %v", names)
	}
	if out["type"] != "component" || out["excluded"] == nil {
		t.Fatalf("回执该带 type 与剔除清单: %v", out)
	}

	// 同版本重传拦截：市场在架 demo-comp v0.2.0 时再传 0.2.0 拒收
	fm.entries["component"] = []marketEntryIn{{ID: "demo-comp", Version: "0.2.0"}}
	a.marketCache = marketCacheEntry{} // 清缓存，逼重拉清单
	code, out = publishReq(t, a, map[string]any{"type": "component", "componentId": "demo-comp"})
	if code != 400 || !strings.Contains(fmt.Sprint(out["error"]), "升") {
		t.Fatalf("同版本重传该 400 并提示升号，实得 %d：%v", code, out)
	}
	// 升号后放行（服务端视角是同 id 新版本）
	writeFileForTest(t, filepath.Join(a.Components.UserDir(), "demo-comp", "manifest.json"),
		strings.Replace(okComponentManifest, "0.2.0", "0.3.0", 1))
	a.Components.Rescan()
	if code, out = publishReq(t, a, map[string]any{"type": "component", "componentId": "demo-comp"}); code != 200 {
		t.Fatalf("升号后该放行，实得 %d：%v", code, out)
	}
	if fm.fields["version"] != "0.3.0" {
		t.Fatalf("升号后版本该是 0.3.0，实得 %q", fm.fields["version"])
	}
}

func TestPublishPluginFlow(t *testing.T) {
	fm := newFakePublishMarket(t)
	a := newPublishTestApp(t, fm.srv.URL+"/market/manifest.json")
	key := installTestPlugin(t, a, "demo-plugin", "0.1.0")
	code, out := publishReq(t, a, map[string]any{"type": "plugin", "pluginKey": key})
	if code != 200 {
		t.Fatalf("期待 200，收到 %d：%v", code, out)
	}
	if fm.fields["type"] != "plugin" || fm.fields["id"] != "demo-plugin" || fm.fields["version"] != "0.1.0" {
		t.Fatalf("插件转发字段不对: %#v", fm.fields)
	}
	if fm.fields["name"] != "演示插件" {
		t.Fatalf("显示名应优先 plugin.json 的 displayName: %q", fm.fields["name"])
	}
}

/* ---------- 待审：服务端 pending + message 原样透传（三类共用） ---------- */

func TestPublishForwardsPendingMessage(t *testing.T) {
	fm := newFakePublishMarket(t)
	fm.reply = map[string]any{
		"pending": true, "message": "已上传成功，等待管理员审核，审核通过后会出现在市场里",
	}
	a := newPublishTestApp(t, fm.srv.URL+"/market/manifest.json")
	installTestComponent(t, a, "demo-comp", okComponentManifest)
	code, out := publishReq(t, a, map[string]any{"type": "component", "componentId": "demo-comp"})
	if code != 200 {
		t.Fatalf("期待 200，收到 %d：%v", code, out)
	}
	if out["pending"] != true || out["message"] != fm.reply["message"] {
		t.Fatalf("待审标记与文案该原样透传给前端: %v", out)
	}
}

/* ---------- publish-meta 端点的三类型参数 ---------- */

func TestPublishMetaByType(t *testing.T) {
	fm := newFakePublishMarket(t)
	a := newPublishTestApp(t, fm.srv.URL+"/market/manifest.json")
	installTestComponent(t, a, "demo-comp", okComponentManifest)
	key := installTestPlugin(t, a, "demo-plugin", "0.1.0")

	metaReq := func(path string) (*httptest.ResponseRecorder, *http.Request) {
		return httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil)
	}
	var meta publishMeta

	rr, req := metaReq("/api/market/publish-meta?type=component&id=demo-comp")
	a.handleMarketPublishMeta(rr, req)
	if rr.Code != 200 {
		t.Fatalf("component meta 期待 200，收到 %d：%s", rr.Code, rr.Body.String())
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &meta)
	if meta.ID != "demo-comp" || meta.Version != "0.2.0" || len(meta.Excluded) == 0 {
		t.Fatalf("component meta 不对: %#v", meta)
	}

	rr, req = metaReq("/api/market/publish-meta?type=plugin&key=" + key)
	a.handleMarketPublishMeta(rr, req)
	if rr.Code != 200 {
		t.Fatalf("plugin meta 期待 200，收到 %d：%s", rr.Code, rr.Body.String())
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &meta)
	if meta.ID != "demo-plugin" || meta.Version != "0.1.0" {
		t.Fatalf("plugin meta 不对: %#v", meta)
	}

	rr, req = metaReq("/api/market/publish-meta?type=bogus")
	a.handleMarketPublishMeta(rr, req)
	if rr.Code != 400 {
		t.Fatalf("未知 type 该 400，实得 %d", rr.Code)
	}
}
