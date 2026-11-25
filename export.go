package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// 导出：一个会话产出两种审计材料，HTTP 层统一经上下文引擎申请
// （a.ctxEngine().ExportTimeline / ExportRawRequests）——引擎是记录者，
// 也是导出门面；本文件只剩时间线的排版实现。
//   运行日志（format=md）：从会话创建起的完整时间线。
//     既包含发给模型的内容（系统提示词组装、环境快照更新、用户消息、模型
//     逐步输出、工具调用与结果），也包含不发给模型但影响判断的事件（运行中
//     报错、用户停止、上下文超限重试、压缩生效、系统提示词变化、上下文事件）。
//     缓存命中率突降通常就发生在两次调用之间的这类事件上——文首的调用表
//     给出每次调用的命中量，对着时间线即可定位是哪次变更打破了前缀。
//   原始数据（format=json，contextengine.go 组装）：引擎缓存的最近三轮真正
//     发给模型的请求体**原样数组**（旧→新，与线上发送逐字节同源），不带任何
//     导出侧元数据——纯原始数据；会话信息与调用统计在运行日志里。
// 两者都取自 messages 表原始历史（append-only），与展示层无关。

func modeLabelOf(mode string) string {
	switch mode {
	case "task":
		return "任务"
	case "assistant":
		return "数字助理"
	case "aux":
		return "辅助对话"
	case "office":
		return "标准办公"
	case "coding":
		return "编程"
	case "beginner-coding":
		return "新手编程"
	case "skill-create":
		return "创建技能"
	}
	return mode
}

func onOff(b bool) string {
	if b {
		return "开"
	}
	return "关"
}

// htmlEscape 只用于 <pre> 块内的原文，防止内容里的标签破坏结构。
func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// expText 保证导出文档是合法 UTF-8：历史上控制台 GBK 输出可能以非法
// 字节入库，这里统一替换为 U+FFFD，文档不因坏字节损坏。
func expText(s string) string {
	return strings.ToValidUTF8(s, "\uFFFD")
}

// isSysNote 是旧库兼容判定（新行一律 kind=report，见《消息类型规范.md》§5）：
// 历史系统注入通知的前缀清单。
func isSysNote(text string) bool {
	for _, p := range []string{"【派发回报】", "【系统中断】", "【执行中断】", "【文件上传】", "【文件回退】",
		"【目标续跑】", "【派发子对话】", "【数字助理派发】", "【数字助理转达】", "【待办任务派发｜", "【网页查看】", "<goal_round>"} {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
}

func remoteSuffix(m *SessionMeta) string {
	if m.Remote == nil {
		return ""
	}
	u := m.Remote.User
	if u == "" {
		u = "root"
	}
	return fmt.Sprintf("（SSH 远程 %s@%s:%s）", u, m.Remote.Host, m.Remote.Dir)
}

// ---------- 运行日志（时间线） ----------

// exportTimeline 生成会话的运行日志 Markdown（引擎 ExportTimeline 门面的实现）。
func (a *App) exportTimeline(m *SessionMeta) string {
	msgs, err := a.Store.RawMessagesAll(m.ID)
	if err != nil {
		msgs = nil
	}
	var b strings.Builder
	writeTimelineHeader(&b, a, m, len(msgs))
	writeCallTable(&b, msgs)
	b.WriteString("---\n\n## 时间线\n\n")
	st := &timelineState{callNames: map[string]string{}}
	for _, row := range msgs {
		st.render(&b, row)
	}
	if len(msgs) == 0 {
		b.WriteString("（没有可展示的记录）\n")
	}
	return b.String()
}

func writeTimelineHeader(b *strings.Builder, a *App, m *SessionMeta, n int) {
	b.WriteString("# 运行日志：" + m.Title + "\n\n")
	info := []string{
		"- 会话：" + m.ID + "（" + modeLabelOf(m.Mode) + "）",
		"- 模型：" + m.Provider + "/" + m.Model,
		"- 工作区：" + wsOr(a, m) + remoteSuffix(m),
		"- 权限：" + permOf(m) + "｜计划模式：" + onOff(m.PlanMode) + "｜目标模式：" + onOff(m.GoalMode),
		"- 创建：" + m.CreatedAt + "｜导出：" + now() + "｜原始记录：" + fmt.Sprint(n) + " 条",
	}
	if m.ParentID != "" {
		info = append(info, "- 来源：派生自会话 "+m.ParentID)
	}
	b.WriteString(strings.Join(info, "\n") + "\n\n")
}

// writeCallTable 文首的模型调用一览：每次调用的输入/缓存命中/输出。
// 命中率突降时，对照下方时间线里两次调用之间的记录即可定位原因。
func writeCallTable(b *strings.Builder, msgs []*StoredMessage) {
	type entry struct {
		ts string
		ar auditRow
	}
	calls := []entry{}
	for _, m := range msgs {
		if m.Kind != "audit" {
			continue
		}
		var ar auditRow
		if json.Unmarshal([]byte(m.Text()), &ar) != nil || ar.Type != "call" {
			continue
		}
		calls = append(calls, entry{ts: m.CreatedAt, ar: ar})
	}
	if len(calls) == 0 {
		return
	}
	b.WriteString("## 模型调用与缓存命中\n\n")
	b.WriteString("| # | 时间 | 输入tok | 缓存命中 | 未命中 | 命中率 | 输出tok | 耗时ms | 结果 |\n")
	b.WriteString("|---:|---|---:|---:|---:|---:|---:|---:|---|\n")
	for i, c := range calls {
		res := "正文 " + fmt.Sprint(c.ar.Text) + " 字"
		if len(c.ar.Tools) > 0 {
			res += " + 工具 " + strings.Join(c.ar.Tools, ",")
		}
		if c.ar.Scope == "summarize" {
			res = "（摘要调用）" + res
		}
		if c.ar.Error != "" {
			res = "失败：" + clipText(c.ar.Error, 40)
		}
		in, hit, miss, rate, out := "-", "-", "-", "-", "-"
		if c.ar.Usage != nil {
			in = fmt.Sprint(c.ar.Usage.Prompt)
			hit = fmt.Sprint(c.ar.Usage.Hit)
			miss = fmt.Sprint(c.ar.Usage.Miss)
			out = fmt.Sprint(c.ar.Usage.Completion)
			if c.ar.Usage.Prompt > 0 {
				rate = fmt.Sprintf("%.1f%%", float64(c.ar.Usage.Hit)*100/float64(c.ar.Usage.Prompt))
			} else {
				rate = "（网关未返回）"
			}
		}
		b.WriteString(fmt.Sprintf("| %d | %s | %s | %s | %s | %s | %s | %d | %s |\n",
			i+1, c.ts, in, hit, miss, rate, out, c.ar.Ms, res))
	}
	b.WriteString("\n命中率为空（网关未返回 usage）时以输入/输出列为准。命中率突降通常意味着两次调用之间发生了改变上下文的事件（环境快照更新、摘要生成、规则压缩、系统提示词变化）——到时间线里找这两次调用之间的记录。\n\n")
}

// timelineState 是时间线渲染的跨行状态：上一次见到的系统提示词/环境快照/
// 工具集（供变更对比）、调用序号、工具调用配对表。
type timelineState struct {
	lastSys   string
	lastEnv   string
	lastTools []string
	roundNo   int
	callNo    int
	callNames map[string]string // toolCallID → 工具名（结果行配对用）
	sawPrompt bool
}

func (st *timelineState) render(b *strings.Builder, m *StoredMessage) {
	switch m.Kind {
	case "request":
		return // 全量请求体另行导出（format=json），时间线不重复展开
	case "audit":
		st.renderAudit(b, m)
		return
	}
	switch m.Kind {
	case "prompt":
		st.renderPrompt(b, m)
	case "env":
		st.renderEnv(b, m)
	case "inherit":
		var im inheritMark
		_ = json.Unmarshal([]byte(m.Text()), &im)
		tlLine(b, m.CreatedAt, fmt.Sprintf("继承主对话 %s 的历史 %d 条（含当时的摘要与压缩状态）", im.Parent, im.Count))
	case "summary":
		st.renderSummary(b, m)
	default: // chat / aiuser / report 等对话行
		switch m.Role {
		case "user":
			switch {
			case m.Kind == kindAiUser:
				tlLine(b, m.CreatedAt, "AI 发出的用户消息（派发指令/回报/待办转交，作为 user 消息发给模型）")
			case isSysNote(m.Text()) || m.Kind == "report":
				tlLine(b, m.CreatedAt, "系统注入（回报/中断/上传，之后作为 user 消息发给模型）")
			default:
				tlLine(b, m.CreatedAt, "用户消息")
			}
			tlQuote(b, expText(m.Text()))
		case "assistant":
			st.renderAssistant(b, m)
		case "tool":
			st.renderToolResult(b, m)
		}
	}
}

// renderPrompt 画一轮开轮快照：变化与否优先读引擎随行记录的 chg（promptChange，
// 比对只在引擎做一次）；老行（chg 字段出现前落的）回退到导出侧与上一条逐字比较。
func (st *timelineState) renderPrompt(b *strings.Builder, m *StoredMessage) {
	var p promptRec
	if err := json.Unmarshal([]byte(m.Text()), &p); err != nil {
		tlLine(b, m.CreatedAt, "上下文组装（提示词快照行损坏，无法解析）")
		return
	}
	st.roundNo++
	names := make([]string, 0, len(p.Tools))
	for _, t := range p.Tools {
		names = append(names, t.Function.Name)
	}
	head := fmt.Sprintf("上下文组装（第 %d 轮）：系统提示词 %d 字 · 环境快照 %d 字 · 工具 %d 个",
		st.roundNo, len([]rune(p.Sys)), len([]rune(p.Env)), len(p.Tools))
	sysChanged, toolsChanged := false, false
	var plus, minus []string
	prevSys := st.lastSys
	if p.Chg != nil {
		sysChanged, toolsChanged = p.Chg.Sys, len(p.Chg.ToolsAdd)+len(p.Chg.ToolsDel) > 0
		plus, minus = p.Chg.ToolsAdd, p.Chg.ToolsDel
		if p.Chg.PrevSys != "" {
			prevSys = p.Chg.PrevSys
		}
	} else {
		sysChanged, toolsChanged = p.Sys != st.lastSys, !sameStrings(names, st.lastTools)
		plus, minus = diffStrings(st.lastTools, names)
	}
	if !st.sawPrompt {
		tlLine(b, m.CreatedAt, head)
		tlFold(b, "系统提示词全文（请求里的第一条 system 消息）", p.Sys)
		tlFold(b, "环境快照全文（固定作为尾部 user 消息发送）", p.Env)
		tlFold(b, "工具定义完整 JSON（请求体里与消息平级的 tools 数组）", prettyJSON(p.Tools))
	} else {
		changed := false
		if sysChanged {
			changed = true
			tlLine(b, m.CreatedAt, head)
			b.WriteString("  系统提示词发生变化（前缀改变——此前已缓存的前缀将失效）\n")
			writeDiff(b, prevSys, p.Sys)
		}
		if toolsChanged {
			changed = true
			if !changed {
				tlLine(b, m.CreatedAt, head)
			}
			b.WriteString("  工具定义变化：")
			if len(minus) > 0 {
				b.WriteString("移除 " + strings.Join(minus, ",") + " ")
			}
			if len(plus) > 0 {
				b.WriteString("新增 " + strings.Join(plus, ","))
			}
			b.WriteString("\n")
			tlFold(b, "变化后的工具定义完整 JSON", prettyJSON(p.Tools))
		}
		if !changed {
			if p.Env != st.lastEnv {
				tlLine(b, m.CreatedAt, head+"（系统提示词与工具集不变；环境快照有更新，见下方记录）")
			} else {
				tlLine(b, m.CreatedAt, head+"（与上一轮一致）")
			}
		}
	}
	st.lastSys, st.lastEnv, st.lastTools, st.sawPrompt = p.Sys, p.Env, names, true
}

func (st *timelineState) renderEnv(b *strings.Builder, m *StoredMessage) {
	if m.Text() == st.lastEnv {
		return // 内容未变（不应发生，组装层有 diff）
	}
	tlLine(b, m.CreatedAt, "环境快照更新（固定作为尾部 user 消息随下次请求发送）")
	if st.lastEnv != "" {
		writeDiff(b, st.lastEnv, m.Text())
	}
	tlFold(b, "快照全文", m.Text())
	st.lastEnv = m.Text()
}

func (st *timelineState) renderSummary(b *strings.Builder, m *StoredMessage) {
	label := "自动"
	if m.Origin == "manual" {
		label = "手动"
	}
	suffix := ""
	if m.Superseded {
		suffix = "（已被更新的自动摘要合并，不再注入后续上下文）"
	}
	tlLine(b, m.CreatedAt, fmt.Sprintf("压缩摘要（%s · 覆盖至第 %d 条）%s：摘要会注入后续请求，其后的消息序列整体前移", label, m.CoversUpTo, suffix))
	tlFold(b, "摘要全文", m.Text())
}

func (st *timelineState) renderAssistant(b *strings.Builder, m *StoredMessage) {
	parts := []string{}
	if t := m.Text(); strings.TrimSpace(t) != "" {
		parts = append(parts, "正文 "+fmt.Sprint(len([]rune(t)))+" 字")
	}
	if m.Thinking != "" {
		parts = append(parts, "思考 "+fmt.Sprint(len([]rune(m.Thinking)))+" 字")
	}
	if len(m.ToolCalls) > 0 {
		names := []string{}
		for _, tc := range m.ToolCalls {
			names = append(names, tc.Function.Name)
			st.callNames[tc.ID] = tc.Function.Name
		}
		parts = append(parts, "工具调用 "+strings.Join(names, ","))
	}
	if len(parts) == 0 {
		parts = append(parts, "（无正文）")
	}
	tlLine(b, m.CreatedAt, "模型输出："+strings.Join(parts, " · "))
	if t := m.Text(); strings.TrimSpace(t) != "" {
		tlQuote(b, expText(t))
	}
	for _, tc := range m.ToolCalls {
		b.WriteString("  - 工具调用 " + tc.Function.Name + " 参数：" + clipText(expText(tc.Function.Arguments), 300) + "\n")
	}
	if m.Thinking != "" {
		tlFold(b, "思考过程全文", m.Thinking)
	}
}

func (st *timelineState) renderToolResult(b *strings.Builder, m *StoredMessage) {
	name := st.callNames[m.ToolCallID]
	if name == "" {
		name = "（未配对）"
	}
	out := expText(m.Text())
	tlLine(b, m.CreatedAt, fmt.Sprintf("工具结果 %s（%d 字）", name, len([]rune(out))))
	if len([]rune(out)) > 1500 {
		tlFold(b, "结果开头 1500 字（超长截断，全文见会话数据）", string([]rune(out)[:1500]))
	} else {
		tlQuote(b, out)
	}
}

func (st *timelineState) renderAudit(b *strings.Builder, m *StoredMessage) {
	var ar auditRow
	if err := json.Unmarshal([]byte(m.Text()), &ar); err != nil {
		return
	}
	switch ar.Type {
	case "call":
		st.callNo++
		head := fmt.Sprintf("（本轮第 %d 步）", ar.Step)
		if ar.Scope == "summarize" {
			head = "（摘要调用）"
		}
		tlLine(b, m.CreatedAt, fmt.Sprintf("模型调用 #%d%s：发送 %d 条消息 · 约 %d tok",
			st.callNo, head, ar.Msgs, ar.Est))
		switch {
		case ar.Error != "":
			b.WriteString("  失败：" + expText(ar.Error) + "\n\n")
		case ar.Usage != nil:
			rate := "（网关未返回命中率数据）"
			if ar.Usage.Prompt > 0 {
				rate = fmt.Sprintf("%.1f%%", float64(ar.Usage.Hit)*100/float64(ar.Usage.Prompt))
			}
			res := fmt.Sprintf("正文 %d 字", ar.Text)
			if len(ar.Tools) > 0 {
				res += " · 工具调用 " + strings.Join(ar.Tools, ",")
			}
			b.WriteString(fmt.Sprintf("  返回：%s · 输入 %d tok · 缓存命中 %d（%s）· 输出 %d tok · 耗时 %d ms\n\n",
				res, ar.Usage.Prompt, ar.Usage.Hit, rate, ar.Usage.Completion, ar.Ms))
		default:
			b.WriteString(fmt.Sprintf("  返回：正文 %d 字（网关未返回 usage）· 耗时 %d ms\n\n", ar.Text, ar.Ms))
		}
		if ar.SysHash != "" || ar.EnvHash != "" {
			b.WriteString(fmt.Sprintf("  前缀指纹：sys %s · env %s（若与上一次不同，则前缀缓存必不命中）\n\n", ar.SysHash, ar.EnvHash))
		}
	case "error":
		tlLine(b, m.CreatedAt, "报错（未发给模型）："+expText(ar.Error))
	case "stop":
		tlLine(b, m.CreatedAt, "用户停止执行")
	case "compact":
		tlLine(b, m.CreatedAt, fmt.Sprintf("规则压缩生效：折叠第 %d–%d 条（%d 轮）——模型可见前缀改变，下次调用缓存不命中", ar.From, ar.To, ar.Rounds))
	case "overflow":
		tlLine(b, m.CreatedAt, "上下文超限，已强制压缩后重发（该轮上下文被大幅改写）："+expText(ar.Error))
	case "ctx":
		// 上下文事件总线（contextengine.go 的 Notify）：会话配置类变更在此留痕，
		// 时间线上位于发生位置——下一次模型调用的缓存命中变化由此可解释
		tlLine(b, m.CreatedAt, "上下文变更（"+ar.Kind+"）："+expText(ar.Detail))
	}
}

// ---------- 渲染小件 ----------

func tlLine(b *strings.Builder, ts, label string) {
	if ts != "" {
		b.WriteString("[" + ts + "] " + label + "\n\n")
	} else {
		b.WriteString(label + "\n\n")
	}
}

func tlQuote(b *strings.Builder, text string) {
	for _, ln := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		b.WriteString("> " + ln + "\n")
	}
	b.WriteString("\n")
}

func tlFold(b *strings.Builder, summary, content string) {
	b.WriteString("<details><summary>" + summary + "</summary>\n\n<pre>" +
		htmlEscape(expText(content)) + "\n</pre>\n\n</details>\n\n")
}

// writeDiff 输出两段文本的行级差异（删除 - / 新增 +，各至多 15 行）。
func writeDiff(b *strings.Builder, old, cur string) {
	removed, added := lineDiff(old, cur)
	if len(removed) == 0 && len(added) == 0 {
		return
	}
	const capLines = 15
	for i, l := range removed {
		if i >= capLines {
			b.WriteString(fmt.Sprintf("  -（其余 %d 行省略）\n", len(removed)-capLines))
			break
		}
		b.WriteString("  - " + l + "\n")
	}
	for i, l := range added {
		if i >= capLines {
			b.WriteString(fmt.Sprintf("  +（其余 %d 行省略）\n", len(added)-capLines))
			break
		}
		b.WriteString("  + " + l + "\n")
	}
	b.WriteString("\n")
}

// lineDiff 按多重集合比较两段文本的行，返回被删行与新增行（保持原序）。
func lineDiff(old, cur string) (removed, added []string) {
	oldLines := strings.Split(old, "\n")
	curLines := strings.Split(cur, "\n")
	curCnt := map[string]int{}
	for _, l := range curLines {
		curCnt[l]++
	}
	for _, l := range oldLines {
		if curCnt[l] > 0 {
			curCnt[l]--
			continue
		}
		removed = append(removed, l)
	}
	oldCnt := map[string]int{}
	for _, l := range oldLines {
		oldCnt[l]++
	}
	for _, l := range curLines {
		if oldCnt[l] > 0 {
			oldCnt[l]--
			continue
		}
		added = append(added, l)
	}
	return removed, added
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func diffStrings(old, cur []string) (plus, minus []string) {
	cnt := map[string]int{}
	for _, s := range old {
		cnt[s]++
	}
	for _, s := range cur {
		if cnt[s] > 0 {
			cnt[s]--
			continue
		}
		plus = append(plus, s)
	}
	cnt2 := map[string]int{}
	for _, s := range cur {
		cnt2[s]++
	}
	for _, s := range old {
		if cnt2[s] > 0 {
			cnt2[s]--
			continue
		}
		minus = append(minus, s)
	}
	return plus, minus
}

func prettyJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	var out bytes.Buffer
	if json.Indent(&out, raw, "", "  ") != nil {
		return string(raw)
	}
	return out.String()
}
