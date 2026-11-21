package main

// Claude Code 插件组件桥：mods（hooks.json 的 {"modules": [...]} 形态）。
// mod 是运行在宿主进程内的 JS 事件处理器：ES module 导出 register(on)，
// on('<事件>', <matcher>?, async ($, e, next) => …) 注册钩子——观察
// （return next(e)）、改写（next({...e, ...})）、短路（返回 {deny}/{result}/
// {text} 等）。本引擎用纯 Go 的 goja 内嵌运行时承载（四平台一致，含安卓）：
//   - ESM→CJS 薄转换：mod 的 import/export 形态受 Claude 规范约束（只能相对
//     路径与 'claude-code'），行级改写即可覆盖；.ts 文件不支持（明确报错）
//   - next 链：同插件内按注册序递归串联；返回 undefined 视为放行（e 取最后一次
//     next 的参数），返回对象视为短路答案
//   - 事件桥：tool.call / prompt.submit / command.run / session.start /
//     turn.complete（与 settings hooks 的分发点共用，mod 在后）
//   - $ API：ccplugin_modapi.go；UI 渲染类事件（ui.render 等）不触发，注册了
//     也收不到（文档列明的差异）
// 运行限制（对齐 Claude）：钩子体 10s 超时；fs 4MiB；敏感操作过会话权限门控。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
)

// modHook 是一个已注册的 mod 钩子。
type modHook struct {
	event   string
	matcher map[string]any // 字段过滤（值为字符串/数组/正则串）
	fn      goja.Callable
}

// modHost 是一个插件 mods 的运行时（一个插件一个 goja VM）。
type modHost struct {
	plugin  *CCPlugin
	reg     *CCPluginRegistry
	vm      *goja.Runtime
	mu      sync.Mutex // goja Runtime 非并发安全：事件分发与 timer 回调都持锁
	storeMu sync.Mutex // $.store 单独一把锁：handler 在持 mu 时也能读写 store
	hooks   map[string][]modHook
	commands map[string]modCommandDef // $.command.register 注册的命令
	tools   map[string]modToolDef     // $.tool.register 注册的工具
	store   map[string]any            // $.store（持久化由外层落盘）
	sessCtx *modSessCtx               // 最近一次事件的会话上下文（$ API 权限判断用）

	moduleCache map[string]goja.Value // 相对路径模块缓存（同文件只执行一次）
}

// requireValue 构造给模块包装用的 require 函数值。
func (h *modHost) requireValue() goja.Value {
	return h.vm.ToValue(func(call goja.FunctionCall) goja.Value {
		return h.modRequire(call.Argument(0).String())
	})
}

// modCommandDef / modToolDef 是 mod 动态注册的命令与工具。
type modCommandDef struct {
	Name         string
	Description  string
	ArgumentHint string
}

type modToolDef struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// modSessCtx 是事件携带的会话上下文（$ API 的权限与路径解析依据）。
type modSessCtx struct {
	Sess *SessionMeta
	Ctx  context.Context
}

// modOutcome 是一次事件分发给引擎的结果。
type modOutcome struct {
	Rewrite map[string]any // 最后一次 next 的事件参数（改写语义）
	Answer  map[string]any // 短路答案（deny/result/text 等；nil = 放行）
}

// ---------- ESM → CJS 薄转换 ----------

var (
	reImportNamed = regexp.MustCompile(`^import\s*\{([^}]*)\}\s*from\s*['"]([^'"]+)['"]\s*$`)
	reImportDef   = regexp.MustCompile(`^import\s+(\w+)\s+from\s+['"]([^'"]+)['"]\s*$`)
	reImportStar  = regexp.MustCompile(`^import\s*\*\s*as\s+(\w+)\s+from\s+['"]([^'"]+)['"]\s*$`)
	reImportBare  = regexp.MustCompile(`^import\s+['"]([^'"]+)['"]\s*$`)

	reExportFunc   = regexp.MustCompile(`^export\s+(async\s+)?function\s+(\w+)`)
	reExportVar    = regexp.MustCompile(`^export\s+(const|let|var)\s+(\w+)`)
	reExportBraces = regexp.MustCompile(`^export\s*\{([^}]*)\}`)
)

// esmToCJS 把 mod 的受限 ESM 源码行级改写为 CJS：import → require 解构，
// export 声明转普通声明、名字在模块末尾统一登记（let/var 的最终值才被导出）。
// 只覆盖 Claude 规范允许的形态（default import 直接取模块本身——mod 生态的
// 相对依赖是 CJS 形态）。
func esmToCJS(src string) string {
	lines := strings.Split(src, "\n")
	out := make([]string, 0, len(lines)+4)
	var exports []string
	for _, line := range lines {
		t := strings.TrimSpace(line)
		switch {
		case t == "" || strings.HasPrefix(t, "//"):
			out = append(out, line)
			continue
		case reImportNamed.MatchString(t):
			m := reImportNamed.FindStringSubmatch(t)
			out = append(out, "const {"+m[1]+"} = require('"+m[2]+"');")
			continue
		case reImportStar.MatchString(t):
			m := reImportStar.FindStringSubmatch(t)
			out = append(out, "const "+m[1]+" = require('"+m[2]+"');")
			continue
		case reImportDef.MatchString(t):
			m := reImportDef.FindStringSubmatch(t)
			out = append(out, "const "+m[1]+" = require('"+m[2]+"');")
			continue
		case reImportBare.MatchString(t):
			m := reImportBare.FindStringSubmatch(t)
			out = append(out, "require('"+m[1]+"');")
			continue
		}
		if m := reExportFunc.FindStringSubmatch(t); m != nil {
			out = append(out, strings.TrimSpace(strings.TrimPrefix(t, "export ")))
			exports = append(exports, m[2])
			continue
		}
		if m := reExportVar.FindStringSubmatch(t); m != nil {
			out = append(out, m[1]+" "+m[2]+" "+strings.TrimSpace(strings.TrimPrefix(t, "export "+m[1]+" "+m[2])))
			exports = append(exports, m[2])
			continue
		}
		if m := reExportBraces.FindStringSubmatch(t); m != nil {
			for _, p := range strings.Split(m[1], ",") {
				p = strings.TrimSpace(p)
				if p == "" {
					continue
				}
				name := p
				if i := strings.Index(p, " as "); i > 0 {
					name = strings.TrimSpace(p[i+4:])
				}
				exports = append(exports, name)
			}
			continue
		}
		if strings.HasPrefix(t, "export default ") {
			out = append(out, "__jxDefault = "+strings.TrimSpace(strings.TrimPrefix(t, "export default ")))
			exports = append(exports, "default")
			continue
		}
		out = append(out, line)
	}
	if len(exports) > 0 {
		for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
			out = out[:len(out)-1]
		}
		out = append(out, "")
		for _, name := range exports {
			local := name
			if name == "default" {
				local = "__jxDefault"
			}
			out = append(out, "module.exports."+name+" = "+local+";")
		}
	}
	return strings.Join(out, "\n")
}

// ---------- mod 宿主 ----------

// newModHost 为一个插件创建 mods 运行时并加载全部模块文件。
func newModHost(p *CCPlugin, reg *CCPluginRegistry) (*modHost, error) {
	h := &modHost{
		plugin: p, reg: reg,
		vm:       goja.New(),
		hooks:    map[string][]modHook{},
		commands: map[string]modCommandDef{},
		tools:    map[string]modToolDef{},
		store:    map[string]any{},
	}
	h.loadStore()
	h.installHostGlobals()
	for _, rel := range p.ModModules {
		path := filepath.Join(p.Root, filepath.FromSlash(strings.TrimPrefix(rel, "./")))
		if err := h.loadModuleFile(path); err != nil {
			return nil, err
		}
	}
	return h, nil
}

// loadModuleFile 读取一个 mod 模块文件（.js/.mjs/.cjs；.ts 明确不支持），
// ESM→CJS 转换后在模块包装里执行，取 module.exports.register 调用之。
func (h *modHost) loadModuleFile(path string) error {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".js", ".mjs", ".cjs":
	case ".ts", ".mts", ".cts", ".tsx", ".jsx":
		return fmt.Errorf("%s：TypeScript mod 暂不支持，请改用 .js", filepath.Base(path))
	default:
		return fmt.Errorf("%s：不认识的 mod 模块扩展名", filepath.Base(path))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.vm.Set("__filename", path)
	h.vm.Set("__dirname", filepath.Dir(path))
	wrapped := "(function(module, exports, require){\n" + esmToCJS(string(data)) + "\n})"
	prog, err := h.vm.RunString(wrapped)
	if err != nil {
		return fmt.Errorf("模块解析失败: %w", err)
	}
	moduleObj := h.vm.NewObject()
	exportsObj := h.vm.NewObject()
	_ = moduleObj.Set("exports", exportsObj)
	fn, ok := goja.AssertFunction(prog)
	if !ok {
		return fmt.Errorf("模块包装不是函数")
	}
	if _, err := fn(goja.Undefined(), moduleObj, exportsObj, h.requireValue()); err != nil {
		return fmt.Errorf("模块执行失败: %w", err)
	}
	register, ok := goja.AssertFunction(exportsObj.Get("register"))
	if !ok {
		return fmt.Errorf("模块未导出 register 函数")
	}
	// register(on)：on 是宿主函数（收集钩子）
	onFn := h.vm.ToValue(func(call goja.FunctionCall) goja.Value {
		return h.onRegister(call)
	})
	if _, err := register(goja.Undefined(), onFn); err != nil {
		return fmt.Errorf("register 调用失败: %w", err)
	}
	return nil
}

// onRegister 处理 on(event, matcher?, handler) 注册。
func (h *modHost) onRegister(call goja.FunctionCall) goja.Value {
	args := call.Arguments
	if len(args) < 2 {
		panic(h.vm.NewTypeError("on(事件名, [matcher], handler) 需要至少两个参数"))
	}
	event := args[0].String()
	var matcher map[string]any
	handler := args[len(args)-1]
	if len(args) >= 3 {
		if m, ok := args[1].Export().(map[string]any); ok {
			matcher = m
		}
	}
	fn, ok := goja.AssertFunction(handler)
	if !ok {
		panic(h.vm.NewTypeError("on 的 handler 必须是函数"))
	}
	h.hooks[event] = append(h.hooks[event], modHook{event: event, matcher: matcher, fn: fn})
	return goja.Undefined()
}

// modRequire 实现 require：'claude-code' 内置模块 + 相对路径递归加载。
// 模块缓存按路径存（同一文件只执行一次）。
func (h *modHost) modRequire(path string) goja.Value {
	switch path {
	case "claude-code":
		return h.claudeCodeModule()
	}
	if !strings.HasPrefix(path, "./") && !strings.HasPrefix(path, "../") {
		panic(h.vm.NewTypeError("mod 只能 import 相对路径或 'claude-code'，不支持: " + path))
	}
	// 相对当前模块文件解析（__dirname 由 loadModuleFile 设置）
	dir := h.plugin.Root
	if dn := h.vm.Get("__dirname"); dn != nil && !goja.IsUndefined(dn) {
		dir = dn.String()
	}
	full := filepath.Join(dir, filepath.FromSlash(path))
	if !strings.HasSuffix(full, ".js") && !strings.HasSuffix(full, ".mjs") && !strings.HasSuffix(full, ".cjs") {
		full += ".js"
	}
	if cache, ok := h.moduleCache[full]; ok {
		return cache
	}
	data, err := os.ReadFile(full)
	if err != nil {
		panic(h.vm.NewTypeError("加载模块失败: " + path))
	}
	wrapped := "(function(module, exports, require){\n" + esmToCJS(string(data)) + "\n})"
	prog, err := h.vm.RunString(wrapped)
	if err != nil {
		panic(h.vm.NewTypeError("模块解析失败: " + path))
	}
	moduleObj := h.vm.NewObject()
	exportsObj := h.vm.NewObject()
	_ = moduleObj.Set("exports", exportsObj)
	if fn, ok := goja.AssertFunction(prog); ok {
		if _, err := fn(goja.Undefined(), moduleObj, exportsObj, h.requireValue()); err != nil {
			panic(h.vm.NewTypeError("模块执行失败: " + path))
		}
	}
	if h.moduleCache == nil {
		h.moduleCache = map[string]goja.Value{}
	}
	h.moduleCache[full] = exportsObj
	return exportsObj
}

// claudeCodeModule 提供 'claude-code' 裸导入（$.state 的 helper 家族；
// 第一版实现 read/update，atom/derive 给占位实现）。
func (h *modHost) claudeCodeModule() goja.Value {
	obj := h.vm.NewObject()
	readFn := h.vm.ToValue(func(call goja.FunctionCall) goja.Value {
		// read(store, key)：读插件 store 的原子值（简化：直接读键）
		key := call.Argument(1).String()
		h.storeMu.Lock()
		v := h.store[key]
		h.storeMu.Unlock()
		return h.vm.ToValue(v)
	})
	updateFn := h.vm.ToValue(func(call goja.FunctionCall) goja.Value {
		key := call.Argument(1).String()
		fn, ok := goja.AssertFunction(call.Argument(2))
		if !ok {
			return goja.Undefined()
		}
		h.storeMu.Lock()
		cur := h.store[key]
		h.storeMu.Unlock()
		res, err := fn(goja.Undefined(), h.vm.ToValue(cur))
		if err == nil {
			h.storeMu.Lock()
			h.store[key] = res.Export()
			h.saveStoreLocked()
			h.storeMu.Unlock()
		}
		return res
	})
	_ = obj.Set("read", readFn)
	_ = obj.Set("update", updateFn)
	_ = obj.Set("atom", func(call goja.FunctionCall) goja.Value { return call.Argument(0) })
	_ = obj.Set("derive", func(call goja.FunctionCall) goja.Value { return call.Argument(1) })
	_ = obj.Set("memberOf", func(call goja.FunctionCall) goja.Value { return goja.Undefined() })
	return obj
}

// ---------- 事件分发 ----------

// modHandlerTimeout 是钩子体（不含 next 后的引擎行为）的超时。
const modHandlerTimeout = 10 * time.Second

// dispatchEvent 分发一个事件到本插件的所有匹配钩子（调用方持 h.mu）。
// 返回短路答案（nil = 放行）与事件改写（最后一次 next 的参数）。
func (h *modHost) dispatchEvent(event string, e map[string]any, sess *modSessCtx) (modOutcome, error) {
	hooks := h.hooks[event]
	if len(hooks) == 0 {
		return modOutcome{}, nil
	}
	matched := make([]modHook, 0, len(hooks))
	for _, hk := range hooks {
		if modMatchEvent(hk.matcher, e) {
			matched = append(matched, hk)
		}
	}
	if len(matched) == 0 {
		return modOutcome{}, nil
	}
	h.sessCtx = sess
	outcome := modOutcome{Rewrite: e}
	var run func(idx int, cur map[string]any) (goja.Value, error)
	run = func(idx int, cur map[string]any) (goja.Value, error) {
		if idx >= len(matched) {
			return goja.Undefined(), nil // 链尾：引擎哨兵
		}
		nextFn := h.vm.ToValue(func(call goja.FunctionCall) goja.Value {
			var next map[string]any
			if m, ok := call.Argument(0).Export().(map[string]any); ok {
				next = m
			}
			outcome.Rewrite = next
			v, err := run(idx+1, next)
			if err != nil {
				panic(h.vm.NewGoError(err))
			}
			return v
		})
		res, err := matched[idx].fn(goja.Undefined(), h.buildDollar(sess), h.vm.ToValue(cur), nextFn)
		if err != nil {
			return nil, err
		}
		return settleModValue(h.vm, res, modHandlerTimeout), nil
	}
	final, err := run(0, e)
	if err != nil {
		return outcome, err
	}
	if final != nil && !goja.IsUndefined(final) {
		if m, ok := final.Export().(map[string]any); ok {
			outcome.Answer = m
		}
	}
	return outcome, nil
}

// settleModValue 等待一个可能是 Promise 的值 settle（驱动 microtask 队列）。
func settleModValue(vm *goja.Runtime, v goja.Value, timeout time.Duration) goja.Value {
	if v == nil {
		return nil
	}
	if p, ok := v.Export().(*goja.Promise); ok {
		deadline := time.Now().Add(timeout)
		for p.State() == goja.PromiseStatePending && time.Now().Before(deadline) {
			_, _ = vm.RunString("0") // 触发 microtask flush
			time.Sleep(2 * time.Millisecond)
		}
		if p.State() != goja.PromiseStatePending {
			return p.Result()
		}
		return goja.Undefined() // 超时：按无意见放行（引擎行为继续）
	}
	return v
}

// modMatchEvent 按 matcher 过滤事件字段：值为字符串（相等或正则）、数组
// （任一相等）、regexp（正则匹配）。空 matcher 匹配全部。
func modMatchEvent(matcher map[string]any, e map[string]any) bool {
	if len(matcher) == 0 {
		return true
	}
	for k, want := range matcher {
		got, ok := e[k]
		if !ok {
			return false
		}
		switch w := want.(type) {
		case string:
			if fmt.Sprintf("%v", got) != w {
				return false
			}
		case []any:
			hit := false
			for _, item := range w {
				if fmt.Sprintf("%v", got) == fmt.Sprintf("%v", item) {
					hit = true
					break
				}
			}
			if !hit {
				return false
			}
		case *regexp.Regexp:
			if s, ok := got.(string); !ok || !w.MatchString(s) {
				return false
			}
		}
	}
	return true
}

// ---------- 注册表侧的 mods 管理 ----------

// ModsStartAll 加载全部启用插件的 mods（mods 组件放行才算），并触发
// session.start 事件（对齐 Claude「装载时每 mod 一次」语义）。
func (r *CCPluginRegistry) ModsStartAll() {
	if r == nil {
		return
	}
	r.modMu.Lock()
	r.modHosts = map[string]*modHost{}
	hosts := []*modHost{}
	for _, p := range r.enabled() {
		if !p.componentEnabled("mods") || len(p.ModModules) == 0 {
			continue
		}
		h, err := newModHost(p, r)
		if err != nil {
			log.Printf("[CC插件] mods %s 加载失败: %v", p.Key, err)
			continue
		}
		r.modHosts[p.Key] = h
		hosts = append(hosts, h)
	}
	r.modMu.Unlock()
	for _, h := range hosts {
		h.mu.Lock()
		_, _ = h.dispatchEvent("session.start", map[string]any{}, &modSessCtx{Ctx: context.Background()})
		h.mu.Unlock()
		log.Printf("[CC插件] mods %s 已装载（事件 %d 类）", h.plugin.Key, len(h.hooks))
	}
}

// ccEmitModEvent 把一个 mod 事件分发给全部已装载的宿主（含事件字段）。
// 返回合并结果：第一个短路答案胜出；改写取最后一个插件的。
func (a *App) ccEmitModEvent(ctx context.Context, sess *SessionMeta, event string, e map[string]any) modOutcome {
	var zero modOutcome
	if a.CCPlugins == nil {
		return zero
	}
	a.CCPlugins.modMu.RLock()
	hosts := make([]*modHost, 0, len(a.CCPlugins.modHosts))
	for _, h := range a.CCPlugins.modHosts {
		hosts = append(hosts, h)
	}
	a.CCPlugins.modMu.RUnlock()
	if len(hosts) == 0 {
		return zero
	}
	out := modOutcome{Rewrite: e}
	for _, h := range hosts {
		h.mu.Lock()
		oc, err := h.dispatchEvent(event, out.Rewrite, &modSessCtx{Sess: sess, Ctx: ctx})
		h.mu.Unlock()
		if err != nil {
			log.Printf("[CC插件] mods %s 事件 %s 失败: %v", h.plugin.Key, event, err)
			continue
		}
		if oc.Rewrite != nil {
			out.Rewrite = oc.Rewrite
		}
		if oc.Answer != nil && out.Answer == nil {
			out.Answer = oc.Answer
		}
	}
	return out
}

// ccModToolArgs 把工具调用参数展开为事件对象（对齐 Claude：参数平铺在 e 上，
// e.tool 是工具名）。
func ccModToolArgs(tool string, args json.RawMessage) map[string]any {
	e := map[string]any{"tool": tool}
	if len(args) > 0 {
		var m map[string]any
		if err := json.Unmarshal(args, &m); err == nil {
			for k, v := range m {
				e[k] = v
			}
		}
	}
	return e
}

// ccModCommandList 汇总 mod 注册的命令（斜杠命令弹层用）。
func (r *CCPluginRegistry) ccModCommandList() []map[string]any {
	return r.ccModCommandListFiltered(nil)
}

// ccModCommandListFiltered 同上，带插件过滤器（会话级插件覆盖：会话把某插件
// 关了，它的 mod 命令不出场；filter 为 nil＝不过滤）。
func (r *CCPluginRegistry) ccModCommandListFiltered(filter func(pluginKey string) bool) []map[string]any {
	out := []map[string]any{}
	if r == nil {
		return out
	}
	r.modMu.RLock()
	defer r.modMu.RUnlock()
	keys := make([]string, 0, len(r.modHosts))
	for k := range r.modHosts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h := r.modHosts[k]
		if filter != nil && !filter(h.plugin.Key) {
			continue
		}
		h.mu.Lock()
		names := make([]string, 0, len(h.commands))
		for n := range h.commands {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			c := h.commands[n]
			out = append(out, map[string]any{
				"name": c.Name, "description": c.Description, "argumentHint": c.ArgumentHint,
				"plugin": h.plugin.Name,
			})
		}
		h.mu.Unlock()
	}
	return out
}

// ccModRunCommand 执行一个 mod 注册的命令（command.run 事件）。命令名支持
// 全名（插件:命令）与全局唯一的裸名（对齐 Claude 的短名规则）。
func (a *App) ccModRunCommand(ctx context.Context, sess *SessionMeta, name, args string) (string, bool) {
	if a.CCPlugins == nil {
		return "", false
	}
	a.CCPlugins.modMu.RLock()
	var host *modHost
	var cmdName string
	shortHits, shortHost, shortName := 0, (*modHost)(nil), ""
	for _, h := range a.CCPlugins.modHosts {
		h.mu.Lock()
		if c, ok := h.commands[name]; ok {
			host, cmdName = h, c.Name
		} else {
			for k := range h.commands {
				if i := strings.Index(k, ":"); i >= 0 && k[i+1:] == name {
					shortHits++
					shortHost, shortName = h, k
				}
			}
		}
		h.mu.Unlock()
		if host != nil {
			break
		}
	}
	if host == nil && shortHits == 1 {
		host, cmdName = shortHost, shortName
	}
	a.CCPlugins.modMu.RUnlock()
	if host == nil {
		return "", false
	}
	// 会话级插件覆盖：本对话把这个插件关了，它的 mod 命令不执行
	if sess != nil && !a.cxoPluginEnabled(sess.ID, "claude", host.plugin.Key) {
		return "", false
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	oc, err := host.dispatchEvent("command.run", map[string]any{"command": strings.TrimPrefix(cmdName, host.plugin.Name+":"), "args": args}, &modSessCtx{Sess: sess, Ctx: ctx})
	if err != nil {
		return "mod 命令执行失败: " + err.Error(), true
	}
	if text, ok := oc.Answer["text"].(string); ok {
		return text, true
	}
	return "", true
}

// ---------- 引擎分发点桥接 ----------

// hasMods 报告是否有已装载的 mods 宿主（分发点快速短路用）。
func (r *CCPluginRegistry) hasMods() bool {
	if r == nil {
		return false
	}
	r.modMu.RLock()
	defer r.modMu.RUnlock()
	return len(r.modHosts) > 0
}

// ccModPreToolUse 工具执行前的 mod 事件桥（在 settings hooks 之后）：
// 返回空串放行；{deny} 拒绝、{result} 自答；改写的参数写回 tc。
func (a *App) ccModPreToolUse(ctx context.Context, sess *SessionMeta, tc *ToolCall) string {
	if a.CCPlugins == nil || !a.CCPlugins.hasMods() {
		return ""
	}
	e := ccModToolArgs(tc.Function.Name, json.RawMessage(tc.Function.Arguments))
	oc := a.ccEmitModEvent(ctx, sess, "tool.call", e)
	if oc.Answer != nil {
		if deny, ok := oc.Answer["deny"].(string); ok && strings.TrimSpace(deny) != "" {
			return "工具调用被 mod 拒绝：" + deny
		}
		if res, ok := oc.Answer["result"].(string); ok {
			return res // 自答（Claude 语义：结果直接作为工具结果）
		}
	}
	if oc.Rewrite != nil {
		// 改写后的参数：去掉事件元字段（tool）重新序列化
		rewritten := map[string]any{}
		for k, v := range oc.Rewrite {
			if k != "tool" {
				rewritten[k] = v
			}
		}
		if len(rewritten) > 0 {
			if data, err := json.Marshal(rewritten); err == nil {
				tc.Function.Arguments = string(data)
			}
		}
	}
	return ""
}

// ccModTurnComplete 回合收尾事件（answer.text 转为报告行；观察型钩子为主）。
func (a *App) ccModTurnComplete(ctx context.Context, sess *SessionMeta, emit func(Event)) {
	if a.CCPlugins == nil || !a.CCPlugins.hasMods() {
		return
	}
	e := map[string]any{"turnId": sess.ID}
	if sess.ParentID != "" {
		e["agentId"] = sess.ID
	}
	oc := a.ccEmitModEvent(ctx, sess, "turn.complete", e)
	if text, ok := oc.Answer["text"].(string); ok && strings.TrimSpace(text) != "" {
		emit(Event{Type: "report", Output: "（mod）" + text})
	}
}

// ModTools 返回 mod 注册的工具（mcp__<插件名>__<工具>，对齐 Claude 命名），
// 挂进 toolsFor 的动态目录段；调用经 tool.call 事件路由回注册的 handler。
func (r *CCPluginRegistry) ModTools() []Tool {
	if r == nil {
		return nil
	}
	r.modMu.RLock()
	defer r.modMu.RUnlock()
	out := []Tool{}
	for _, h := range r.modHosts {
		h.mu.Lock()
		for _, t := range h.tools {
			tool := t
			host := h
			full := "mcp__" + host.plugin.Name + "__" + tool.Name
			params := tool.InputSchema
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			desc := tool.Description
			if desc == "" {
				desc = "mod 工具（来自插件 " + host.plugin.Name + "）"
			}
			out = append(out, Tool{
				Spec: ToolDef{Type: "function", Function: ToolSpec{
					Name: full, Description: desc, Parameters: params,
				}},
				Run: func(t *toolContext, args json.RawMessage) (string, error) {
					return t.app.ccModToolCall(t.ctx, t.sess, host, tool.Name, args)
				},
			})
		}
		h.mu.Unlock()
	}
	return out
}

// ccModToolCall 执行一个 mod 工具（触发 tool.call 事件，插件内按 matcher
// 路由；handler 不处理则如实提示）。
func (a *App) ccModToolCall(ctx context.Context, sess *SessionMeta, h *modHost, tool string, args json.RawMessage) (string, error) {
	full := "mcp__" + h.plugin.Name + "__" + tool
	h.mu.Lock()
	oc, err := h.dispatchEvent("tool.call", ccModToolArgs(full, args), &modSessCtx{Sess: sess, Ctx: ctx})
	h.mu.Unlock()
	if err != nil {
		return "", err
	}
	if oc.Answer != nil {
		if deny, ok := oc.Answer["deny"].(string); ok {
			return "", fmt.Errorf("mod 拒绝调用: %s", deny)
		}
		if res, ok := oc.Answer["result"].(string); ok {
			return res, nil
		}
	}
	return "（mod 工具 " + tool + " 已分发，但插件没有处理 " + full + " 的 tool.call）", nil
}
