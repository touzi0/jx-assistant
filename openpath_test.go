package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// postPath 直连同一 mux 请求会话级端点，返回状态码与响应体。
func postPath(t *testing.T, srv *httptest.Server, sid, ep string, body any) (int, string) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(srv.URL+"/api/sessions/"+sid+"/"+ep, "application/json", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// TestUserGestureOpenRevealDirect 用户手势链路（open-path / reveal-path /
// delivery 的 open·reveal）只保留自然错误：路径不存在 → 404 且文案带完整
// 解析路径（前端弹明确提示、用户可手动前往）；其余一律放行（工作区内外、
// 是否登记过交付都不再校验——放行分支会真调 ShellExecute/explorer，由
// gui-test/ctxmenu-desktop-smoke.mjs 在真机上覆盖，这里只测拒绝分支）。
func TestUserGestureOpenRevealDirect(t *testing.T) {
	a, _ := newDispatchTestApp(t)
	m := &SessionMeta{ID: "t-open", Title: "用户手势直通", Mode: "task",
		Workspace: a.Cfg.Workspace, CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(m); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.Cfg.Workspace, "在的.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.apiMuxOf())
	defer srv.Close()

	// 不存在：四条端点都 404，文案带完整解析路径（body 是 JSON，反斜杠已
	// 转义，用 json.Marshal 造同形态的比对串）
	jq := func(s string) string {
		raw, _ := json.Marshal(s)
		return strings.Trim(string(raw), `"`)
	}
	for _, ep := range []string{"open-path", "reveal-path", "delivery/open", "delivery/reveal"} {
		want := filepath.Join(a.Cfg.Workspace, "不存在的文件.txt")
		code, body := postPath(t, srv, "t-open", ep, map[string]any{"path": "不存在的文件.txt"})
		if code != 404 || !strings.Contains(body, jq(want)) {
			t.Fatalf("%s 不存在应 404 且文案带完整路径 %s：%d %s", ep, want, code, body)
		}
	}

	// 预览（open-view）同口径：不存在 → 404 自然错误
	resp, err := http.Post(srv.URL+"/api/browser/open-view", "application/json",
		strings.NewReader(`{"session":"t-open","path":"不存在的文件.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 404 || !strings.Contains(string(raw), "文件不存在") {
		t.Fatalf("open-view 不存在应 404 自然错误：%d %s", resp.StatusCode, raw)
	}
}

// TestDeliveryDownloadRegisteredOnly 下载端点只放行本会话登记过的路径：交付卡
// （deliveries）与上传记录（uploads）都算——对话里双击上传记录的「下载下来」
// （远程工作区 / 远程访问端，见 app-chat.js openUploadedFile）走的就是它；
// 工作区里存在但没登记的文件、别的会话登记的文件一律 404。
func TestDeliveryDownloadRegisteredOnly(t *testing.T) {
	a, _ := newDispatchTestApp(t)
	m := &SessionMeta{ID: "t-dl", Title: "下载白名单", Mode: "task",
		Workspace: a.Cfg.Workspace, CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(m); err != nil {
		t.Fatal(err)
	}
	other := &SessionMeta{ID: "t-dl-other", Title: "别的会话", Mode: "task",
		Workspace: a.Cfg.Workspace, CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(other); err != nil {
		t.Fatal(err)
	}
	u := seedUpload(t, a, m, a.Cfg.Workspace, "报告.docx", "docx-bytes")
	seedUpload(t, a, other, a.Cfg.Workspace, "别人的.xlsx", "other-bytes")
	if err := os.WriteFile(filepath.Join(a.Cfg.Workspace, "没登记.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.apiMuxOf())
	defer srv.Close()
	get := func(sid, p string) (int, string) {
		t.Helper()
		resp, err := http.Get(srv.URL + "/api/sessions/" + sid + "/delivery/download?path=" + url.QueryEscape(p))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	// 注记里是正斜杠形态（后端 filepath.ToSlash 给前端的），登记的是 OS 原生分隔符：
	// 归一后对得上，正文原样回
	code, body := get("t-dl", filepath.ToSlash(u.CopyPath))
	if code != 200 || body != "docx-bytes" {
		t.Fatalf("上传登记的文件应可下载：状态 %d 正文 %q", code, body)
	}
	if code, _ := get("t-dl", "没登记.txt"); code != 404 {
		t.Fatalf("工作区里没登记的文件应 404：%d", code)
	}
	if code, _ := get("t-dl", filepath.ToSlash(filepath.Join(a.Cfg.Workspace, ".jxai", "uploads", "别人的.xlsx"))); code != 404 {
		t.Fatalf("别的会话登记的文件应 404：%d", code)
	}
}
