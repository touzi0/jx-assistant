package main

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 模板灌数据的口径（md-viewer/viewer-template.html 的四个占位符）：全换掉、
// 标题按 HTML 转义、副标题按 JS 单引号串转义、正文只以 base64 出现（正文里
// 有 </script> 也破坏不了文档结构）；顺带把 CSP 内嵌进页面（srcdoc 那条路
// 没有响应头，见 mdPageCSPMeta）。
func TestRenderMDViewerPage(t *testing.T) {
	tpl := "<html><head><title>__TITLE__</title></head><body>" +
		`<script type="text/plain" id="__md__">__B64__</script>` +
		"<script>var keepRunning = __KEEP_RUNNING__;</script>" +
		"<script>$('meta').textContent = '__META__';</script></body></html>"
	body := []byte("# 标题\n\n<script>alert(1)</script>\n")
	page, err := renderMDViewerPage(tpl, "a<b>.md", "docs/a<b>'x'.md · 12 B", body)
	if err != nil {
		t.Fatal(err)
	}
	for _, ph := range []string{"__TITLE__", "__B64__", "__META__", "__KEEP_RUNNING__"} {
		if strings.Contains(page, ph) {
			t.Fatalf("占位符 %s 未被替换", ph)
		}
	}
	if !strings.Contains(page, "<title>a&lt;b&gt;.md</title>") {
		t.Fatalf("标题未按 HTML 转义：%s", page)
	}
	if !strings.Contains(page, "var keepRunning = false;") {
		t.Fatalf("__KEEP_RUNNING__ 应为 false（右栏没有独立窗口可常驻）：%s", page)
	}
	if strings.Contains(page, "<script>alert(1)") {
		t.Fatalf("正文应只以 base64 出现，不该原样落进 HTML：%s", page)
	}
	if strings.Contains(page, "a<b>'x'") {
		t.Fatalf("副标题应按 JS 字符串转义（尖括号/引号）：%s", page)
	}
	if !strings.Contains(page, "Content-Security-Policy") {
		t.Fatalf("渲染页应内嵌 CSP（connect-src 'none' 挡住 md 脚本调产品 API）：%s", page)
	}
	// 正文能从 base64 还原（模板里就是这么解码的）
	const open = `<script type="text/plain" id="__md__">`
	i := strings.Index(page, open)
	j := strings.Index(page[i:], "</script>")
	raw, err := base64.StdEncoding.DecodeString(page[i+len(open) : i+j])
	if err != nil {
		t.Fatalf("正文不是合法 base64：%v", err)
	}
	if string(raw) != string(body) {
		t.Fatalf("正文还原不一致：%q", raw)
	}
	// 模板缺占位符要报错（模板被改坏时给明确原因，而不是渲染出一页 __B64__）
	if _, err := renderMDViewerPage("<html><head></head></html>", "t", "m", body); err == nil {
		t.Fatal("模板缺占位符时应报错")
	}
}

// mdViewURL：路径形态（不用查询参数），逐段转义、前导 / 与反斜杠统一——页地址
// 带着目录结构，md 里的相对图片/链接才解析得到同一路由上。
func TestMDViewURL(t *testing.T) {
	cases := []struct{ rel, want string }{
		{"docs/说明 v2.md", "/api/md/s1/docs/%E8%AF%B4%E6%98%8E%20v2.md"},
		{"/docs/a.md", "/api/md/s1/docs/a.md"},   // 前导 / 去掉：与其它工作区端点同口径
		{`docs\sub\a.md`, "/api/md/s1/docs/sub/a.md"}, // 本机路径形态也认
	}
	for _, c := range cases {
		if got := mdViewURL("s1", c.rel); got != c.want {
			t.Fatalf("mdViewURL(%q) = %q，期望 %q", c.rel, got, c.want)
		}
	}
	// 不裂图的关键：页面地址 + md 里的相对图 → 相对资源路由（浏览器就是这么做解析的）
	base, err := url.Parse(mdViewURL("s1", "docs/说明.md"))
	if err != nil {
		t.Fatal(err)
	}
	rel, _ := url.Parse("img/pic 1.png")
	if got, want := base.ResolveReference(rel).String(), mdViewURL("s1", "docs/img/pic 1.png"); got != want {
		t.Fatalf("相对图片解析成 %q，期望 %q", got, want)
	}
}

// 右栏 md 渲染页端到端：.md 回渲染页（正文进模板、带 CSP），相对图片走同一
// 路由按真实 Content-Type 回，超大 md 退化成原文，不存在/目录一律 404。
func TestHandleMDView(t *testing.T) {
	a, main := newDispatchTestApp(t)
	ws := a.Cfg.Workspace
	if err := os.MkdirAll(filepath.Join(ws, "docs", "img"), 0o755); err != nil {
		t.Fatal(err)
	}
	const md = "# 一级标题\n\n![图](img/pic.png)\n\n| a | b |\n|---|---|\n| 1 | 2 |\n"
	if err := os.WriteFile(filepath.Join(ws, "docs", "说明.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "docs", "img", "pic.png"), []byte("\x89PNG\r\n\x1a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "big.md"), []byte(strings.Repeat("x", mdViewMaxBytes+16)), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.apiMuxOf())
	defer srv.Close()
	get := func(p string) (*http.Response, string) {
		t.Helper()
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(raw)
	}

	// 1) md → 渲染页（正文在 base64 里，CSP 响应头在）
	resp, page := get(mdViewURL(main.ID, "docs/说明.md"))
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("md 应回渲染页：%d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("Content-Security-Policy") == "" || !strings.Contains(page, "id=\"__md__\"") {
		t.Fatalf("渲染页缺 CSP 响应头或正文占位容器")
	}
	if !strings.Contains(page, "<title>说明.md</title>") {
		t.Fatalf("渲染页标题应是文件名：%s", page[:min(400, len(page))])
	}

	// 2) 相对图片：同一路由按扩展名给真实 Content-Type（不裂图）
	resp, img := get(mdViewURL(main.ID, "docs/img/pic.png"))
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" || img != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("相对图片应原样回：%d %s %q", resp.StatusCode, resp.Header.Get("Content-Type"), img)
	}

	// 3) 超大 md：按原文回（text/plain），不渲染——几 MB 铺成 DOM 会把右栏卡死
	resp, big := get(mdViewURL(main.ID, "big.md"))
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("超大 md 应退化成原文：%d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if len(big) != mdViewMaxBytes+16 {
		t.Fatalf("原文长度不符：%d", len(big))
	}

	// 4) 不存在 / 目录：404（自然错误，前端弹提示即可）
	if resp, _ := get(mdViewURL(main.ID, "docs/没有.md")); resp.StatusCode != 404 {
		t.Fatalf("不存在的 md 应 404，got %d", resp.StatusCode)
	}
	if resp, _ := get(mdViewURL(main.ID, "docs")); resp.StatusCode != 404 {
		t.Fatalf("目录应 404，got %d", resp.StatusCode)
	}
	// 5) 未知会话：404（与 open-view 同口径，不做工作区外拦截也不越误会话）
	if resp, _ := get(mdViewURL("not-a-session", "docs/说明.md")); resp.StatusCode != 404 {
		t.Fatalf("未知会话应 404，got %d", resp.StatusCode)
	}
}

// marked 单一副本注入：模板只带 /*__MARKED__*/ 占位符，启动时从 web/marked.lib.js
//（go:embed web 的同一份字节）灌入。注入链断了（模板/lib 改名、占位符被删）
// 这里第一时间红，而不是等到右栏查看器显示「解析失败」。
func TestMDViewerTemplateMarkedInjected(t *testing.T) {
	if !strings.Contains(mdViewerTemplateRaw, mdViewerMarkedSlot) {
		t.Fatalf("模板缺少 %s 占位符（模板被改坏）", mdViewerMarkedSlot)
	}
	if strings.Contains(mdViewerTemplate, mdViewerMarkedSlot) {
		t.Fatal("占位符未被替换：marked 没有注入模板")
	}
	// 注入的是能跑的 marked：UMD 头 + 挂到 globalThis.marked 的导出语句
	if !strings.Contains(mdViewerTemplate, "marked v12.0.0") || !strings.Contains(mdViewerTemplate, "e.marked=ce") {
		t.Fatal("注入内容不是 marked v12 浏览器版（web/marked.lib.js 被换错文件？）")
	}
	// 灌数据后整页可组装（renderMDViewerPage 的占位符口径未被注入破坏）
	page, err := renderMDViewerPage(mdViewerTemplate, "t.md", "m", []byte("# x"))
	if err != nil {
		t.Fatalf("注入后的模板无法灌数据: %v", err)
	}
	if strings.Contains(page, "__B64__") || strings.Contains(page, mdViewerMarkedSlot) {
		t.Fatal("渲染页残留占位符")
	}
}
