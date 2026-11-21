// sitesproxy.go —— 企业小程序（jxai-server 的 /sites 网页托管平台）内嵌半边。
//
// 设计意图（2026-10-30 用户口径）：企业小程序必须像企业技能市场一样**在应用内
// 打开**，不能跳系统浏览器。此前 iframe 直连服务端 HTTPS 会被浏览器自己的证书
// 校验静默拒绝（自签证书/用 IP 访问域名证书，连"继续访问"按钮都没有）——企业技能
// 市场从来没这个问题，因为它从不让前端直连服务端：全部经本端 Go 的
// companyTransport（tlspolicy.go：域名/内网 IP 免校验）代抓。这里复用同一经验：
// 本端做透明反向代理，iframe 加载本地同源地址（http://127.0.0.1:<port>/sites），
// 证书问题从根上消失，服务端也不用再照顾"浏览器能不能加载"（2228 明文口不再依赖）。
//
// 挂载路径与服务端 jxaiserver/sites.go 的平台页依赖一一对应（页面内部的绝对路径
// fetch 因此天然命中本端同路径代理）：
//
//	/sites            平台页本体
//	/sites-assets/    平台页静态资源（qr.js 等）
//	/api/sites/*      站点管理接口（list/publish/delete/cover…）
//	/api/whoami       当前访问者身份
//	/s/               已发布站点与其数据文件接口（平台页里直接查数据清单）
//
// 身份：服务端取值顺序是 X-Jxai-User 头 > ?user= 查询参数 > cookie（浏览器 fetch
// 的 header 不允许中文，网页端只能走查询参数）。代理统一注入本机发布者姓名头——
// 仅在页面没显式传身份时（?user= 或自带头），保证平台页名字框临时换名仍然生效。
package main

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// sitesProxyTarget 从市场地址推导代理目标 origin（同一台 jxai-server）；没配置
// 或不合法返回 nil。
func (a *App) sitesProxyTarget() *url.URL {
	raw := strings.TrimSpace(a.Cfg.MarketURL)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host}
}

// handleSitesProxy 透明转发到市场服务器：路径原样、body/cookie/响应头透传，
// Transport 用 companyTransport（复用市场同款证书策略）。
func (a *App) handleSitesProxy(w http.ResponseWriter, r *http.Request) {
	target := a.sitesProxyTarget()
	if target == nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("还没配置市场服务器：请到 设置 · 网络访问 填「市场服务器」，企业小程序与它同一台服务端。"))
		return
	}
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			// Origin/Referer 改写成目标源：本代理已把 Host 换成市场服务器，浏览器
			// 带来的 Origin（http://127.0.0.1:本机端口）若原样透传，服务端数据 API
			// 的同源校验（sameOriginOK：Origin.host vs r.Host）必然不一致——应用内
			// 打开小程序一切正常（读接口 GET 不带 Origin），一到填数据/提交就被
			// 403「跨站写入被拒绝」（2026-11-04 现场报障，已实测复现）。顺带不再
			// 把本机端口泄露给服务端日志。
			if o := req.Header.Get("Origin"); o != "" {
				req.Header.Set("Origin", target.Scheme+"://"+target.Host)
			}
			if ref := req.Header.Get("Referer"); ref != "" {
				req.Header.Set("Referer", target.Scheme+"://"+target.Host+req.URL.Path)
			}
			// 页面没显式传身份（平台页名字框的 ?user= 或自带头）才注入本机
			// 发布者姓名——服务端取值头优先，注入了就会盖过页面，必须让着页面
			if req.Header.Get(marketUserHeader) == "" && req.URL.Query().Get("user") == "" {
				if u := a.marketUser(); u != "" {
					req.Header.Set(marketUserHeader, u)
				}
			}
		},
		Transport: companyTransport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("企业小程序连不上服务端（" + target.Host + "）：" + err.Error()))
		},
	}
	proxy.ServeHTTP(w, r)
}
