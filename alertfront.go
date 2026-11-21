package main

// 久等提醒（ask_alert）的「把提醒顶到用户眼前」跨平台半边：
//   - alertFrontText：窗口标题/系统通知里那一行字（会话标题 + 问题摘要）；
//   - handleDesktopFront：/api/desktop/front 的自测/兜底入口。
// 真正动作在各平台文件里：alertfront_windows.go（还原并置前 + 任务栏闪烁 +
// 托盘通知兜底）、alertfront_other.go（空实现）。
//
// 为什么需要它：提醒浮窗是应用内 UI，软件在后台或最小化时用户根本看不见——
// 全屏变蓝也白变。所以提醒触发时（与叮声同一时刻）要把窗口拉回眼前。

import (
	"encoding/json"
	"net/http"
	"strings"
)

// alertFrontText 系统通知/日志里的一句话提示：会话标题 + 问题摘要（超长截断）。
func alertFrontText(sessionTitle, question string) string {
	q := strings.TrimSpace(question)
	if r := []rune(q); len(r) > 120 {
		q = string(r[:120]) + "…"
	}
	if q == "" {
		q = "有个请求需要你确认一下"
	}
	if t := strings.TrimSpace(sessionTitle); t != "" {
		return "来自「" + t + "」的请求：" + q
	}
	return q
}

// alertFrontHook 提醒触发时「把提醒顶到用户眼前」的入口：默认走平台实现
//（alertfront_windows.go 的还原+置前+闪烁/通知兜底），测试里换成桩来断言
// 「到点确实去唤起了窗口」，不必真去动用户的窗口。
var alertFrontHook = alertBringToFront

// handleDesktopFront 把主窗口顶到用户眼前。
//
//	POST /api/desktop/front {title, text, flashOnly}
//
// 提醒触发时后端已经直接调用同一实现（不必经前端）；这个接口是给前端/自测用的
// 兜底入口：flashOnly=true 跳过置前，只走「任务栏闪烁 + 托盘通知」那条兜底路径，
// 用来确认兜底在本机确实看得见。
func (a *App) handleDesktopFront(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title     string `json:"title"`
		Text      string `json:"text"`
		FlashOnly bool   `json:"flashOnly"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	title := strings.TrimSpace(body.Title)
	if title == "" {
		title = "坚祥数字助理 · 有个请求在等你处理"
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		text = "有个请求需要你确认一下"
	}
	if body.FlashOnly {
		alertFallbackNotify(title, text)
	} else {
		alertBringToFront(title, text)
	}
	writeJSON(w, 200, map[string]string{"ok": "1"})
}

// handleAlertScreenHide 前端收起主窗口提醒层时，把桌面全屏蒙层一起关掉
//（两处提醒点任意一处、两边同退的另一半边；实现见 alertfull_windows.go）。
//
//	POST /api/alert/screen {"qid": "..."}   qid=这轮提醒的 questionId/approvalId，
//	与蒙层当前显示的那轮一致才关——上一轮迟到的关闭请求不许把刚弹出的新一轮关掉。
func (a *App) handleAlertScreenHide(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Qid string `json:"qid"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	alertScreenHideIf(strings.TrimSpace(body.Qid))
	writeJSON(w, 200, map[string]string{"ok": "1"})
}
