package main

// progress_write 的两种用法（tools.go）：
//   · action 缺省 / set → 与改造前逐字节一致（整体替换、空数组清空、只能一项
//     in_progress）；**漏传 progress 不再被当成"清空"**（那会静默抹掉清单），
//     改为报错提示补参数或改用 list。
//   · action=list → 只读，返回前端那把进度面板的同源数据（进度清单 + 运行中
//     的程序），一次输出带状态符号、命令截断、条数封顶；不写库。

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// addFakeChild 往「运行中的程序」登记表塞一条记录（进度面板的数据源）。
func addFakeChild(t *testing.T, sessionID, id, label, command string, startedAgo time.Duration, exit string) {
	t.Helper()
	c := &childRun{
		ID: id, Label: label, SessionID: sessionID, Command: command,
		StartedAt: time.Now().Add(-startedAgo),
	}
	if exit != "" {
		c.setExit(exit)
	}
	childRegMu.Lock()
	childReg[id] = c
	childRegMu.Unlock()
	t.Cleanup(func() {
		childRegMu.Lock()
		delete(childReg, id)
		childRegMu.Unlock()
	})
}

func TestProgressListSnapshot(t *testing.T) {
	st := testPlanStore(t)
	a := newPlanTestApp(t, st)
	sess := createPlanSession(t, st, "s1", "会话一", false)
	tc := &toolContext{app: a, sess: sess}

	// 空清单也要能读（不能报错，也不能把"空"说成"没有这个功能"）
	out, err := runProgressWrite(tc, json.RawMessage(`{"action":"list"}`))
	if err != nil || !strings.Contains(out, "进度清单：（空") || !strings.Contains(out, "运行中的程序：无") {
		t.Fatalf("空会话的 list 输出不对：%q err=%v", out, err)
	}

	// 写一份清单，再读回来
	if _, err = runProgressWrite(tc, json.RawMessage(
		`{"progress":[{"content":"读完 interact.go","status":"completed"},{"content":"改工具签名","status":"in_progress"},{"content":"补单测","status":"pending"}]}`)); err != nil {
		t.Fatalf("写清单失败: %v", err)
	}
	addFakeChild(t, "s1", "child-run1", "job:run_in_background", "pytest -q", 80*time.Second, "")
	addFakeChild(t, "s1", "child-done1", "cmd /c build.bat", "", 5*time.Second, "exit 0")

	out, err = runProgressWrite(tc, json.RawMessage(`{"action":"list"}`))
	if err != nil {
		t.Fatalf("list 失败: %v", err)
	}
	for _, want := range []string{
		"进度清单：3 项，完成 1",
		"✔ 读完 interact.go",
		"▶ 改工具签名",
		"· 补单测",
		"运行中的程序：2 条（1 个在跑）",
		"job:run_in_background｜pytest -q｜已跑 1m20s｜运行中",
		"cmd /c build.bat｜｜已跑 5s｜已结束（exit 0）",
		"job_output",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("list 输出缺少 %q：\n%s", want, out)
		}
	}

	// 只读：库里那份清单一个字节都不能动
	if got := st.GetSessionData("s1", "progress"); !strings.Contains(got, "改工具签名") {
		t.Fatalf("list 不该写库，实际库内容：%q", got)
	}
}

func TestProgressWriteStillReplacesAndClears(t *testing.T) {
	st := testPlanStore(t)
	a := newPlanTestApp(t, st)
	sess := createPlanSession(t, st, "s1", "会话一", false)
	tc := &toolContext{app: a, sess: sess}

	// 老调用形态（不带 action）行为不变：整体替换
	res, err := runProgressWrite(tc, json.RawMessage(`{"progress":[{"content":"A","status":"completed"}]}`))
	if err != nil || !strings.Contains(res, "进度清单已更新（1 项，完成 1）") {
		t.Fatalf("老形态写入失败: %q err=%v", res, err)
	}
	// 空数组 = 清空
	res, err = runProgressWrite(tc, json.RawMessage(`{"progress":[]}`))
	if err != nil || !strings.Contains(res, "已清空") {
		t.Fatalf("空数组清空失败: %q err=%v", res, err)
	}
	if got := st.GetSessionData("s1", "progress"); got != "" {
		t.Fatalf("清空后库应为空，实际 %q", got)
	}
}

func TestProgressWriteRejectsBadCalls(t *testing.T) {
	st := testPlanStore(t)
	a := newPlanTestApp(t, st)
	sess := createPlanSession(t, st, "s1", "会话一", false)
	tc := &toolContext{app: a, sess: sess}
	if _, err := runProgressWrite(tc, json.RawMessage(
		`{"progress":[{"content":"别被抹掉","status":"pending"}]}`)); err != nil {
		t.Fatalf("预置清单失败: %v", err)
	}

	// 漏传 progress 不能当"清空"：必须报错，且库里清单原封不动
	if _, err := runProgressWrite(tc, json.RawMessage(`{}`)); err == nil {
		t.Fatal("缺 progress 应报错，不能静默清空")
	}
	if got := st.GetSessionData("s1", "progress"); !strings.Contains(got, "别被抹掉") {
		t.Fatalf("报错时不该动库，实际 %q", got)
	}
	// 非法 action 报错
	if _, err := runProgressWrite(tc, json.RawMessage(`{"action":"bogus"}`)); err == nil {
		t.Fatal("非法 action 应报错")
	}
	// 同时只能一项 in_progress（老语义）
	if _, err := runProgressWrite(tc, json.RawMessage(
		`{"progress":[{"content":"甲","status":"in_progress"},{"content":"乙","status":"in_progress"}]}`)); err == nil {
		t.Fatal("两项 in_progress 应报错")
	}

	// 最怕的误用：只想查清单，却顺手把清单带进来（甚至空数组）——list 必须只读
	for _, raw := range []string{
		`{"action":"list","progress":[]}`,
		`{"action":"list","progress":[{"content":"不该生效","status":"completed"}]}`,
		`{"action":"LIST","progress":[]}`,
	} {
		if _, err := runProgressWrite(tc, json.RawMessage(raw)); err != nil {
			t.Fatalf("%s 不该报错: %v", raw, err)
		}
		if got := st.GetSessionData("s1", "progress"); !strings.Contains(got, "别被抹掉") {
			t.Fatalf("%s 竟然改动了清单：%q", raw, got)
		}
	}
}

func TestElapsedCN(t *testing.T) {
	cases := map[int]string{
		0: "0s", 9: "9s", 88: "1m28s", 200: "3m20s", 3720: "1h02m",
	}
	for sec, want := range cases {
		if got := elapsedCN(sec); got != want {
			t.Fatalf("elapsedCN(%d)=%q，期望 %q", sec, got, want)
		}
	}
}
