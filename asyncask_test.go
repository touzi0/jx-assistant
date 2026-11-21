package main

// 后台询问（asyncask.go）的关键语义：
//   · 挂出去立刻返回（模型不等），条目落库、重启不丢；
//   · 用户处理 → 先落实真实效果（权限真提、计划真标记），再把结果以**合成
//     工具回执对**（当初那次调用的后续结果：assistant+tool 两行，见
//     landToolPair）投回会话（忙则插话、闲则起轮），不塞环境快照；
//   · 后台计划拒绝/取消**不改变会话的计划模式开关**（批准时只有在计划模式才退出）；
//   · 到期只回一条「超时未处理」，不叫醒会话；
//   · 撤销（模型或用户）之后，迟到的处理请求一律失败。

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// shortMerge 把合并窗口缩短到 5ms（测试里别真等 800ms）。
func shortMerge(t *testing.T) {
	t.Helper()
	old := asyncMergeWindow
	asyncMergeWindow = 5 * time.Millisecond
	t.Cleanup(func() { asyncMergeWindow = old })
}

func newAsyncTestApp(t *testing.T) (*App, *Store, *SessionMeta) {
	t.Helper()
	st := testPlanStore(t)
	sess := createPlanSession(t, st, "s1", "会话一", false)
	a := newPlanTestApp(t, st)
	a.Cfg = &Config{}               // 权限变更落地会重建环境快照（envPrompt 读 Cfg）
	a.runners["s1"] = &sessionRun{} // 会话正在跑：投递走插话，不真跑模型
	return a, st, sess
}

// 等插话队列里出现内容（后台投递是异步的：合并窗口后 flush）
func waitSteer(t *testing.T, a *App, n int) []*PendingMsg {
	t.Helper()
	for i := 0; i < 400; i++ {
		if q := a.pendingOf("s1"); len(q.Steer) >= n {
			return q.Steer
		}
		time.Sleep(5 * time.Millisecond)
	}
	q := a.pendingOf("s1")
	t.Fatalf("等不到投递（期望 %d 条，实际 %d 条）", n, len(q.Steer))
	return nil
}

func TestBackgroundAskDoesNotBlock(t *testing.T) {
	shortMerge(t)
	a, st, sess := newAsyncTestApp(t)
	tc := &toolContext{app: a, sess: sess}

	done := make(chan string, 1)
	go func() {
		out, err := runAskUserQuestion(tc, json.RawMessage(`{"question":"要不要导出 Markdown？","options":["导出","不导出"],"background":true}`))
		if err != nil {
			done <- "ERR:" + err.Error()
			return
		}
		done <- out
	}()
	select {
	case out := <-done:
		if !strings.Contains(out, "aq-") || !strings.Contains(out, "不要等") {
			t.Fatalf("后台提问应立即返回编号与「不要等」提示，实际：%s", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("后台提问把模型阻塞住了（应立即返回）")
	}

	// 落库：换一个 App（等同重启）照样读得到
	after := newPlanTestApp(t, st)
	asks := after.asyncPendingOf("s1")
	if len(asks) != 1 || asks[0].Kind != asyncKindAsk {
		t.Fatalf("重启后应有 1 条后台询问，实际 %+v", asks)
	}

	// 清单工具能看到它
	tc2 := &toolContext{app: after, sess: sess}
	list, err := runProgressWrite(tc2, json.RawMessage(`{"action":"list"}`))
	if err != nil {
		t.Fatalf("list 报错: %v", err)
	}
	if !strings.Contains(list, "后台等待项：1 条") || !strings.Contains(list, asks[0].ID) {
		t.Fatalf("清单里应列出这条后台询问，实际：%s", list)
	}
}

func TestResolveBackgroundAskDelivers(t *testing.T) {
	shortMerge(t)
	a, st, sess := newAsyncTestApp(t)
	tc := &toolContext{app: a, sess: sess}
	if _, err := runAskUserQuestion(tc, json.RawMessage(`{"question":"要不要导出？","background":true}`)); err != nil {
		t.Fatalf("挂后台失败: %v", err)
	}
	id := a.asyncPendingOf("s1")[0].ID

	if err := a.resolveQuestion(id, "不导出"); err != nil {
		t.Fatalf("处理应成功: %v", err)
	}
	steer := waitSteer(t, a, 1)
	if !strings.Contains(steer[0].Content, "【后台询问·已回答】") || !strings.Contains(steer[0].Content, "不导出") {
		t.Fatalf("投递内容不对：%s", steer[0].Content)
	}
	if steer[0].ToolName != "ask_user_question" || steer[0].Origin != originToolReport {
		t.Fatalf("后台回执应以合成工具回执对投递（ask_user_question/%s），实际 toolName=%q origin=%q",
			originToolReport, steer[0].ToolName, steer[0].Origin)
	}
	if len(a.asyncPendingOf("s1")) != 0 {
		t.Fatal("处理完不该还挂在待办里")
	}
	// 再答一次：应报「已处理过」
	if err := a.resolveQuestion(id, "再答一次"); err == nil || !strings.Contains(err.Error(), "已经处理过") {
		t.Fatalf("重复处理应报错，实际 %v", err)
	}
	_ = st
}

func TestBackgroundPermApprovesForReal(t *testing.T) {
	shortMerge(t)
	a, st, sess := newAsyncTestApp(t)
	tc := &toolContext{app: a, sess: sess}

	out, err := runRequestPermission(tc, json.RawMessage(`{"target":"full","justification":"要写工作区外的文件","background":true}`))
	if err != nil {
		t.Fatalf("后台申请权限失败: %v", err)
	}
	if !strings.Contains(out, "不要假设已提权") {
		t.Fatalf("返回串应提醒勿假设已提权，实际：%s", out)
	}
	id := a.asyncPendingOf("s1")[0].ID

	if err := a.resolveQuestion(id, "__approve__"); err != nil {
		t.Fatalf("批准应成功: %v", err)
	}
	fresh, _ := st.Get("s1")
	if got := permModeOf(fresh); got != permFull {
		t.Fatalf("批准后会话权限应真的提到完全访问（已落库），实际 %q", got)
	}
	steer := waitSteer(t, a, 1)
	if !strings.Contains(steer[0].Content, "【后台审批·已批准】") {
		t.Fatalf("投递内容不对：%s", steer[0].Content)
	}
}

func TestBackgroundPermDeniedKeepsMode(t *testing.T) {
	shortMerge(t)
	a, _, sess := newAsyncTestApp(t)
	before := permModeOf(sess)
	tc := &toolContext{app: a, sess: sess}
	if _, err := runRequestPermission(tc, json.RawMessage(`{"target":"full","justification":"理由","background":true}`)); err != nil {
		t.Fatalf("后台申请失败: %v", err)
	}
	id := a.asyncPendingOf("s1")[0].ID
	if err := a.resolveQuestion(id, "__deny__"); err != nil {
		t.Fatalf("拒绝应成功（后台路径不返回报错）: %v", err)
	}
	if permModeOf(sess) != before {
		t.Fatal("拒绝后权限不应变化")
	}
	steer := waitSteer(t, a, 1)
	if !strings.Contains(steer[0].Content, "已拒绝") {
		t.Fatalf("投递内容不对：%s", steer[0].Content)
	}
}

// 后台计划：不在计划模式也能提交；拒绝/取消都不改变计划模式开关
func TestBackgroundPlanDoesNotTouchPlanMode(t *testing.T) {
	shortMerge(t)
	a, st, sess := newAsyncTestApp(t)
	tc := &toolContext{app: a, sess: sess}
	out, err := runExitPlanMode(tc, json.RawMessage(`{"plan":"# 计划\n1. 改 A","background":true}`))
	if err != nil {
		t.Fatalf("不在计划模式也应能提交后台计划: %v", err)
	}
	if !strings.Contains(out, "不改会话模式") {
		t.Fatalf("返回串应说明不改会话模式，实际：%s", out)
	}
	id := a.asyncPendingOf("s1")[0].ID

	if err := a.resolveQuestion(id, "先别动 A，只补测试"); err != nil {
		t.Fatalf("给意见应成功: %v", err)
	}
	m, _ := st.Get("s1")
	if m.PlanMode {
		t.Fatal("拒绝后台计划不能把会话推进计划模式")
	}
	steer := waitSteer(t, a, 1)
	if !strings.Contains(steer[0].Content, "【后台计划·未通过】") || !strings.Contains(steer[0].Content, "只补测试") {
		t.Fatalf("投递内容不对：%s", steer[0].Content)
	}
}

// 在计划模式下批准后台计划：应退出计划模式（执行的前提）
func TestBackgroundPlanApproveExitsPlanMode(t *testing.T) {
	shortMerge(t)
	a, st, sess := newAsyncTestApp(t)
	sess.PlanMode = true
	_ = st.SaveMeta(sess)
	tc := &toolContext{app: a, sess: sess}
	if _, err := runExitPlanMode(tc, json.RawMessage(`{"plan":"# 计划\n做点什么","background":true}`)); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	id := a.asyncPendingOf("s1")[0].ID
	if err := a.resolveQuestion(id, "__approve__"); err != nil {
		t.Fatalf("批准失败: %v", err)
	}
	m, _ := st.Get("s1")
	if m.PlanMode {
		t.Fatal("批准后应退出计划模式")
	}
}

func TestCancelBackgroundAsk(t *testing.T) {
	shortMerge(t)
	a, _, sess := newAsyncTestApp(t)
	tc := &toolContext{app: a, sess: sess}
	if _, err := runAskUserQuestion(tc, json.RawMessage(`{"question":"问点什么","background":true}`)); err != nil {
		t.Fatalf("挂后台失败: %v", err)
	}
	id := a.asyncPendingOf("s1")[0].ID

	out, err := runProgressWrite(tc, json.RawMessage(`{"action":"cancel","id":"`+id+`"}`))
	if err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	if !strings.Contains(out, "已撤销") {
		t.Fatalf("撤销返回不对：%s", out)
	}
	if len(a.asyncPendingOf("s1")) != 0 {
		t.Fatal("撤销后不该还挂在待办里")
	}
	if err := a.resolveQuestion(id, "迟到的回答"); err == nil {
		t.Fatal("撤销后再处理应失败")
	}
}

func TestExpireBackgroundAskDoesNotWake(t *testing.T) {
	shortMerge(t)
	a, _, sess := newAsyncTestApp(t)
	tc := &toolContext{app: a, sess: sess}
	if _, err := runAskUserQuestion(tc, json.RawMessage(`{"question":"还问吗","background":true}`)); err != nil {
		t.Fatalf("挂后台失败: %v", err)
	}
	item := a.asyncPendingOf("s1")[0]

	a.expireAsyncAsk(item.ID)
	for i := 0; i < 200 && a.pendingOf("s1").Steer == nil; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	q := a.pendingOf("s1")
	// 到期提示不叫醒：会话"在跑"就插话，但绝不新起一轮（这里 runner 在，只看内容）
	if len(q.Steer) == 1 && !strings.Contains(q.Steer[0].Content, "超时未处理") {
		t.Fatalf("到期提示内容不对：%s", q.Steer[0].Content)
	}
	if len(a.asyncPendingOf("s1")) != 0 {
		t.Fatal("到期后不该还挂在待办里")
	}
}

// 后台提权要落到"本轮正持有的会话对象"上（工具执行读的是它），只改库里那份不够
func TestBackgroundPermUpgradesLiveSession(t *testing.T) {
	shortMerge(t)
	a, st, sess := newAsyncTestApp(t)
	live := *sess
	a.runners["s1"].sess = &live // 假装这一轮正在跑，持有它自己的副本
	tc := &toolContext{app: a, sess: sess}
	if _, err := runRequestPermission(tc, json.RawMessage(`{"target":"full","justification":"理由","background":true}`)); err != nil {
		t.Fatalf("后台申请失败: %v", err)
	}
	id := a.asyncPendingOf("s1")[0].ID
	if err := a.resolveQuestion(id, "__approve__"); err != nil {
		t.Fatalf("批准失败: %v", err)
	}
	if permModeOf(a.runners["s1"].sess) != permFull {
		t.Fatal("本轮持有的会话对象也必须提权（否则下一步仍按旧档走）")
	}
	if fresh, _ := st.Get("s1"); permModeOf(fresh) != permFull {
		t.Fatal("库里那份也要提权（重启后仍生效）")
	}
}

// 不在计划模式时，老的同步计划入口仍按原样报错（兼容性锚点）
func TestSyncPlanStillNeedsPlanMode(t *testing.T) {
	a, _, sess := newAsyncTestApp(t)
	tc := &toolContext{app: a, sess: sess}
	if _, err := runExitPlanMode(tc, json.RawMessage(`{"plan":"# 计划"}`)); err == nil {
		t.Fatal("同步提交计划在计划模式外应照旧报错")
	}
}

// 挂出即弹卡（2026-11-18 起）：asyncAdd 广播 question_required（与同步提问同一
// 张卡直接弹到输入框上方），kind 用存储原值、不带倒计时；广播无回放，重启后由
// pendingQuestions（/api/questions 兜底清单）把卡找回。
func TestAsyncAskPopsQuestionCard(t *testing.T) {
	shortMerge(t)
	a, st, sess := newAsyncTestApp(t)

	ch := make(chan Event, 16)
	a.evtMu.Lock()
	a.subscribers[ch] = true
	a.evtMu.Unlock()

	tc := &toolContext{app: a, sess: sess}
	if _, err := runAskUserQuestion(tc, json.RawMessage(`{"question":"要弹卡吗？","options":["要","不要"],"background":true}`)); err != nil {
		t.Fatalf("挂后台失败: %v", err)
	}

	gotReq := false
	deadline := time.After(2 * time.Second)
	for !gotReq {
		select {
		case ev := <-ch:
			if ev.Type != "question_required" || !strings.HasPrefix(ev.QuestionID, "aq-") {
				continue
			}
			gotReq = true
			if ev.Kind != asyncKindAsk || ev.SessionID != "s1" || ev.DeadlineMs != 0 {
				t.Fatalf("弹卡事件形态不对（应 kind=async-ask、无倒计时）: %+v", ev)
			}
			if !strings.Contains(ev.Summary, "要弹卡吗") || !containsStr(ev.Options, "要") {
				t.Fatalf("弹卡事件应带问题与选项: %+v", ev)
			}
		case <-deadline:
			t.Fatal("挂出后台询问应广播 question_required 弹卡")
		}
	}

	// 重启后（新 App）：/api/questions 兜底清单里能找回这张卡（Owner=async、无限时）
	after := newPlanTestApp(t, st)
	found := false
	for _, q := range after.pendingQuestions() {
		if strings.HasPrefix(q.ID, "aq-") {
			found = true
			if q.Owner != "async" || q.Kind != asyncKindAsk || !q.NoTimeout || q.DeadlineMs != 0 {
				t.Fatalf("后台提问卡应为 owner=async、无限时: %+v", q)
			}
		}
	}
	if !found {
		t.Fatal("pendingQuestions 应包含后台等待项（刷新/重连后把卡找回）")
	}
}
