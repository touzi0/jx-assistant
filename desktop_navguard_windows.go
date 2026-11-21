//go:build windows

package main

// 主窗口导航守卫：外链永不出壳。
//
// 背景（2026-10-31 客户故障）：「确认更新 / 下载完整安装包」用 <a download>
// 的 click() 触发下载——跨源 URL 上 download 属性会被浏览器忽略（规范行为），
// 实际效果是主窗口顶层导航到官网直链；官网不可达时（运营商换 IP 的 DDNS
// 滞后窗口，TCP 连死 IP 直到超时）导航以 Edge 错误页收场，铺满整个主界面，
// 桌面壳没有后退 UI，客户只能重启软件。守卫在 WebView 层把一切非本机源的
// 顶层导航拦下、转交系统默认浏览器：无论前端哪条路径触发（a.click、链接
// Ctrl/中键、location 跳转），都不可能再把主界面顶掉——失败只会发生在系统
// 浏览器里，与本应用无关。
//
// 只挂 NavigationStarting（顶层导航的唯一入口），不接管 NewWindowRequested：
// window.open 走 WebView2 默认弹窗、不顶主界面，本守卫目标之外的收益不值得
// 再冒 vtbl 逆向风险（NWR args 布局与 NavigationStarting 不同，逐槽实证成本
// 是整进程崩溃；右栏面板的原生 NWR 处理器已于 2026-11-18 移除，改由文档创建
// 脚本原地收敛——其 args vtbl 错位当日实测实锤为点 _blank 链接整进程闪退）。

import (
	"log"
	"net/url"
	"strings"
	"unsafe"

	edge "github.com/jchv/go-webview2/pkg/edge"
)

// navStartArgsVtbl 镜像 ICoreWebView2NavigationStartingEventArgs（未随绑定库
// 导出）。槽位为 2026-10-31 在 Chromium 156 WebView2 上逐槽实测定稿（与老版
// SDK 头文件声明序不同，新版运行时重排过；盲按头文件排布会把 get_RequestHeaders
// 当 put_Cancel 调——写访问冲突整进程崩溃）：槽3 get_Uri（返回 LPWSTR，
// 官方文档要求 CoTaskMemFree 释放）、槽8 put_Cancel、槽10 get_NavigationId。
type navStartArgsVtbl struct {
	queryInterface, addRef, release             edge.ComProc
	getUri, getIsUserInitiated, getIsRedirected edge.ComProc
	getRequestHeaders, slot7                    edge.ComProc // 占位（get_Cancel 等，不调用）
	putCancel, slot9                            edge.ComProc // 槽9 占位
	getNavigationId                             edge.ComProc
}

var (
	navGuardHandler *evtHandler // NavigationStarting 回调（全局保活：COM 侧只存裸指针）
	navGuardToken   int64
)

// navGuardAllow 判定顶层导航目标是否允许留在主 WebView 内：本机服务源
// （127.0.0.1/localhost/[::1] 任意端口）与内部页（about/data/blob/devtools）
// 放行；其余（外网、内网他机、file: 等）一律出壳。
func navGuardAllow(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false // 解析不了的导航不放行，拦下最安全
	}
	switch strings.ToLower(u.Scheme) {
	case "about", "data", "blob", "devtools":
		return true
	case "http", "https":
		h := u.Hostname()
		return h == "127.0.0.1" || strings.EqualFold(h, "localhost") || h == "::1"
	}
	return false
}

// attachNavGuard 给主 WebView 挂导航守卫（每轮前端创建后调用，热重启重建窗口
// 同样覆盖）。回调发生在 WebView2 原生栈上，evtHandler 内置 recover 兜底。
func attachNavGuard(wv *edge.ICoreWebView2) {
	if wv == nil {
		return
	}
	navGuardHandler = newEvtHandler(func(sender, args unsafe.Pointer) {
		if args == nil {
			return
		}
		v := *(**navStartArgsVtbl)(args)
		var uri *uint16
		_, _, _ = v.getUri.Call(uintptr(args), uintptr(unsafe.Pointer(&uri)))
		u := readWVString(uri)
		if navGuardAllow(u) {
			return
		}
		_, _, _ = v.putCancel.Call(uintptr(args), 1)
		log.Printf("[导航守卫] 拦截主窗口顶层导航并转系统浏览器: %s", u)
		navGuardOpen(u)
	})
	var tok int64
	_, _, _ = wvVtblOf(wv).addNavigationStarting.Call(
		uintptr(unsafe.Pointer(wv)), uintptr(unsafe.Pointer(navGuardHandler)), uintptr(unsafe.Pointer(&tok)))
	navGuardToken = tok
}

// detachNavGuard 反注册（前端收尾时调用，与 attach 配对）。
func detachNavGuard(wv *edge.ICoreWebView2) {
	if wv == nil || navGuardHandler == nil {
		return
	}
	_, _, _ = wvVtblOf(wv).removeNavigationStarting.Call(
		uintptr(unsafe.Pointer(wv)), uintptr(unsafe.Pointer(navGuardHandler)), uintptr(unsafe.Pointer(&navGuardToken)))
	navGuardHandler = nil
}

// navGuardOpen 把拦下的目标交给系统默认浏览器。回调栈上不阻塞，异步执行；
// 只转 http/https——其余 scheme（file: 等）没有安全的系统去处，仅拦截记日志。
func navGuardOpen(u string) {
	low := strings.ToLower(u)
	if !strings.HasPrefix(low, "http://") && !strings.HasPrefix(low, "https://") {
		log.Printf("[导航守卫] 非 http(s) 目标不转浏览器，仅拦截: %s", u)
		return
	}
	go func() {
		defer panelRecover("导航守卫转系统浏览器")
		if err := openPathWithDefaultApp(u); err != nil {
			log.Printf("[导航守卫] 系统浏览器打开失败: %v（%s）", err, u)
		}
	}()
}
