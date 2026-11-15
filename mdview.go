package main

// mdview.go —— 右栏里的 Markdown 渲染页。
//
// 产品自带的 MD 查看器（md-viewer/）本质就是一个渲染网页 + 一个 WebView 外壳，
// 右栏这边复用同一份渲染网页（自带的 marked、GitHub 风格样式、目录侧栏），
// 只把「标题 / 正文 / 副标题」灌进模板，不另写第三套 markdown 渲染器：右栏看到
// 的就是双击 .md 用 MD 查看器看到的那一页。模板进 exe 靠 go:embed，装完不依赖
// md-viewer 目录还在（随包只带 exe + 说明）。
//
// 地址用【路径形态】而不是 ?sid=&path= 查询参数：
//     /api/md/<会话id>/<工作区相对路径>.md
// md 里的相对图片/链接因此按 URL 语义自动落到同一路由上（页在 …/docs/a.md，
// 文中 ./img/x.png → …/docs/img/x.png），不裂图、也不用改写正文。同一个路由
// 两种回法：.md 回渲染网页，其余按真实 Content-Type 原样回文件（图片、附件）。

import (
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strings"
)

//go:embed md-viewer/viewer-template.html
var mdViewerTemplateRaw string

// marked 渲染库全仓只存 web/marked.lib.js 一份：主 exe 里 go:embed 一次
//（server.go 的 //go:embed web 与这里的注入共用同一份字节），启动时灌进
// 查看器模板的 /*__MARKED__*/ 占位符；独立 MD查看器.exe 由 md-viewer/build.ps1
// 构建时注入同一份。注入源异常时退回原文——查看器页会显示「解析失败」，
// 主功能不受影响。
const mdViewerMarkedSlot = "/*__MARKED__*/"

var mdViewerTemplate = func() string {
	lib, err := webFS.ReadFile("web/marked.lib.js")
	if err != nil || !strings.Contains(mdViewerTemplateRaw, mdViewerMarkedSlot) || strings.Contains(string(lib), "</script>") {
		return mdViewerTemplateRaw
	}
	return strings.Replace(mdViewerTemplateRaw, mdViewerMarkedSlot, string(lib), 1)
}()

const (
	mdViewPrefix = "/api/md/" // 右栏 md 渲染页路由前缀（见 mdViewURL）
	// mdViewMaxBytes 渲染上限：超过就按原文回（text/plain）。几 MB 的 md 铺成
	// DOM 会把右栏 WebView 卡住——原文可读可复制，比界面僵死强。
	mdViewMaxBytes = 4 << 20
)

// mdPageCSP 渲染页的内容安全策略。md 里的内联 HTML/脚本照旧执行（与 MD 查看器
// 同策略，marked 不过滤），但这一页现在跑在产品同源（127.0.0.1 的 HTTP 源）里，
// 直接放行就等于给 md 文件一把调用产品 API 的钥匙（fetch 本地 /api/…）。
// 所以：脚本与样式照旧内联，网络请求（fetch/XHR/WebSocket）与表单提交一律禁止，
// 图片/音视频按常态放行（md 里贴的外链图、相对图都要能显示）。
const mdPageCSP = "default-src 'none'; img-src * data: blob:; media-src * data: blob:; " +
	"style-src 'unsafe-inline'; script-src 'unsafe-inline'; font-src * data:; " +
	"connect-src 'none'; form-action 'none'; base-uri 'none'; frame-ancestors 'self'"

// mdPageCSPMeta 同一策略的 meta 版（去掉 frame-ancestors：该指令在 meta 里被
// 忽略，带着只会刷一条控制台警告）。手机外壳的远程形态把页面 HTML 经隧道取回
// 后写进 iframe srcdoc，那条路上没有响应头，靠这行 meta 兜住。
var mdPageCSPMeta = strings.TrimSuffix(mdPageCSP, "; frame-ancestors 'self'")

// isMarkdownPath 判断是不是 Markdown 文档（右栏渲染页 vs 原样回文件的分水岭）。
// filepath.Ext 在两种平台上都同时认 / 与 \ 分隔符，前端传 /、本机路径带 \ 都不误判。
func isMarkdownPath(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".md", ".markdown", ".mdown", ".mkd":
		return true
	}
	return false
}

// mdViewURL 右栏 md 渲染页地址（相对路径，宿主由调用方拼）：会话内的工作区相对
// 路径逐段转义；前导 / 去掉——与其它工作区端点同口径，相对路径基于会话工作区。
func mdViewURL(sid, rel string) string {
	segs := strings.Split(strings.TrimPrefix(strings.ReplaceAll(rel, "\\", "/"), "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return mdViewPrefix + url.PathEscape(sid) + "/" + strings.Join(segs, "/")
}

// jsStringEscape 把文本安全放进模板里的单引号 JS 字符串（'__META__'）：转义
// 反斜杠/单引号/换行，并把 </script> 这类尖括号转成 \u003c，免得文件名里有
// 尖括号把文档截断。
func jsStringEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\'':
			b.WriteString(`\'`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '<':
			b.WriteString(`\u003c`)
		case '>':
			b.WriteString(`\u003e`)
		case '&':
			b.WriteString(`\u0026`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// renderMDViewerPage 把正文灌进查看器模板。占位符口径与 md-viewer 一致：
// __TITLE__（<title> 与页头标题两处）、__B64__（正文 base64，模板自己解码——
// 正文里有什么字符都不会破坏 HTML 结构）、__META__（页头副标题，JS 单引号串）、
// __KEEP_RUNNING__（关闭窗口后后台待命：右栏没有独立窗口，恒 false）。
func renderMDViewerPage(tpl, title, meta string, body []byte) (string, error) {
	for _, ph := range []string{"__TITLE__", "__B64__", "__META__", "__KEEP_RUNNING__"} {
		if !strings.Contains(tpl, ph) {
			return "", errors.New("md 查看器模板缺少占位符 " + ph + "（模板文件被改坏：md-viewer/viewer-template.html）")
		}
	}
	return strings.NewReplacer(
		"__TITLE__", html.EscapeString(title),
		"__META__", jsStringEscape(meta),
		"__B64__", base64.StdEncoding.EncodeToString(body),
		"__KEEP_RUNNING__", "false",
	).Replace(mdViewCSPMeta(tpl)), nil
}

// mdViewCSPMeta 把 CSP 以 <meta> 内嵌进页面（响应头之外的第二道，覆盖 srcdoc 那条路，
// 见 mdPageCSPMeta）。模板没有 <head> 就原样返回，由响应头保底。
func mdViewCSPMeta(tpl string) string {
	i := strings.Index(tpl, "<head>")
	if i < 0 {
		return tpl
	}
	tag := "\n<meta http-equiv=\"Content-Security-Policy\" content=\"" + html.EscapeString(mdPageCSPMeta) + "\">"
	return tpl[:i+len("<head>")] + tag + tpl[i+len("<head>"):]
}

// humanSize 副标题里的文件大小（只看量级，不做本地化）
func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// handleMDView 右栏 Markdown 渲染页：GET /api/md/{sid}/{path...}
// 与 open-view / browser-view 同口径（用户手势链路：只查存在性，不经模型权限
// 门控，也不进 call_api 目录——AI 调不到它）。
func (a *App) handleMDView(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	rel := r.PathValue("path")
	m, abs, ok := a.workspaceFileForView(sid, rel)
	if !ok {
		http.NotFound(w, r)
		return
	}
	tctx := &toolContext{app: a, sess: m}
	isDir, size, err := tctx.envStat(abs)
	if err != nil || isDir {
		http.NotFound(w, r)
		return
	}
	if isMarkdownPath(rel) && size <= mdViewMaxBytes {
		data, rerr := tctx.envReadFile(abs)
		if rerr != nil {
			http.NotFound(w, r)
			return
		}
		page, perr := renderMDViewerPage(mdViewerTemplate, path.Base(rel), rel+" · "+humanSize(size), data)
		if perr != nil {
			writeErr(w, http.StatusInternalServerError, "渲染页模板异常: "+perr.Error())
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", mdPageCSP) // 见 mdPageCSP：md 脚本不能反过来调产品 API
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(page))
		return
	}
	// 非 md（相对图片/附件/外链文件）或超限的 md：原样回文件，按扩展名给真实
	// Content-Type（与 /api/browser/view 同一张表）。超限 md 落到这里是 text/plain，
	// 浏览器原文显示——能读能拷，不至于把右栏卡死。
	data, err := tctx.envReadFile(abs)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", viewContentType(rel))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}
