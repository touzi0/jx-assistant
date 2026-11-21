package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// 历史查询工具：让模型能找到并读取任何会话的原生历史（未压缩原文，
// append-only 永久保留）。对齐 DSH 的 session_search / session_event_read 思路，
// 体验像翻一份超长对话日志：search 定位（命中给 seq 与上下文片段）→
// read_session_messages 从命中位置往后按字数预算分页读取。
//
// 单条消息的完整性是硬要求：早先的实现把每条消息固定截到 3000 字，于是几千字的
// 最终报告读到固定位置必断、**加大 chars 预算也没用**。现在两条路都通：
//   - 窗口读取（from_seq）按预算装，装不下就把预算填满并写明「本条共 X 字，已显示
//     前 Y 字」+ 续读入口（seq= 或 seq= + from_char=）；该档位下工具调用参数按
//     argKeepLimit 折叠（预算场景的防灌入，seq 完整读取是兜底入口）；
//   - 单条读取（seq）整条完整返回——正文、思考、工具调用参数全部原样
//     （histSingleMax 是唯一上限，超了给 from_char 续读）。

const (
	histEntryClip = 3000   // 倒序翻页视图里单条消息的截断长度（截断时给完整读取入口）
	histCharsDef  = 6000   // 默认单次返回的总字数预算
	histCharsMax  = 20000  // 单次返回总字数上限
	histSingleMax = 100000 // 单条完整读取（seq 模式）单次返回上限，超了给 from_char 续读起点
)

func searchSessionsTool() Tool {
	return Tool{
		Spec: toolSpec("search_sessions", "跨所有会话（含任务会话、子任务、辅助对话）搜索消息原文：用户输入、助手正文、工具调用参数、思考过程、工具结果都能搜到，返回命中的会话标题、id、位置（seq）与片段。想找「之前哪个对话说过/做过什么」用它；命中后用 read_session_messages（from_seq=命中 seq）从该位置往后读原文。",
			objSchema(map[string]any{
				"query":   strProp("搜索关键词"),
				"limit":   intProp("最多返回的会话数，默认 20"),
				"context": intProp("每个命中的上下文字数（命中点前后各一半，如 2000=前1000字+后1000字），默认 0 只给短片段；上限 4000"),
			}, "query")),
		Run: runSearchSessions,
	}
}

func runSearchSessions(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		Query   string `json:"query"`
		Limit   int    `json:"limit"`
		Context int    `json:"context"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	radius := p.Context / 2
	if radius > 2000 {
		radius = 2000 // 上下文窗口合计上限 4000 字
	}
	hits, err := t.app.Store.SearchMessages(strings.TrimSpace(p.Query), p.Limit, radius)
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return "（没有命中）", nil
	}
	lines := []string{}
	for _, h := range hits {
		title := h.Title
		if title == "" {
			title = "(未命名)"
		}
		lines = append(lines, fmt.Sprintf("- %s｜id=%s｜模式=%s｜第 %d 条 %s：%s",
			title, h.SessionID, h.Mode, h.Seq, h.Role, h.Snippet))
	}
	return strings.Join(lines, "\n") + "\n（要细看用 read_session_messages（from_seq=命中 seq）从该位置往后读原文；只要那一条就传 seq=命中 seq，整条完整返回）", nil
}

func readSessionMessagesTool() Tool {
	return Tool{
		Spec: toolSpec("read_session_messages", "分页读取任意会话的原生历史（未压缩的原始记录，含思考过程、工具调用与完整结果）。主用法：search_sessions 命中后传 from_seq 从该条开始往后读（从旧到新），字数预算用尽自动停止、提示下个起点；只要某一条就传 seq，**整条完整返回、不再按 3000 字截断**（超长消息可用 from_char 续读剩余部分）；不带 from_seq / seq 时从最新往前翻页。",
			objSchema(map[string]any{
				"session_id": strProp("目标会话 id，来自 list_sessions 或 search_sessions"),
				"from_seq":   intProp("从第几条开始往后读（含）。配合搜索定位：看命中点之后的内容"),
				"seq":        intProp("只读某一条（配合返回里的 [seq] 标注或搜索命中的 seq）：整条完整返回，不受字数预算与 3000 字截断限制"),
				"from_char":  intProp("仅与 seq 同用：从本条正文的第 N 个字（0 基）开始读剩余部分——续读超长消息、接着上次读完的位置往下看"),
				"limit":      intProp("最多读取条数，默认 30，上限 200"),
				"chars":      intProp("本次返回的总字数预算，默认 6000，上限 20000；预算用尽自动停止（单条装不下时会填满预算并写明本条共多少字、怎么续读）"),
				"offset":     intProp("不带 from_seq / seq 时的旧用法：从最新往前偏移的条数，默认 0"),
				"role":       strProp("可选：只看某角色（user/assistant/tool）"),
			}, "session_id")),
		Run: runReadSessionMessages,
	}
}

func runReadSessionMessages(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		SessionID string `json:"session_id"`
		FromSeq   int64  `json:"from_seq"`
		Seq       int64  `json:"seq"`
		FromChar  int    `json:"from_char"`
		Limit     int    `json:"limit"`
		Chars     int    `json:"chars"`
		Offset    int    `json:"offset"`
		Role      string `json:"role"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	sid := strings.TrimSpace(p.SessionID)
	if sid == "" {
		return "", fmt.Errorf("session_id 不能为空")
	}
	if _, err := t.app.Store.Get(sid); err != nil {
		return "", fmt.Errorf("会话不存在：%s", sid)
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 30
	}
	if limit > 200 {
		limit = 200
	}
	chars := p.Chars
	if chars <= 0 {
		chars = histCharsDef
	}
	if chars > histCharsMax {
		chars = histCharsMax
	}
	all, err := t.app.Store.RawMessagesAll(sid)
	if err != nil {
		return "", err
	}
	// 只保留对话流：跳过摘要行、继承标记行、模型输入快照、审计行，以及
	// 上下文材料（环境快照/拼装快照）——读的是对话日志，不是上下文组装材料
	rows := []*StoredMessage{}
	for _, m := range all {
		if m.Kind == "summary" || m.Kind == "request" || m.Kind == inheritKind || m.Kind == "audit" || m.Kind == "env" || m.Kind == "prompt" {
			continue
		}
		if p.Role != "" && m.Role != p.Role {
			continue
		}
		rows = append(rows, m)
	}
	total := len(rows)

	// 单条完整读取（seq）：整条返回，不受预算与 histEntryClip 限制——几千字的最终
	// 报告、带完整思考的中间步，都能在这里一次读完
	if p.Seq > 0 {
		row, why := findHistoryRow(all, rows, p.Seq)
		if row == nil {
			return why, nil
		}
		body := renderHistoryEntryFull(row)
		r := []rune(body)
		start := p.FromChar
		if start < 0 {
			start = 0
		}
		if start >= len(r) {
			return fmt.Sprintf("（会话 %s 第 %d 条共 %d 字，from_char=%d 已到本条末尾）", sid, row.Seq, len(r), p.FromChar), nil
		}
		end := len(r)
		clipped := false
		if end-start > histSingleMax {
			end = start + histSingleMax
			clipped = true
		}
		out := fmt.Sprintf("（会话 %s 第 %d 条 %s，本条共 %d 字，本次显示第 %d–%d 字）\n%s",
			sid, row.Seq, row.Role, len(r), start+1, end, string(r[start:end]))
		if clipped {
			out += fmt.Sprintf("\n（本条共 %d 字，已显示第 %d–%d 字；继续读剩余部分：read_session_messages(session_id=%s, seq=%d, from_char=%d)）",
				len(r), start+1, end, sid, row.Seq, end)
		}
		return out, nil
	}

	// 正向窗口：从 from_seq（含）往后读，字数预算用尽即停——搜到位置后看
	// 「这段及之后」的主路径。单条装不下时把预算填满（旧实现固定截 3000 字，
	// 加大 chars 也没用）并明确告知本条共多少字、怎么续读。
	if p.FromSeq > 0 {
		start := sort.Search(len(rows), func(i int) bool { return rows[i].Seq >= p.FromSeq })
		if start >= len(rows) {
			return fmt.Sprintf("（会话 %s 共 %d 条记录，from_seq=%d 已越过最新记录）", sid, total, p.FromSeq), nil
		}
		end := start + limit
		if end > len(rows) {
			end = len(rows)
		}
		lines := []string{fmt.Sprintf("（会话 %s 共 %d 条，从第 %d 条开始从旧到新，预算 %d 字）", sid, total, rows[start].Seq, chars)}
		used, next := 0, int64(0)
		for i := start; i < end; i++ {
			head, body := historyRowParts(rows[i], false)
			headLen, bodyLen := len([]rune(head)), len([]rune(body))
			remain := chars - used - headLen
			if remain <= 0 {
				next = rows[i].Seq
				break
			}
			if bodyLen > remain {
				// 已显示的字数含标识头，from_char 也按整条（含头）计数——与 seq 单条
				// 读取的口径一致，续读位置才对得上
				totalLen, shown := headLen+bodyLen, headLen+remain
				lines = append(lines, head+string([]rune(body)[:remain])+
					fmt.Sprintf("…（本条共 %d 字，已显示前 %d 字；完整读取：read_session_messages(session_id=%s, seq=%d)；从第 %d 字往后续读：seq=%d, from_char=%d）",
						totalLen, shown, sid, rows[i].Seq, shown+1, rows[i].Seq, shown))
				next = rows[i].Seq
				break
			}
			used += headLen + bodyLen
			lines = append(lines, head+body)
			if i == end-1 {
				next = rows[i].Seq + 1
			}
		}
		if next > 0 && next <= rows[len(rows)-1].Seq {
			lines = append(lines, fmt.Sprintf("（已达读取边界，继续读用 from_seq=%d）", next))
		}
		return strings.Join(lines, "\n"), nil
	}

	// 倒序翻页（旧用法）：从最新往前取一页
	offset := p.Offset
	if offset < 0 {
		offset = 0
	}
	hi := total - offset
	if hi > total {
		hi = total
	}
	lo := hi - limit
	if lo < 0 {
		lo = 0
	}
	if hi <= 0 {
		return fmt.Sprintf("（会话 %s 共 %d 条记录，offset=%d 已越过最旧记录）", sid, total, offset), nil
	}
	page := rows[lo:hi]
	lines := []string{fmt.Sprintf("（会话 %s 共 %d 条，当前显示第 %d–%d 条，从旧到新）", sid, total, lo+1, hi)}
	for _, m := range page {
		lines = append(lines, renderHistoryEntry(m))
	}
	return strings.Join(lines, "\n"), nil
}

// findHistoryRow 在（已按 kind/role 过滤的）rows 里找 seq；找不到时看它是不是被
// 过滤掉的行——上下文材料（环境快照/摘要/审计行等）不进对话流，如实说明，别让
// 模型以为那条记录不存在。
func findHistoryRow(all, rows []*StoredMessage, seq int64) (*StoredMessage, string) {
	idx := sort.Search(len(rows), func(i int) bool { return rows[i].Seq >= seq })
	if idx < len(rows) && rows[idx].Seq == seq {
		return rows[idx], ""
	}
	for _, m := range all {
		if m.Seq != seq {
			continue
		}
		label := m.Kind
		switch m.Kind {
		case kindEnv:
			label = "环境快照"
		case kindPrompt:
			label = "拼装快照"
		case kindRequest:
			label = "请求快照"
		case kindAudit:
			label = "审计行"
		case kindSummary:
			label = "压缩摘要"
		case kindInherit:
			label = "继承标记"
		}
		return nil, fmt.Sprintf("（会话里的第 %d 条是 %s 行（role=%s），不属于对话流——本工具只读用户输入/助手正文/工具调用与结果；这类上下文材料可用「导出」功能查看）", seq, label, m.Role)
	}
	return nil, fmt.Sprintf("（会话里没有第 %d 条记录）", seq)
}

// renderHistoryEntry 把一条消息渲染为倒序翻页视图的文本行：思考过程前置
//（【思考】段），正文与工具调用并入同一条；超长时截断并给出完整读取入口。
func renderHistoryEntry(m *StoredMessage) string {
	head, body := historyRowParts(m, false)
	if r := []rune(body); len(r) > histEntryClip {
		return head + string(r[:histEntryClip]) +
			fmt.Sprintf("…（本条共 %d 字，已显示前 %d 字；完整读取：read_session_messages(session_id=%s, seq=%d)）",
				len(r), histEntryClip, m.SessionID, m.Seq)
	}
	return head + body
}

// renderHistoryEntryFull 完整渲染一条消息（seq 模式用）：正文、思考、工具调用
// 参数全部原样——单条完整读取的语义就是「原文」。防 write 大参数灌爆上下文由
// 调用方的 histSingleMax 预算 + from_char 续读兜底（分页可控，不需要在这里截）。
func renderHistoryEntryFull(m *StoredMessage) string {
	head, body := historyRowParts(m, true)
	return head + body
}

// historyRowParts 把一条消息拆成「标识头 + 正文」：正文保留原始换行（历史查询看
// 的就是原文格式）。思考原文完整保留；工具调用参数按档位折叠：full=false（倒序
// 翻页 / from_seq 窗口，字数预算场景）按 argKeepLimit 折叠并注明；full=true
// （seq 单条完整读取）**参数原样**——压缩折叠视图承诺的「全文取
// read_session_messages(seq=N)」必须兑现，否则被压缩的工具调用只剩前 500 字、
// 且渲染文本已「显示完整」连续读入口都没有，原文就再也找不回来了（前端能显示
// 的原文，模型就必须能读回；2026-11-19 修）。超长由调用方的预算与续读兜底。
func historyRowParts(m *StoredMessage, full bool) (head, body string) {
	head = fmt.Sprintf("[%d] %s：", m.Seq, m.Role)
	var b strings.Builder
	if m.Thinking != "" {
		b.WriteString("【思考】" + m.Thinking + "\n")
	}
	if s := m.Text(); strings.TrimSpace(s) != "" {
		b.WriteString(strings.TrimSpace(s))
	}
	for _, tc := range m.ToolCalls {
		args := tc.Function.Arguments
		note := ""
		if !full {
			if r := []rune(args); len(r) > argKeepLimit {
				args = string(r[:argKeepLimit])
				note = fmt.Sprintf("…（参数共 %d 字，折叠显示前 %d 字）", len(r), argKeepLimit)
			}
		}
		b.WriteString("\n[调用 " + tc.Function.Name + " 参数=" + args + note + "]")
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		out = "（无文本内容）"
	}
	return head, out
}

// clipRunes 按 rune 数截断（保留换行与原格式），区别于压平空白的 clipText。
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…（后面省略）"
}
