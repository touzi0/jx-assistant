package main

// 上下文材料「比对只在引擎做一次」的验收测试（ContextEngine 契约 6）：
//  1. OpenRound 随快照行记录 chg：首条 first、无变化全零、系统提示词变化带 prevSys；
//     实时推送与会话详情同一瘦身口径——无变化只推空壳 {}；
//  2. 子对话首轮以父会话最后一条快照为基线：相同即无变化；模型侧不发「工具目录
//     变更」公告（子对话的模型没见过父对话的目录）；
//  3. renderEnvRows 与组装层原内联渲染逐字节一致，摘要覆盖边界内的快照不产出。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func promptRowsOf(t *testing.T, a *App, id string) []promptRec {
	t.Helper()
	raw, err := a.Store.RawMessagesAll(id)
	if err != nil {
		t.Fatal(err)
	}
	var out []promptRec
	for _, m := range raw {
		if m.Kind != kindPrompt {
			continue
		}
		var p promptRec
		if err := json.Unmarshal([]byte(m.Text()), &p); err != nil {
			t.Fatalf("prompt 行损坏: %v", err)
		}
		out = append(out, p)
	}
	return out
}

func TestOpenRoundRecordsChange(t *testing.T) {
	a, main := newDispatchTestApp(t)
	var events []Event
	emit := func(ev Event) { events = append(events, ev) }

	a.ctxEngine().OpenRound(main, emit)
	rows := promptRowsOf(t, a, main.ID)
	if len(rows) != 1 || rows[0].Chg == nil || !rows[0].Chg.First {
		t.Fatalf("首条快照应带 chg.first: %+v", rows)
	}
	if len(events) != 1 || events[0].Kind != kindPrompt || events[0].Message.Content == "{}" {
		t.Fatalf("首条快照实时推送应为全文: %+v", events)
	}

	// 同一状态再开一轮：无变化 → chg 全零，实时只推空壳
	events = nil
	a.ctxEngine().OpenRound(main, emit)
	rows = promptRowsOf(t, a, main.ID)
	if len(rows) != 2 || rows[1].Chg == nil || rows[1].Chg.changed() {
		t.Fatalf("无变化的快照 chg 应全零: %+v", rows[1].Chg)
	}
	if len(events) != 1 || events[0].Message.Content != "{}" {
		t.Fatalf("无变化的快照实时推送应为空壳: %q", events[0].Message.Content)
	}

	// 覆盖文件改变系统提示词：chg.sys + prevSys = 上一条全文，实时推全文
	if err := os.MkdirAll(filepath.Join(a.Cfg.DataDir, "prompts"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.Cfg.DataDir, "prompts", main.Mode+".md"), []byte("覆盖后的系统提示词"), 0644); err != nil {
		t.Fatal(err)
	}
	events = nil
	a.ctxEngine().OpenRound(main, emit)
	rows = promptRowsOf(t, a, main.ID)
	if len(rows) != 3 {
		t.Fatalf("应有三条快照: %d", len(rows))
	}
	chg := rows[2].Chg
	if chg == nil || !chg.Sys || chg.PrevSys != rows[1].Sys || rows[2].Sys == rows[1].Sys {
		t.Fatalf("系统提示词变化应记 chg.sys 与 prevSys: %+v", chg)
	}
	if len(events) != 1 || !strings.Contains(events[0].Message.Content.(string), `"sys":true`) {
		t.Fatalf("有变化的快照实时推送应为全文且带 chg: %q", events[0].Message.Content)
	}
}

func TestOpenRoundChildUsesParentBaseline(t *testing.T) {
	a, main := newDispatchTestApp(t)
	a.ctxEngine().OpenRound(main, func(Event) {})

	child := &SessionMeta{ID: "child", Title: "子对话", Mode: main.Mode, ParentID: main.ID,
		Workspace: a.Cfg.Workspace, CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(child); err != nil {
		t.Fatal(err)
	}
	a.ctxEngine().OpenRound(child, func(Event) {})
	rows := promptRowsOf(t, a, child.ID)
	if len(rows) != 1 || rows[0].Chg == nil || rows[0].Chg.changed() {
		t.Fatalf("子对话首轮与父对话相同应判无变化（基线取父会话）: %+v", rows[0].Chg)
	}

	// 换一个工具目录不同的模式：相对父对话有增删，但模型侧不发目录变更公告
	other := &SessionMeta{ID: "child2", Title: "子对话2", Mode: "coding", ParentID: main.ID,
		Workspace: a.Cfg.Workspace, CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(other); err != nil {
		t.Fatal(err)
	}
	a.ctxEngine().OpenRound(other, func(Event) {})
	rows = promptRowsOf(t, a, other.ID)
	if len(rows) != 1 || rows[0].Chg == nil || rows[0].Chg.First {
		t.Fatalf("子对话首轮有父对话可比，不应是 first: %+v", rows[0].Chg)
	}
	raw, _ := a.Store.RawMessagesAll(other.ID)
	for _, m := range raw {
		if m.Kind == kindReport && strings.Contains(m.Text(), "【工具目录变更】") {
			t.Fatal("子对话首轮不该给模型发「工具目录变更」公告（它的模型没见过父对话的目录）")
		}
	}
}

func TestRenderEnvRowsMatchesInlineRendering(t *testing.T) {
	env := func(seq int64, txt string) *StoredMessage {
		return &StoredMessage{Message: TextMessage("system", txt), Seq: seq, Kind: "env"}
	}
	raw := []*StoredMessage{
		env(1, "# 运行环境快照\n- 时间：10:00"),
		env(2, "# 运行环境快照\n- 时间：10:00\n- 权限：只读"),
		env(3, "# 运行环境快照\n- 时间：10:00\n- 权限：修改"),
		{Message: TextMessage("user", "问"), Seq: 4, Kind: "chat"},
		env(5, "# 运行环境快照\n- 时间：10:01\n- 权限：修改"),
		{Message: TextMessage("user", "摘要"), Seq: 6, Kind: "summary", CoversUpTo: 1, Origin: "auto"},
	}
	boundary := int64(1)
	// 组装层原内联口径作为对照（首条整份、后续差分、连续差分满阈值完整重发）
	prevEnv, updates := "", 0
	want := map[int64]string{}
	for _, m := range raw {
		if m.Seq <= boundary || m.Kind != "env" {
			continue
		}
		if s, n := envRowRender(prevEnv, m.Text(), updates); s != "" {
			want[m.Seq] = s
			updates = n
		}
		prevEnv = m.Text()
	}
	got := renderEnvRows(raw, boundary)
	if len(got) != len(want) {
		t.Fatalf("产出条数不一致: got %d want %d", len(got), len(want))
	}
	for seq, text := range want {
		if got[seq].Text != text {
			t.Fatalf("seq %d 渲染文本不一致:\n got: %q\nwant: %q", seq, got[seq].Text, text)
		}
	}
	if _, ok := got[1]; ok {
		t.Fatal("摘要覆盖边界内的快照模型看不到，不应产出 render")
	}
	if got[2].Mode != "full" || got[3].Mode != "diff" || got[5].Mode != "diff" {
		t.Fatalf("形态应为 整份/差分/差分: %s %s %s", got[2].Mode, got[3].Mode, got[5].Mode)
	}
	// 连续差分满阈值（5 条）→ 下一次变化完整重发校准，其后又回到差分
	many := []*StoredMessage{env(1, "# 运行环境快照\n- 时间：10:00")}
	for i := int64(2); i <= 8; i++ {
		many = append(many, env(i, "# 运行环境快照\n- 时间：10:0"+string(rune('0'+i-1))+"\n- 权限：修改"))
	}
	all := renderEnvRows(many, 0)
	if all[1].Mode != "full" || all[6].Mode != "diff" || all[7].Mode != "resend" || all[8].Mode != "diff" {
		t.Fatalf("形态应为 整份 / 5 条差分 / 完整重发 / 差分: %s %s %s %s", all[1].Mode, all[6].Mode, all[7].Mode, all[8].Mode)
	}
	if !strings.HasPrefix(all[7].Text, "# 运行环境快照（完整重发") {
		t.Fatalf("完整重发文本应以声明开头: %q", all[7].Text)
	}
}
