package main

// 会话级组件配置的 HTTP 接口（合并层的数据面，语义见 sessioncx.go 文件头）：
//
//	GET  /api/components/cfg?s=<会话id>        读该会话的配置视图（总开关 + 每组件覆盖）
//	POST /api/components/cfg                    写/删一条配置覆盖 {session,id,key,value}
//	POST /api/components/cfg/mode               单组件「会话独立 / 跟随全局」{session,id,on}
//	POST /api/components/cfg/scope              总开关（一键切全场）{session,on}
//	    关＝整行完全回归全局：配置覆盖与启停覆盖一起清空（行级下开关只管配置
//	    独立/跟随，启停由行级上开关独立控制）。
//
// 前端用的是「会话视角」而不是「会话值」：GET 返回的 values 只是覆盖部分，
// 没覆盖的键由 web/cx.js 的读取通道向全局值回落——组件照旧读自己的存储，
// 拿到的永远是当前会话视角的值（组件零改动）。

import (
	"encoding/json"
	"net/http"
	"strings"
)

// cfgValueLimit 单条配置值的长度上限（localStorage 里是原始字符串；防跑偏）。
const cfgValueLimit = 64 << 10

// cfgResp 统一的响应体：写完就把最新视图回给前端，省一次往返。
func (a *App) cfgResp(sid string) map[string]any {
	return map[string]any{
		"cfg":           a.cxoComponentCfgView(sid),
		"sessionCustom": a.SessionCX.custom(sid),
	}
}

// cfgSession 解析并校验请求里的会话 id（不存在即 404，前端提示重开对话）。
func (a *App) cfgSession(w http.ResponseWriter, sid string) bool {
	sid = strings.TrimSpace(sid)
	if sid == "" {
		writeErr(w, 400, "需要 session（会话id）")
		return false
	}
	if _, err := a.Store.Get(sid); err != nil {
		writeErr(w, 404, "会话不存在")
		return false
	}
	return true
}

func (a *App) handleComponentCfgGet(w http.ResponseWriter, r *http.Request) {
	sid := strings.TrimSpace(r.URL.Query().Get("s"))
	if !a.cfgSession(w, sid) {
		return
	}
	writeJSON(w, 200, a.cfgResp(sid))
}

func (a *App) handleComponentCfgSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Session string  `json:"session"`
		ID      string  `json:"id"`
		Key     string  `json:"key"`
		Value   *string `json:"value"` // 缺省或 null＝删这条覆盖（回落到全局值）
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体解析失败")
		return
	}
	sid := strings.TrimSpace(body.Session)
	if !a.cfgSession(w, sid) {
		return
	}
	id := strings.TrimSpace(body.ID)
	key := strings.TrimSpace(body.Key)
	if id == "" || key == "" {
		writeErr(w, 400, "需要 id 与 key")
		return
	}
	if _, ok := a.Components.Manifest(id); !ok {
		writeErr(w, 404, "组件不存在")
		return
	}
	if !a.cxoComponentCfgOn(sid, id) {
		writeErr(w, 400, "该组件在本对话是「跟随全局」：先在浮窗里把它切成「会话独立」（或打开底部总开关）")
		return
	}
	if body.Value == nil {
		a.cxoDeleteComponentCfgValue(sid, id, key)
	} else {
		if len(*body.Value) > cfgValueLimit {
			writeErr(w, 400, "单条配置过长（上限 64KB）")
			return
		}
		a.cxoSetComponentCfgValue(sid, id, key, *body.Value)
	}
	writeJSON(w, 200, a.cfgResp(sid))
}

func (a *App) handleComponentCfgMode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Session string `json:"session"`
		ID      string `json:"id"`
		On      bool   `json:"on"` // true＝会话独立，false＝跟随全局（删该组件的覆盖）
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体解析失败")
		return
	}
	sid := strings.TrimSpace(body.Session)
	if !a.cfgSession(w, sid) {
		return
	}
	id := strings.TrimSpace(body.ID)
	if id == "" {
		writeErr(w, 400, "需要 id")
		return
	}
	if _, ok := a.Components.Manifest(id); !ok {
		writeErr(w, 404, "组件不存在")
		return
	}
	a.cxoSetComponentCfgMode(sid, id, body.On)
	writeJSON(w, 200, a.cfgResp(sid))
}

func (a *App) handleComponentCfgScope(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Session string `json:"session"`
		On      bool   `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体解析失败")
		return
	}
	sid := strings.TrimSpace(body.Session)
	if !a.cfgSession(w, sid) {
		return
	}
	a.cxoSetComponentCfgScope(sid, body.On)
	writeJSON(w, 200, a.cfgResp(sid))
}
