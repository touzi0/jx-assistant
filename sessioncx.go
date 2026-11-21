package main

// 会话级组件/插件开关覆盖 + 会话级组件配置合并层：每个对话可以有自己的
// 生效集与组件配置，优先于全局；全局开关/全局配置本身不动（设置页仍归全部
// 对话）。设计动机——语音、Git 面板这类组件不是每个对话都用得上；组件的偏好
// 设置（如朗读音色、听写模型）也常要按对话分开。
//
// **会话级组件配置（定稿 2026-11-08）：统一「会话视角配置合并层」**
//   - 覆盖从「组件开关（布尔）」扩成「任意配置项 KV」：Cfg[组件id][键]＝值；
//   - 读取通道上按「会话覆盖 > 全局值」合并喂给组件（web/cx.js 拦组件自己的
//     存储命名空间 cx:<组件id>:*，组件零改动——它读到的永远是该会话视角的值）；
//   - 不搞 manifest 声明：不注入对话的设置会话化也无感知，机制自动覆盖全部
//     组件的全部前端偏好；
//   - 全局值仍在组件自己的存储里（独立不动），覆盖统一存会话附属数据；
//   - 恢复语义：行级切回「跟随全局」＝删该组件的配置覆盖（Cfg[id] 整段消失）；
//     总开关关掉＝**整行完全回归全局**（配置覆盖 Cfg/CfgFollow 与行级启停覆盖
//     Components 一起清空）；行级上开关只管启停、下开关只管配置独立/跟随。
//
// 存储走会话附属数据（meta 表 sessdata:cx-overrides:<会话id>，随会话生命周期）：
// 只记显式覆盖（会话里没有记录的键跟随全局）；把某键拨回与全局一致的值时，
// 覆盖记录即删除——文件不膨胀，"恢复跟随全局"也不需要额外的清理入口。
// 前端（web/cx.js + web/app-cxsession.js）按当前会话的生效集热装配组件，
// 与设置页的全局热开关共用同一条 CX.toggle 注入/卸载路径。

import (
	"encoding/json"
	"sync"
)

// sessionOverridesKey 是会话附属数据里的存储键（Store.SetSessionData 的 key）。
const sessionOverridesKey = "cx-overrides"

// SessionOverrides 是一个会话的显式覆盖集（未记录的键＝跟随全局）。
type SessionOverrides struct {
	Components map[string]bool `json:"components,omitempty"` // 组件id → 生效值
	Plugins    map[string]bool `json:"plugins,omitempty"`    // "层/插件id" → 生效值
	// 会话级组件配置合并层（见文件头）：
	//   CfgScope  浮窗底部总开关：本对话启用独立的组件设置；关＝一切回落全局
	//   Cfg       组件id → 配置键 → 值（组件存储里的原始字符串，如 "\"zh\""）
	//   CfgFollow 组件id → true＝该组件在本会话显式跟随全局
	//             （缺省＝总开关开着时即会话独立：总开关就是「一键切全场」）
	CfgScope  bool                         `json:"cfgScope,omitempty"`
	Cfg       map[string]map[string]string `json:"cfg,omitempty"`
	CfgFollow map[string]bool              `json:"cfgFollow,omitempty"`
}

// sessionCXRegistry 是全部会话覆盖的内存快照（写时同步落库）。
// 启动时从 Store 载入；会话删除经 forget 摘除。并发安全。
type sessionCXRegistry struct {
	mu  sync.RWMutex
	per map[string]*SessionOverrides
}

// loadSessionCX 启动时载入全部会话的覆盖（遍历会话表读附属数据）。
func loadSessionCX(store *Store) *sessionCXRegistry {
	r := &sessionCXRegistry{per: map[string]*SessionOverrides{}}
	if store == nil {
		return r
	}
	sessions, err := store.List()
	if err != nil {
		return r
	}
	for _, s := range sessions {
		if o := parseSessionOverrides(store.GetSessionData(s.ID, sessionOverridesKey)); o != nil {
			r.per[s.ID] = o
		}
	}
	return r
}

func parseSessionOverrides(raw string) *SessionOverrides {
	if raw == "" {
		return nil
	}
	var o SessionOverrides
	if err := json.Unmarshal([]byte(raw), &o); err != nil {
		return nil
	}
	if len(o.Components) == 0 && len(o.Plugins) == 0 && len(o.Cfg) == 0 && len(o.CfgFollow) == 0 && !o.CfgScope {
		return nil
	}
	return &o
}

// of 返回某会话的覆盖集（没有＝nil；返回值只读，改动走 set）。
func (r *sessionCXRegistry) of(sid string) *SessionOverrides {
	if r == nil || sid == "" {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.per[sid]
}

// set 在锁内改一份副本并落库；改完两个映射都空了就整体摘除该会话。
func (r *sessionCXRegistry) set(store *Store, sid string, mutate func(o *SessionOverrides)) {
	if r == nil || sid == "" {
		return
	}
	r.mu.Lock()
	cur := r.per[sid]
	if cur == nil {
		cur = &SessionOverrides{}
	} else { // 副本：读侧拿到的旧引用不受影响（Cfg 两层都要拷）
		cc, cp := map[string]bool{}, map[string]bool{}
		for k, v := range cur.Components {
			cc[k] = v
		}
		for k, v := range cur.Plugins {
			cp[k] = v
		}
		cf := map[string]map[string]string{}
		for id, kv := range cur.Cfg {
			inner := map[string]string{}
			for k, v := range kv {
				inner[k] = v
			}
			cf[id] = inner
		}
		cfl := map[string]bool{}
		for k, v := range cur.CfgFollow {
			cfl[k] = v
		}
		cur = &SessionOverrides{Components: cc, Plugins: cp, CfgScope: cur.CfgScope, Cfg: cf, CfgFollow: cfl}
	}
	mutate(cur)
	empty := len(cur.Components) == 0 && len(cur.Plugins) == 0 && len(cur.Cfg) == 0 && len(cur.CfgFollow) == 0 && !cur.CfgScope
	if empty {
		delete(r.per, sid)
	} else {
		r.per[sid] = cur
	}
	r.mu.Unlock()
	if store != nil {
		if empty {
			_ = store.SetSessionData(sid, sessionOverridesKey, "")
		} else if b, err := json.Marshal(cur); err == nil {
			_ = store.SetSessionData(sid, sessionOverridesKey, string(b))
		}
	}
}

// custom 报告某会话是否有任何覆盖记录（前端加号菜单的「已定制」标记）。
func (r *sessionCXRegistry) custom(sid string) bool {
	o := r.of(sid)
	return o != nil && (len(o.Components) > 0 || len(o.Plugins) > 0 ||
		len(o.Cfg) > 0 || len(o.CfgFollow) > 0 || o.CfgScope)
}

// forget 会话删除时摘掉它的覆盖记录（库里的附属数据行由 Store 一并清）。
func (r *sessionCXRegistry) forget(sid string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.per, sid)
	r.mu.Unlock()
}

// anyComponentEnabled 报告是否「还有任何会话」显式启用某组件
// （worker 进程与语音端点的存活判定用它：全局关了但还有会话要它，就留）。
func (r *sessionCXRegistry) anyComponentEnabled(id string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, o := range r.per {
		if o.Components[id] {
			return true
		}
	}
	return false
}

// ---------- App 侧助手（nil 安全：注册表没初始化时一切回落全局） ----------

// cxoComponentEnabled 某会话里某组件是否生效：显式覆盖优先，否则全局开关。
func (a *App) cxoComponentEnabled(sid, id string) bool {
	if a.Components == nil {
		return false
	}
	if o := a.SessionCX.of(sid); o != nil {
		if v, ok := o.Components[id]; ok {
			return v
		}
	}
	return a.Components.IsEnabled(id)
}

// cxoPluginEnabled 某会话里某插件是否生效（键＝层id/插件id）。
func (a *App) cxoPluginEnabled(sid, layer, id string) bool {
	if o := a.SessionCX.of(sid); o != nil {
		if v, ok := o.Plugins[pluginOverrideKey(layer, id)]; ok {
			return v
		}
	}
	return a.pluginGlobalEnabled(layer, id)
}

// pluginOverrideKey 插件覆盖键：层前缀防跨层重名。
func pluginOverrideKey(layer, id string) string { return layer + "/" + id }

// pluginGlobalEnabled 插件的全局开关值（不认会话；找不到按未启用）。
func (a *App) pluginGlobalEnabled(layer, id string) bool {
	_, ok := a.pluginGlobalLookup(layer, id)
	return ok
}

// pluginGlobalKnown 插件是否存在于某层（会话覆盖写入前的存在性校验）。
func (a *App) pluginGlobalKnown(layer, id string) bool {
	_, ok := a.pluginGlobalLookup(layer, id)
	return ok
}

// pluginGlobalLookup 按 Layers.SetEnabled 同款规则定位插件：layer 为空时在
// 全部层里按 id 匹配（跨层重名取首个——写入侧的一致性靠层 id 前缀键保证）。
func (a *App) pluginGlobalLookup(layer, id string) (LayerPlugin, bool) {
	if a.Layers == nil {
		return LayerPlugin{}, false
	}
	for _, l := range a.Layers.Layers() {
		if layer != "" && l.Info().ID != layer {
			continue
		}
		for _, p := range l.Plugins() {
			if p.ID == id {
				return p, true
			}
		}
	}
	return LayerPlugin{}, false
}

// cxoSetComponent 写一条组件覆盖（与全局一致时删除记录——跟随全局是缺省态）。
func (a *App) cxoSetComponent(sid, id string, on bool) {
	g := a.Components != nil && a.Components.IsEnabled(id)
	a.SessionCX.set(a.Store, sid, func(o *SessionOverrides) {
		if on == g {
			delete(o.Components, id)
		} else {
			if o.Components == nil {
				o.Components = map[string]bool{}
			}
			o.Components[id] = on
		}
	})
}

// cxoSetPlugin 写一条插件覆盖（同款归一）。
func (a *App) cxoSetPlugin(sid, layer, id string, on bool) {
	g := a.pluginGlobalEnabled(layer, id)
	a.SessionCX.set(a.Store, sid, func(o *SessionOverrides) {
		if on == g {
			delete(o.Plugins, pluginOverrideKey(layer, id))
		} else {
			if o.Plugins == nil {
				o.Plugins = map[string]bool{}
			}
			o.Plugins[pluginOverrideKey(layer, id)] = on
		}
	})
}

// cxoComponentLive 组件「活着」的判定：全局开着，或还有任何会话显式要它。
// 组件桥端点与语音端点用它——这些请求不带会话（组件脚本只知道自己的端点），
// 按最宽松口径放行：会话关掉的对话里组件 UI 已被热卸载，根本不会发请求来。
func (a *App) cxoComponentLive(id string) bool {
	if a.Components != nil && a.Components.IsEnabled(id) {
		return true
	}
	return a.SessionCX.anyComponentEnabled(id)
}

// cxoComponentList 全局清单叠加某会话覆盖后的生效视图（设置浮窗/前端装配用）。
func (a *App) cxoComponentList(sid string) []ComponentStatus {
	list := a.Components.List()
	o := a.SessionCX.of(sid)
	for i := range list {
		g := list[i].Enabled
		list[i].GlobalEnabled = g
		if o != nil {
			if v, ok := o.Components[list[i].ID]; ok {
				list[i].Enabled = v
				list[i].Override = true
			}
		}
	}
	return list
}

// ---------- 会话级组件配置合并层（会话视角 > 全局值） ----------

// cxoComponentCfgOn 该会话里某组件的配置是否「会话独立」：总开关开着、且没有
// 显式标记跟随全局（缺省＝独立——总开关本身就是「一键切全场」）。
func (a *App) cxoComponentCfgOn(sid, id string) bool {
	o := a.SessionCX.of(sid)
	return o != nil && o.CfgScope && !o.CfgFollow[id]
}

// cxoComponentCfgView 会话视角的组件配置视图（前端装配/浮窗渲染/接口返回用）：
//
//	{ "scope": 总开关, "components": { <组件id>: {"on": 是否会话独立, "values": {键:值}} } }
//
// values 是该组的会话覆盖；没被覆盖的键由前端向全局值回落（合并层在前端读取
// 通道上完成，组件零改动）。
func (a *App) cxoComponentCfgView(sid string) map[string]any {
	comps := map[string]any{}
	if a.Components == nil {
		return map[string]any{"scope": false, "components": comps}
	}
	o := a.SessionCX.of(sid)
	scope := o != nil && o.CfgScope
	for _, c := range a.Components.List() {
		vals := map[string]string{}
		if scope {
			for k, v := range o.Cfg[c.ID] {
				vals[k] = v
			}
		}
		on := scope && !o.CfgFollow[c.ID]
		comps[c.ID] = map[string]any{"on": on, "values": vals}
	}
	return map[string]any{"scope": scope, "components": comps}
}

// cxoSetComponentCfgScope 总开关：开＝本对话启用独立组件设置（缺省每个组件都
// 独立，初始覆盖为空＝值全部来自全局）；关＝**整行完全回归全局**——配置覆盖与
// 启停覆盖一起清空（每行的配置跟随全局、启停开关也跟随全局，见浮窗交互口径）。
func (a *App) cxoSetComponentCfgScope(sid string, on bool) {
	a.SessionCX.set(a.Store, sid, func(o *SessionOverrides) {
		if !on {
			o.Cfg = nil
			o.CfgFollow = nil
			o.Components = nil // 行级启停覆盖一并撤销：整行回到全局状态
		}
		o.CfgScope = on
	})
}

// cxoSetComponentCfgMode 单个组件「会话独立 / 跟随全局」：跟随全局＝删掉它的
// 覆盖记录（恢复语义，文件不膨胀）。
func (a *App) cxoSetComponentCfgMode(sid, id string, on bool) {
	a.SessionCX.set(a.Store, sid, func(o *SessionOverrides) {
		if on {
			delete(o.CfgFollow, id)
			return
		}
		delete(o.Cfg, id) // 删该组件的会话覆盖：跟随全局＝没有自己的记录
		if o.CfgFollow == nil {
			o.CfgFollow = map[string]bool{}
		}
		o.CfgFollow[id] = true
	})
}

// cxoSetComponentCfgValue 写/删一条组件配置覆盖（键不存在＝删）。值＝组件存进
// localStorage 的原始字符串；与全局一致时由前端负责改发删除（服务端不读组件
// 的全局存储）。
func (a *App) cxoSetComponentCfgValue(sid, id, key, value string) {
	if key == "" {
		return
	}
	a.SessionCX.set(a.Store, sid, func(o *SessionOverrides) {
		if o.Cfg == nil {
			o.Cfg = map[string]map[string]string{}
		}
		inner := o.Cfg[id]
		if inner == nil {
			inner = map[string]string{}
		}
		inner[key] = value
		o.Cfg[id] = inner
	})
}

// cxoDeleteComponentCfgValue 删一条覆盖（前端在「值与全局一致」或组件
// removeItem 时调）：删空后整段（含组件节点）一并摘除。
func (a *App) cxoDeleteComponentCfgValue(sid, id, key string) {
	a.SessionCX.set(a.Store, sid, func(o *SessionOverrides) {
		inner := o.Cfg[id]
		if inner == nil {
			return
		}
		delete(inner, key)
		if len(inner) == 0 {
			delete(o.Cfg, id)
		}
	})
}

// cxoPluginList 插件清单的会话生效视图（同 cxoComponentList）。
func (a *App) cxoPluginList(sid string) []LayerPlugin {
	list := a.Layers.AllPlugins()
	o := a.SessionCX.of(sid)
	for i := range list {
		g := list[i].Enabled
		list[i].GlobalEnabled = g
		if o != nil {
			if v, ok := o.Plugins[pluginOverrideKey(list[i].Layer, list[i].ID)]; ok {
				list[i].Enabled = v
				list[i].Override = true
			}
		}
	}
	return list
}
