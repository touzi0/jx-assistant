package main

// 合成工具回执对（origin=toolreport，runtime.go landToolPair 一族）的关键语义：
//   · 落库形态＝一次真实工具调用：assistant(tool_calls)+紧随的 tool 两行 kind=chat，
//     组装层（buildContext）原样进上下文且两行紧邻（repairToolPairing 不需要动手）；
//   · 会话忙（runners 在）→ 进插话队列（ToolName 带着回执身份），时机与派发
//     回报/用户插话同一套；
//   · 空闲 + NoKick → 只落对不起轮（被终止作业/超时回执的纪律）；
//   · 完整配对不受 repairDanglingToolCalls 影响（不会误补合成结尾）。

import (
	"encoding/json"
	"strings"
	"testing"
)

func toolReportTestApp(t *testing.T) (*App, *SessionMeta) {
	t.Helper()
	st := testPlanStore(t)
	sess := createPlanSession(t, st, "s1", "回执对会话", false)
	a := newPlanTestApp(t, st)
	return a, sess
}

func sampleCmdJobPair() *PendingMsg {
	args, _ := json.Marshal(map[string]any{"command": "ping -n 2 127.0.0.1", "cwd": "D:\\tmp", "run_in_background": true})
	return toolReportPair("run_command", string(args),
		"【后台命令回报】作业 job-x1 已结束（exit 0，运行 2s）。\n命令：ping -n 2 127.0.0.1\n\n输出：\nOK")
}

// 落库形态与上下文组装：两行 kind=chat、紧邻配对、原样进 buildContext
func TestLandToolPairRowsAndContext(t *testing.T) {
	a, sess := toolReportTestApp(t)
	a.deliverToolReportNoKick(sess, sampleCmdJobPair())

	rows, err := a.Store.RawMessagesAll(sess.ID)
	if err != nil {
		t.Fatalf("读历史失败: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("应落 assistant+tool 两行，实际 %d 行", len(rows))
	}
	asm, tm := rows[0], rows[1]
	if asm.Role != "assistant" || len(asm.ToolCalls) != 1 || asm.Kind != kindChat || asm.Origin != originToolReport {
		t.Fatalf("首行应为带 tool_calls 的 assistant（kind=chat, origin=toolreport），实际 %+v", asm)
	}
	if asm.ToolCalls[0].Function.Name != "run_command" || asm.ToolCalls[0].ID == "" {
		t.Fatalf("回显的调用名不对：%+v", asm.ToolCalls[0])
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(asm.ToolCalls[0].Function.Arguments), &args); err != nil || args["command"] != "ping -n 2 127.0.0.1" {
		t.Fatalf("回显参数应可解析且含 command，实际 %v (%v)", asm.ToolCalls[0].Function.Arguments, err)
	}
	if tm.Role != "tool" || tm.ToolCallID != asm.ToolCalls[0].ID || tm.Kind != kindChat || tm.Origin != originToolReport {
		t.Fatalf("次行应为配对的 tool 结果行，实际 %+v", tm)
	}
	if !strings.Contains(tm.Text(), "【后台命令回报】") {
		t.Fatalf("结果正文应含回执标题，实际 %q", tm.Text())
	}
	if tm.Seq != asm.Seq+1 {
		t.Fatalf("两行应紧邻落库（seq %d → %d）", asm.Seq, tm.Seq)
	}
	// 空闲 + NoKick：不起轮（执行注册表仍空）
	if a.IsRunning(sess.ID) {
		t.Fatal("NoKick 投递不得拉起执行")
	}

	// 组装：system 后紧跟 assistant(tool_use)+tool(result)，无需修复重排
	msgs := a.buildContext(sess, "sys", 0)
	if len(msgs) != 3 {
		t.Fatalf("组装应为 system+assistant+tool 三条，实际 %d", len(msgs))
	}
	if msgs[1].Role != "assistant" || len(msgs[1].ToolCalls) != 1 {
		t.Fatalf("组装第二条应为 assistant(tool_calls)，实际 %+v", msgs[1])
	}
	if msgs[2].Role != "tool" || msgs[2].ToolCallID != msgs[1].ToolCalls[0].ID {
		t.Fatalf("组装第三条应为紧邻配对的 tool 结果，实际 %+v", msgs[2])
	}
	fixed := repairToolPairing(msgs)
	if len(fixed) != 3 || fixed[1].Role != "assistant" || fixed[1].ToolCalls[0].ID != msgs[1].ToolCalls[0].ID ||
		fixed[2].ToolCallID != msgs[2].ToolCallID || fixed[2].Text() != msgs[2].Text() {
		t.Fatal("配对完整的序列不应被 repairToolPairing 改动")
	}

	// 完整配对不受悬空修复影响
	a.repairDanglingToolCalls(sess, nil)
	after, _ := a.Store.RawMessagesAll(sess.ID)
	if len(after) != 2 {
		t.Fatalf("完整回执对不应被补合成结尾，实际 %d 行", len(after))
	}
}

// 会话忙：回执对进插话队列（带着工具身份），不直接落库
func TestToolReportSteerWhileRunning(t *testing.T) {
	a, sess := toolReportTestApp(t)
	a.runners[sess.ID] = &sessionRun{} // 会话正在跑：投递走插话，不真跑模型

	a.startToolReportRound(sess, sampleCmdJobPair())

	st := a.pendingOf(sess.ID)
	if len(st.Steer) != 1 {
		t.Fatalf("应进插话队列 1 条，实际 %+v", st.Steer)
	}
	pm := st.Steer[0]
	if pm.ToolName != "run_command" || pm.Origin != originToolReport {
		t.Fatalf("插话项应带工具回执身份，实际 toolName=%q origin=%q", pm.ToolName, pm.Origin)
	}
	if !strings.Contains(pm.Content, "【后台命令回报】") {
		t.Fatalf("插话项正文应为回执文本，实际 %q", pm.Content)
	}
	// 排水后由 interjectsBefore 落对：这里直接调 landToolPair 验证落库收口一致
	a.landToolPair(sess, pm, nil)
	rows, _ := a.Store.RawMessagesAll(sess.ID)
	if len(rows) != 2 || rows[0].Role != "assistant" || rows[1].Role != "tool" {
		t.Fatalf("排水落库应为 assistant+tool 两行，实际 %d 行", len(rows))
	}
}

// 超长回执正文过 clipToolResult 封顶（防单条回执吃掉半个上下文窗口）
func TestToolReportResultClipped(t *testing.T) {
	a, sess := toolReportTestApp(t)
	pm := toolReportPair("run_python", `{"path":"x.py","run_in_background":true}`,
		"【后台脚本回报】作业 job-x2 已结束。\n输出：\n"+strings.Repeat("巨", 200000))
	a.deliverToolReportNoKick(sess, pm)
	rows, _ := a.Store.RawMessagesAll(sess.ID)
	if len(rows) != 2 {
		t.Fatalf("应落两行，实际 %d", len(rows))
	}
	if !strings.Contains(rows[1].Text(), "工具结果过长已截断") {
		t.Fatalf("超长回执应被截断标注，实际长度 %d", len(rows[1].Text()))
	}
}
