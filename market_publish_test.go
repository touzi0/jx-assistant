package main

// 市场发布单测（market_publish.go）：假市场服务器的 /api/market/publish 收端，
// 覆盖「先装后传」（只发已装用户技能、包由后端从安装目录现场打包）、元信息
// 补全（displayName 优先）、长度校验、各种拒绝路径与缓存失效。

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

/* ---------- 假市场（清单 + 用户发布端点） ---------- */

type fakePublishMarket struct {
	srv    *httptest.Server
	fields map[string]string
	user   string // 收到的 X-Jxai-User 头（身份=发布者姓名）
	device string // 收到的 X-Jxai-Device 头（三要素审计的设备名）
	file   string
	zip    []byte
	code   int
	errMsg string
	hits   int
	// entries 清单里的在架条目（type → 列表）：发布前同版本拦截的闸门读它；
	// 不设则为空清单
	entries map[string][]marketEntryIn
	// reply 覆盖成功响应（默认 {entry:…}；测待审透传时给 pending+message）
	reply map[string]any
}

func newFakePublishMarket(t *testing.T) *fakePublishMarket {
	t.Helper()
	fm := &fakePublishMarket{fields: map[string]string{}, code: 200, entries: map[string][]marketEntryIn{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /market/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		m := map[string]any{"name": "测试市场", "updatedAt": "2026-10-09T00:00:00Z", "skills": []marketEntryIn{}}
		for typ, list := range fm.entries {
			m[typ+"s"] = list
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m)
	})
	mux.HandleFunc("POST /api/market/publish", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		fm.hits++
		fm.user = r.Header.Get(marketUserHeader)
		fm.device = r.Header.Get(marketDeviceHeader)
		for _, k := range []string{"type", "id", "name", "version", "desc", "tags", "owner"} {
			fm.fields[k] = r.FormValue(k)
		}
		if f, hdr, err := r.FormFile("file"); err == nil {
			fm.file = hdr.Filename
			fm.zip, _ = io.ReadAll(f)
			f.Close()
		}
		w.Header().Set("Content-Type", "application/json")
		if fm.code >= 400 {
			w.WriteHeader(fm.code)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": fm.errMsg})
			return
		}
		if fm.reply != nil {
			_ = json.NewEncoder(w).Encode(fm.reply)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"entry": map[string]any{
			"id": fm.fields["id"], "version": fm.fields["version"],
			"zip": "/market/items/skill/" + fm.fields["id"] + "/" + fm.fields["version"] + ".zip",
		}})
	})
	fm.srv = httptest.NewServer(mux)
	t.Cleanup(fm.srv.Close)
	return fm
}

func newPublishTestApp(t *testing.T, marketURL string) *App {
	a := newMarketTestApp(t, marketURL)
	a.Cfg.PublisherName = "张三"                 // 发布者姓名：身份头的来源
	a.Cfg.RemoteAccess.MyID = "jx-dev01"         // 远程接入设备号：设备头的来源
	a.CfgPath = filepath.Join(t.TempDir(), "config.json")
	return a
}

/* ---------- 安装技能（先装后传的「装」）与请求构造 ---------- */

// installTestSkill 往 技能/user/<dir>/ 直接放一个技能（绕过 addUserSkill 的拷贝，
// 等价于装好的状态），返回技能 id（=目录名）。
func installTestSkill(t *testing.T, a *App, dir string, files map[string]string) string {
	t.Helper()
	userDir, err := a.userSkillsDir()
	if err != nil {
		t.Fatal(err)
	}
	skill := filepath.Join(userDir, dir)
	if err := os.MkdirAll(skill, 0755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		p := filepath.Join(skill, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func publishReq(t *testing.T, a *App, body map[string]any) (int, map[string]any) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/market/publish", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	a.handleMarketPublish(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func zipNamesForTest(t *testing.T, data []byte) []string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("发布出去的包不是合法 zip: %v", err)
	}
	out := []string{}
	for _, f := range zr.File {
		out = append(out, f.Name)
	}
	return out
}

func zipBytesForTest(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const okSkillMD = "---\nname: wechat-chat-ops\ndescription: 驱动真实微信 PC 端\n---\n\n正文\n"

/* ---------- 名字 → 版本号（用户定的规则） ---------- */

func TestSkillVersionFromName(t *testing.T) {
	cases := map[string]string{
		"微信操作_技能包_v1.3.1_20261009.zip": "1.3.1", // v 后一定是版本号，_时间戳不是
		"微信操作_技能包_v1.3.0_20261009.zip": "1.3.0",
		"wechat-chat-ops-1.2.0.zip":    "1.2.0", // 无 v 的裸版本
		"工具_v2_20261009.zip":           "2",
		"技能包_1.3.1+20261009.zip":       "1.3.1", // +时间戳只取版本本体
		"技能包-v1.3.1-20261009.zip":      "1.3.1",
		"site-map_v10.20.30.zip":       "10.20.30",
		"没有版本的技能包.zip":                 "",
		"OCR 识别-v0.9.zip":              "0.9",
	}
	for in, want := range cases {
		if got := skillVersionFromName(in); got != want {
			t.Errorf("skillVersionFromName(%q) = %q，期待 %q", in, got, want)
		}
	}
}

func TestSkillIDFromName(t *testing.T) {
	cases := map[string]string{
		"微信操作_技能包_v1.3.1_20261009.zip": "微信操作_技能包",
		"wechat-chat-ops-1.2.0.zip":    "wechat-chat-ops",
		"APK":                          "APK",
		"Office 文档/":                   "Office 文档",
		"技能包":                          "技能包",
	}
	for in, want := range cases {
		if got := skillIDFromName(in); got != want {
			t.Errorf("skillIDFromName(%q) = %q，期待 %q", in, got, want)
		}
	}
}

/* ---------- 正常发布：从已装目录打包，displayName 优先 ---------- */

func TestPublishInstalledSkill(t *testing.T) {
	fm := newFakePublishMarket(t)
	a := newPublishTestApp(t, fm.srv.URL+"/market/manifest.json")
	skillID := installTestSkill(t, a, "wechat-chat-ops", map[string]string{
		"SKILL.md":       "---\nname: wechat-chat-ops\ndisplayName: 微信操作\ndescription: 驱动真实微信 PC 端\n---\n\n正文\n",
		"scripts/run.py": "print(1)\n",
	})
	code, out := publishReq(t, a, map[string]any{"skillId": skillID})
	if code != 200 {
		t.Fatalf("期待 200，收到 %d：%v", code, out)
	}
	if fm.user != "张三" {
		t.Fatalf("身份头应带发布者姓名，实得 %q", fm.user)
	}
	if fm.device != "jx-dev01" {
		t.Fatalf("三要素审计应带 X-Jxai-Device 头（值=远程接入设备号），实得 %q", fm.device)
	}
	if fm.fields["owner"] != "" {
		t.Fatalf("owner 不再走表单字段（身份在头里），实得 %q", fm.fields["owner"])
	}
	if fm.fields["type"] != "skill" || fm.fields["id"] != "wechat-chat-ops" {
		t.Fatalf("转发字段不对: %#v", fm.fields)
	}
	if fm.fields["name"] != "微信操作" {
		t.Fatalf("显示名应优先 SKILL.md 的 displayName: %#v", fm.fields)
	}
	if fm.file != "wechat-chat-ops-1.0.0.zip" {
		t.Fatalf("发布包名不对: %s", fm.file)
	}
	names := zipNamesForTest(t, fm.zip)
	if !hasZipEntry(names, "SKILL.md") || !hasZipEntry(names, "scripts/run.py") {
		t.Fatalf("包内容不对（应从安装目录原样打包）: %v", names)
	}
	if out["sha256"] == "" {
		t.Fatalf("回执缺 sha256: %v", out)
	}
}

func TestPublishMetaFallback(t *testing.T) {
	fm := newFakePublishMarket(t)
	a := newPublishTestApp(t, fm.srv.URL+"/market/manifest.json")
	// 中文目录名 + 英文 frontmatter 名 + manifest 版本：id/名称/版本全靠推导
	installTestSkill(t, a, "微信操作", map[string]string{
		"SKILL.md":      "---\nname: wechat-chat-ops\ndescription: 描述来自 SKILL.md\nversion: 9.9.9\ntags: 办公, 自动化\n---\n\n正文\n",
		"manifest.json": `{"id":"wxops","name":"微信操作（manifest）","version":"1.2.3"}`,
	})
	code, out := publishReq(t, a, map[string]any{"skillId": "微信操作"})
	if code != 200 {
		t.Fatalf("期待 200，收到 %d：%v", code, out)
	}
	if out["id"] != "wxops" {
		t.Fatalf("id 应优先包内 manifest.json 的 id，实得 %v", out["id"])
	}
	if fm.fields["name"] != "微信操作（manifest）" {
		t.Fatalf("显示名回退链：displayName（无）→ manifest.name: %#v", fm.fields)
	}
	if fm.fields["version"] != "1.2.3" {
		t.Fatalf("版本应优先 manifest.json，实得 %q", fm.fields["version"])
	}
	if fm.fields["tags"] != "办公,自动化" {
		t.Fatalf("标签应从 SKILL.md 补全，实得 %q", fm.fields["tags"])
	}
	// 显式字段优先
	fm2 := newFakePublishMarket(t)
	b := newPublishTestApp(t, fm2.srv.URL+"/market/manifest.json")
	installTestSkill(t, b, "微信操作", map[string]string{
		"SKILL.md": okSkillMD,
	})
	if code, out = publishReq(t, b, map[string]any{
		"skillId": "微信操作", "name": "手填名", "version": "2.0.0",
		"description": "手填描述", "tags": []string{"手工"},
	}); code != 200 {
		t.Fatalf("期待 200，收到 %d：%v", code, out)
	}
	if fm2.fields["name"] != "手填名" || fm2.fields["version"] != "2.0.0" ||
		fm2.fields["desc"] != "手填描述" || fm2.fields["tags"] != "手工" {
		t.Fatalf("显式字段应优先，实得 %#v", fm2.fields)
	}
}

func TestPublishSkillKeepsCacheInvalidated(t *testing.T) {
	fm := newFakePublishMarket(t)
	a := newPublishTestApp(t, fm.srv.URL+"/market/manifest.json")
	if _, err := a.fetchMarketManifest(a.Cfg.MarketURL, false); err != nil {
		t.Fatal(err)
	}
	if a.marketCache.man == nil {
		t.Fatal("前置：清单缓存应已填充")
	}
	installTestSkill(t, a, "x", map[string]string{"SKILL.md": okSkillMD})
	if code, out := publishReq(t, a, map[string]any{"skillId": "x"}); code != 200 {
		t.Fatalf("期待 200，收到 %d：%v", code, out)
	}
	if a.marketCache.man != nil {
		t.Fatal("发布成功后应清掉清单缓存，否则前端刷新看不到新条目")
	}
}

/* ---------- 拒绝路径 ---------- */

func TestPublishRejectsNotInstalled(t *testing.T) {
	fm := newFakePublishMarket(t)
	a := newPublishTestApp(t, fm.srv.URL+"/market/manifest.json")
	code, out := publishReq(t, a, map[string]any{"skillId": "没装过的技能"})
	if code != 400 || !strings.Contains(fmt.Sprint(out["error"]), "先在 设置 · 技能 里安装") {
		t.Fatalf("未安装的技能应 400 且指路设置页，实得 %d：%v", code, out)
	}
	if fm.hits != 0 {
		t.Fatal("校验没过就不该打扰市场服务器")
	}
}

func TestPublishRejectsBadAndBuiltinID(t *testing.T) {
	fm := newFakePublishMarket(t)
	a := newPublishTestApp(t, fm.srv.URL+"/market/manifest.json")
	installTestSkill(t, a, "demo", map[string]string{"SKILL.md": okSkillMD})
	if code, out := publishReq(t, a, map[string]any{"skillId": "demo", "id": "pdf"}); code != 400 ||
		!strings.Contains(fmt.Sprint(out["error"]), "自带技能") {
		t.Fatalf("自带技能 id 应 400，实得 %d：%v", code, out)
	}
	if code, out := publishReq(t, a, map[string]any{"skillId": "demo", "id": "中文"}); code != 400 ||
		!strings.Contains(fmt.Sprint(out["error"]), "小写字母/数字/中划线") {
		t.Fatalf("纯中文 id 应 400，实得 %d：%v", code, out)
	}
	if fm.hits != 0 {
		t.Fatal("校验没过就不该打扰市场服务器")
	}
}

func TestPublishLengthChecks(t *testing.T) {
	fm := newFakePublishMarket(t)
	a := newPublishTestApp(t, fm.srv.URL+"/market/manifest.json")
	installTestSkill(t, a, "demo", map[string]string{"SKILL.md": okSkillMD})
	longName := strings.Repeat("名", 65)
	if code, out := publishReq(t, a, map[string]any{"skillId": "demo", "name": longName}); code != 400 ||
		!strings.Contains(fmt.Sprint(out["error"]), "64") {
		t.Fatalf("超长名称应 400，实得 %d：%v", code, out)
	}
	longDesc := strings.Repeat("述", 1025)
	if code, out := publishReq(t, a, map[string]any{"skillId": "demo", "description": longDesc}); code != 400 ||
		!strings.Contains(fmt.Sprint(out["error"]), "1024") {
		t.Fatalf("超长描述应 400，实得 %d：%v", code, out)
	}
	// 边界值放行
	if code, out := publishReq(t, a, map[string]any{
		"skillId": "demo", "name": strings.Repeat("名", 64), "description": strings.Repeat("述", 1024),
	}); code != 200 {
		t.Fatalf("上限内的名称/描述应放行，实得 %d：%v", code, out)
	}
	if fm.hits != 1 {
		t.Fatalf("只有边界值那一次应到达市场服务器，实得 hits=%d", fm.hits)
	}
}

func TestPublishUnconfiguredAndServerReject(t *testing.T) {
	a := newPublishTestApp(t, "")
	installTestSkill(t, a, "x", map[string]string{"SKILL.md": okSkillMD})
	if code, out := publishReq(t, a, map[string]any{"skillId": "x"}); code != 400 ||
		!strings.Contains(fmt.Sprint(out["error"]), "未配置市场服务器") {
		t.Fatalf("未配置市场应 400，实得 %d：%v", code, out)
	}

	fm := newFakePublishMarket(t)
	fm.code, fm.errMsg = 409, "条目是「李四」发布的，只能由本人更新"
	b := newPublishTestApp(t, fm.srv.URL+"/market/manifest.json")
	installTestSkill(t, b, "x", map[string]string{"SKILL.md": okSkillMD})
	code, out := publishReq(t, b, map[string]any{"skillId": "x"})
	if code != 409 || !strings.Contains(fmt.Sprint(out["error"]), "只能由本人更新") {
		t.Fatalf("应原样转达市场服务器的拒绝（归属校验在服务端），实得 %d：%v", code, out)
	}
}

func TestPublishRequiresPublisherName(t *testing.T) {
	fm := newFakePublishMarket(t)
	a := newPublishTestApp(t, fm.srv.URL+"/market/manifest.json")
	a.Cfg.PublisherName = "" // 没设发布者姓名
	installTestSkill(t, a, "x", map[string]string{"SKILL.md": okSkillMD})
	code, out := publishReq(t, a, map[string]any{"skillId": "x"})
	if code != 400 || !strings.Contains(fmt.Sprint(out["error"]), "发布者姓名") {
		t.Fatalf("没设发布者姓名应 400，实得 %d：%v", code, out)
	}
	if fm.hits != 0 {
		t.Fatal("没身份就不该打扰市场服务器")
	}
}

/* ---------- 包体规整（防御路径：安装目录理论上总是干净的，单测兜住逻辑） ---------- */

func TestRewriteSkillZipNestedPack(t *testing.T) {
	// 真实技能包的形状：外层包目录 + 安装说明.md + 内层技能目录 → 规整成「技能目录在包根」
	pack, err := inspectSkillZip(zipBytesForTest(t, map[string]string{
		"微信操作_技能包/安装说明.md":       "# 安装说明\n",
		"微信操作_技能包/微信操作/SKILL.md": "---\nname: wechat-chat-ops\ndescription: 嵌套包\n---\n\n正文\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	fixed, changed, err := rewriteSkillZip(zipBytesForTest(t, map[string]string{
		"微信操作_技能包/安装说明.md":       "# 安装说明\n",
		"微信操作_技能包/微信操作/SKILL.md": "---\nname: wechat-chat-ops\ndescription: 嵌套包\n---\n\n正文\n",
	}), pack)
	if err != nil || !changed {
		t.Fatalf("嵌套包应被规整: changed=%v err=%v", changed, err)
	}
	names := zipNamesForTest(t, fixed)
	for _, w := range []string{"微信操作/SKILL.md", "微信操作/安装说明.md"} {
		if !hasZipEntry(names, w) {
			t.Fatalf("规整后的包里缺 %s：%v", w, names)
		}
	}
	for _, n := range names {
		if strings.HasPrefix(n, "微信操作_技能包") {
			t.Fatalf("外层包目录应被剥掉: %v", names)
		}
	}
}

func TestRewriteSkillZipStrayRootFiles(t *testing.T) {
	// 「技能目录 + 根级散文件」：散文件并进技能目录，包根只剩技能目录
	data := zipBytesForTest(t, map[string]string{
		"微信操作/SKILL.md":     okSkillMD,
		"微信操作/scripts/x.py": "print(1)\n",
		"安装说明.md":           "# 说明\n",
	})
	pack, err := inspectSkillZip(data)
	if err != nil {
		t.Fatal(err)
	}
	fixed, changed, err := rewriteSkillZip(data, pack)
	if err != nil || !changed {
		t.Fatalf("应规整: changed=%v err=%v", changed, err)
	}
	names := zipNamesForTest(t, fixed)
	if !hasZipEntry(names, "微信操作/安装说明.md") {
		t.Fatalf("根级散文件应并入技能目录: %v", names)
	}
	tops := map[string]bool{}
	for _, n := range names {
		if i := strings.Index(n, "/"); i > 0 {
			tops[n[:i]] = true
		} else {
			tops[n] = true
		}
	}
	if len(tops) != 1 || !tops["微信操作"] {
		t.Fatalf("规整后包根应只剩技能目录: %v", names)
	}
}
