package main

// Claude Code 插件组件桥：commands（斜杠命令）。
// 插件的 commands/<名>.md 注册为 /<插件>:<路径:命令>；用户在输入框发送
// /命令 参数… 时，handleChat 在落库前展开：frontmatter 只取 description 与
// argument-hint，正文替换 $ARGUMENTS / $1..$9 / ${CLAUDE_PLUGIN_ROOT} 等占位
// 符后作为用户消息落库（展开标记行在消息开头，前端照常渲染用户气泡）。
// manifest.commands 支持 string / []string（路径替换默认扫描）与对象映射
// （每项 source 引用文件或 content 内联）。

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ccCommandEntry 是插件携带的一个斜杠命令。
type ccCommandEntry struct {
	PluginKey   string
	PluginName  string
	Name        string // 完整命令名（不含 /）：<插件>:<路径:文件名>
	Description string
	ArgumentHint string
	Body        string // 原文（展开时替换占位符）
	Source      string // 来源文件（内联命令为空）
}

// loadCommands 收集插件的全部命令。
func (p *CCPlugin) loadCommands() {
	p.Commands = nil
	if raw := p.Manifest.Commands; len(raw) > 0 && raw[0] == '{' {
		p.loadInlineCommands(raw)
		return
	}
	for _, d := range ccStringList(p.Manifest.Commands, "commands") {
		dir := filepath.Join(p.Root, filepath.FromSlash(d))
		p.loadCommandDir(dir, "")
	}
}

// loadCommandDir 递归扫描命令目录（子目录路径拼进命令名，/ → :）。
func (p *CCPlugin) loadCommandDir(dir, prefix string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if e.IsDir() {
			p.loadCommandDir(filepath.Join(dir, name), prefix+name+":")
			continue
		}
		if !strings.EqualFold(filepath.Ext(name), ".md") {
			continue
		}
		cmdName := strings.TrimSuffix(name, filepath.Ext(name))
		p.addCommandFile(filepath.Join(dir, name), prefix+cmdName, "")
	}
}

// loadInlineCommands 解释 manifest.commands 的对象映射：
// {<命令名>: {source: "./x.md"} | {content: "...", description: "..."}}。
func (p *CCPlugin) loadInlineCommands(raw []byte) {
	var defs map[string]struct {
		Source      string `json:"source"`
		Content     string `json:"content"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(raw, &defs); err != nil {
		return
	}
	names := make([]string, 0, len(defs))
	for k := range defs {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, n := range names {
		def := defs[n]
		if def.Source != "" {
			src := filepath.Join(p.Root, filepath.FromSlash(strings.TrimPrefix(def.Source, "./")))
			p.addCommandFile(src, n, def.Description)
			continue
		}
		if strings.TrimSpace(def.Content) == "" {
			continue
		}
		body := def.Content
		// content 允许带 frontmatter（描述优先取映射里的 description）
		fm, main := parseCCFrontmatter(body)
		if def.Description == "" {
			def.Description, _ = fm["description"].(string)
		}
		p.Commands = append(p.Commands, ccCommandEntry{
			PluginKey: p.Key, PluginName: p.Name,
			Name: p.Name + ":" + n, Description: def.Description, Body: main,
		})
	}
}

// addCommandFile 从 .md 文件登记一个命令。
func (p *CCPlugin) addCommandFile(path, relName, descOverride string) {
	fm, body := parseCCFrontmatter(readFileOr(path))
	if strings.TrimSpace(body) == "" {
		return
	}
	desc := descOverride
	if desc == "" {
		desc, _ = fm["description"].(string)
	}
	hint, _ := fm["argument-hint"].(string)
	p.Commands = append(p.Commands, ccCommandEntry{
		PluginKey: p.Key, PluginName: p.Name,
		Name: p.Name + ":" + relName, Description: desc, ArgumentHint: hint,
		Body: body, Source: path,
	})
}

// ccCommands 返回全部启用插件中组件放行的命令（按名排序）。
func (r *CCPluginRegistry) ccCommands() []ccCommandEntry {
	out := []ccCommandEntry{}
	for _, p := range r.enabled() {
		if !p.componentEnabled("commands") {
			continue
		}
		out = append(out, p.Commands...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ccCommandByName 按完整名（插件:命令）查命令；裸名在全局唯一时也接受
// （对齐 Claude「无占用时可短名调用」的规则）。
func (r *CCPluginRegistry) ccCommandByName(name string) (ccCommandEntry, *CCPlugin, bool) {
	name = strings.TrimPrefix(strings.TrimSpace(name), "/")
	var shortHit ccCommandEntry
	var shortPlugin *CCPlugin
	shortCount := 0
	for _, p := range r.enabled() {
		if !p.componentEnabled("commands") {
			continue
		}
		for _, c := range p.Commands {
			if c.Name == name {
				return c, p, true
			}
			// 裸名匹配：命令名的最后一段（或去掉 插件: 前缀）与 name 相等
			if stripPluginPrefix(c.Name) == name {
				shortHit, shortPlugin, shortCount = c, p, shortCount+1
			}
		}
	}
	if shortCount == 1 {
		return shortHit, shortPlugin, true
	}
	return ccCommandEntry{}, nil, false
}

// stripPluginPrefix 去掉命令名的 插件: 前缀（首段不含 / 时视为命名空间段）。
func stripPluginPrefix(name string) string {
	if i := strings.Index(name, ":"); i > 0 {
		return name[i+1:]
	}
	return name
}

// ccExpandSlashMessage 尝试把用户消息的首个词解释为斜杠命令并展开。
// 三种来源按序尝试：插件 commands/*.md 正文展开、mods 注册的命令（触发
// command.run 事件取回文本）、未命中原样返回（模型自行处理，避免误伤普通输入）。
func (a *App) ccExpandSlashMessage(ctx context.Context, sess *SessionMeta, content string) string {
	if !strings.HasPrefix(strings.TrimSpace(content), "/") {
		return content
	}
	first := strings.Fields(strings.TrimSpace(content))
	if len(first) == 0 {
		return content
	}
	cmdName := strings.TrimPrefix(first[0], "/")
	args := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(content), first[0]))
	// 会话级插件覆盖：本对话把某插件关了，它的 mod 命令与插件命令都不展开
	pluginAllowed := func(key string) bool {
		return sess == nil || a.cxoPluginEnabled(sess.ID, "claude", key)
	}
	// mod 注册的命令：执行 command.run，返回文本作为消息
	if a.CCPlugins != nil && a.CCPlugins.hasMods() {
		if text, ok := a.ccModRunCommand(ctx, sess, cmdName, args); ok {
			if strings.TrimSpace(text) == "" {
				text = "（mod 命令 " + cmdName + " 已执行，无输出）"
			}
			return "> （斜杠命令 /" + cmdName + " 的执行结果，来自 mod）\n\n" + text
		}
	}
	// 插件命令：正文展开
	if a.CCPlugins == nil {
		return content
	}
	entry, plugin, ok := a.CCPlugins.ccCommandByName(cmdName)
	if !ok || !pluginAllowed(plugin.Key) {
		return content
	}
	userCfg := a.CCPlugins.UserConfigOf(plugin)
	expanded := plugin.expandVars(entry.Body, userCfg)
	expanded = expandCommandPlaceholders(expanded, args)
	return "> （斜杠命令 " + first[0] + " 的展开内容，来自插件 " + entry.PluginName + "）\n\n" + expanded
}

// expandCommandPlaceholders 替换命令正文的占位符：$ARGUMENTS（全部参数）、
// $1..$9（按空白分词的位置参数）、${CLAUDE_PROJECT_DIR}；\$ 转义的字面美元
// 保留（如 \$1.00）。
func expandCommandPlaceholders(body, args string) string {
	// 转义占位：\$ → \x00ESC\x00，替换完再还原
	const escDollar = "\x00DOLLAR\x00"
	body = strings.ReplaceAll(body, `\$`, escDollar)
	words := strings.Fields(args)
	for i := 1; i <= 9; i++ {
		placeholder := "$" + string(rune('0'+i))
		val := ""
		if i <= len(words) {
			val = words[i-1]
		}
		body = strings.ReplaceAll(body, placeholder, val)
	}
	body = strings.ReplaceAll(body, "$ARGUMENTS", args)
	body = strings.ReplaceAll(body, escDollar, "$")
	return body
}

// ---------- HTTP ----------

// handleCCPluginCommands 给前端 / 命令弹层：命令清单（插件命令 + mod 命令）。
// ?s=<会话id>：按该会话的插件生效集过滤（会话把某插件关了，它的命令不出场）。
func (a *App) handleCCPluginCommands(w http.ResponseWriter, r *http.Request) {
	if a.CCPlugins == nil {
		writeJSON(w, 200, map[string]any{"commands": []any{}})
		return
	}
	sid := strings.TrimSpace(r.URL.Query().Get("s"))
	allow := func(pluginKey string) bool { return sid == "" || a.cxoPluginEnabled(sid, "claude", pluginKey) }
	out := []map[string]any{}
	for _, c := range a.CCPlugins.ccCommands() {
		if !allow(c.PluginKey) {
			continue
		}
		out = append(out, map[string]any{"name": c.Name, "description": c.Description, "argumentHint": c.ArgumentHint, "plugin": c.PluginName})
	}
	for _, c := range a.CCPlugins.ccModCommandListFiltered(allow) {
		out = append(out, c)
	}
	writeJSON(w, 200, map[string]any{"commands": out})
}
