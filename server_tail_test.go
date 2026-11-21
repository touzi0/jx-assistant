package main

// 分批加载（超长对话）切窗逻辑的验收测试：
//  1. 回合边界向前对齐：窗口起点落在回合中间时向前扩到回合起点，逐批加载
//     （before_seq 接力）并集 = 全量，无重复无遗漏（向右缩会让残段永远丢失）；
//  2. base 基线：切点前最后一条有效 prompt 的 sys/tools 进 older.base，空壳行
//     （已被 trim 掏成 {}）跳过；
//  3. inherit 标记行不与后续首批拆开；envBoundary 取摘要覆盖边界最大值；
//  4. isTurnStartMsg 与前端 lastTurnStart 同口径（aiuser/chat 起点，report 与
//     旧库【前缀通知不算）；
//  5. HTTP 层：不带参数 = 全量且无 older 字段（旧调用方零影响）；tail=N 带older。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func tailMsgs(n int) []*StoredMessage { // n 个回合：user + assistant
	out := []*StoredMessage{}
	for i := 1; i <= n; i++ {
		out = append(out,
			&StoredMessage{Message: TextMessage("user", fmt.Sprintf("问题 %d", i)), Seq: int64(i*2 - 1), Kind: "chat"},
			&StoredMessage{Message: TextMessage("assistant", fmt.Sprintf("回答 %d", i)), Seq: int64(i * 2), Kind: "chat"},
		)
	}
	return out
}

// ① 逐批接力加载的完整性：并集 = 全量、无重复、每批从回合起点开始。
func TestSliceMsgWindowBatchesCoverAll(t *testing.T) {
	msgs := tailMsgs(30) // 60 条，30 回合
	var got []*StoredMessage
	// 首批 tail=13（落在回合中间）
	win, info := sliceMsgWindow(msgs, len(msgs)-13, len(msgs))
	if len(win)%2 != 0 || win[0].Role != "user" {
		t.Fatalf("首批应从回合起点开始（向前对齐）: got %d 条, 首条 %+v", len(win), win[0])
	}
	got = append(got, win...)
	for info.Count > 0 { // before_seq 接力取上一批
		idx := 0
		for i, m := range msgs {
			if m.Seq >= win[0].Seq {
				idx = i
				break
			}
		}
		win, info = sliceMsgWindow(msgs, idx-13, idx)
		got = append(win, got...)
	}
	if len(got) != len(msgs) {
		t.Fatalf("逐批加载并集应覆盖全量: got %d want %d", len(got), len(msgs))
	}
	for i, m := range got {
		if m.Seq != int64(i+1) {
			t.Fatalf("并集应无重复无遗漏且有序: 位置 %d 期望 seq %d 实得 %d", i, i+1, m.Seq)
		}
	}
}

// ② base 基线：取切点前最后一条有效 prompt；空壳（{}）跳过；envBoundary 取最大覆盖边界。
func TestSliceMsgWindowBase(t *testing.T) {
	msgs := []*StoredMessage{
		{Message: TextMessage("user", "第一轮"), Seq: 1, Kind: "chat"},
		{Message: Message{Content: `{"sys":"提示词A","tools":[{"a":1}]}`}, Seq: 2, Kind: "prompt"},
		{Message: TextMessage("assistant", "答"), Seq: 3, Kind: "chat"},
		{Message: TextMessage("user", "第二轮"), Seq: 4, Kind: "chat"},
		{Message: Message{Content: "{}"}, Seq: 5, Kind: "prompt"}, // 与前一条相同，已被 trim 掏空
		{Message: TextMessage("assistant", "答2"), Seq: 6, Kind: "chat"},
		{Message: TextMessage("user", "第三轮"), Seq: 7, Kind: "chat"},
		{Message: Message{Content: `{"sys":"提示词B","tools":[{"b":2}]}`}, Seq: 8, Kind: "prompt"},
		{Message: TextMessage("assistant", "答3"), Seq: 9, Kind: "chat"},
		{Message: TextMessage("user", "第四轮"), Seq: 10, Kind: "chat"},
		{Message: Message{Content: "摘要"}, Seq: 11, Kind: "summary", CoversUpTo: 6},
	}
	win, info := sliceMsgWindow(msgs, 9, len(msgs)) // 切点在 seq=9 之前
	if info.Count != 9 {
		t.Fatalf("older.count 应为切点前条数: got %d", info.Count)
	}
	if info.Sys != "提示词B" || string(info.Tools) != `[{"b":2}]` {
		t.Fatalf("base 应取切点前最后一条有效 prompt: sys=%q tools=%s", info.Sys, info.Tools)
	}
	if info.EnvBoundary != 6 {
		t.Fatalf("envBoundary 应取摘要覆盖边界: got %d", info.EnvBoundary)
	}
	if win[0].Seq != 10 {
		t.Fatalf("窗口应从回合起点（user 第10条）开始: got seq %d", win[0].Seq)
	}
	// 切点前只有空壳 prompt：base 退到它之前的有效行
	win2, info2 := sliceMsgWindow(msgs, 6, len(msgs))
	if info2.Sys != "提示词A" {
		t.Fatalf("空壳行不作为基线，应退到前一条有效 prompt: got %q", info2.Sys)
	}
	if win2[0].Seq != 7 {
		t.Fatalf("窗口起点应为回合起点 seq=7: got %d", win2[0].Seq)
	}
}

// ③ inherit 标记行不拆开：切点停在 inherit 行之后时并入本批。
func TestSliceMsgWindowInherit(t *testing.T) {
	msgs := []*StoredMessage{
		{Message: TextMessage("user", `{"parent":"p","count":2}`), Seq: 1, Kind: "inherit"},
		{Message: TextMessage("user", "克隆1"), Seq: 2, Kind: "chat"},
		{Message: TextMessage("assistant", "克隆2"), Seq: 3, Kind: "chat"},
		{Message: TextMessage("user", "第一轮"), Seq: 4, Kind: "chat"},
		{Message: TextMessage("assistant", "答"), Seq: 5, Kind: "chat"},
		{Message: TextMessage("user", "第二轮"), Seq: 6, Kind: "chat"},
		{Message: TextMessage("assistant", "答2"), Seq: 7, Kind: "chat"},
	}
	// 加载到头：切点 = 1（inherit 行之后）
	win, info := sliceMsgWindow(msgs, 1, len(msgs))
	if win[0].Kind != "inherit" {
		t.Fatalf("切点贴着 inherit 行时应并入本批: 首条 kind=%s", win[0].Kind)
	}
	if info.Count != 0 {
		t.Fatalf("并入后 older.count 应为 0: got %d", info.Count)
	}
}

// ④ isTurnStartMsg 与前端 lastTurnStart 同口径。
func TestIsTurnStartMsg(t *testing.T) {
	cases := []struct {
		m    *StoredMessage
		want bool
	}{
		{&StoredMessage{Message: TextMessage("user", "普通发言"), Kind: "chat"}, true},
		{&StoredMessage{Message: TextMessage("user", "旧库无 kind 的普通发言")}, true},
		{&StoredMessage{Message: TextMessage("user", "AI 用户"), Kind: "aiuser"}, true},
		{&StoredMessage{Message: TextMessage("user", "【派发回报】xx"), Kind: "report"}, false},
		{&StoredMessage{Message: TextMessage("user", "【数字助理派发】xx")}, false}, // 旧库残留
		{&StoredMessage{Message: TextMessage("user", "<goal_round>x")}, false},
		{&StoredMessage{Message: TextMessage("assistant", "回答"), Kind: "chat"}, false},
		{&StoredMessage{Message: TextMessage("user", "环境快照"), Kind: "env"}, false},
	}
	for i, c := range cases {
		if got := isTurnStartMsg(c.m); got != c.want {
			t.Fatalf("用例 %d：isTurnStartMsg(%+v) = %v, want %v", i, c.m, got, c.want)
		}
	}
}

// ⑤ HTTP 层：缺省 = 全量无 older；tail=N 返回 older。
func TestHandleGetSessionTail(t *testing.T) {
	a, main := newDispatchTestApp(t)
	for i := 0; i < 20; i++ {
		if err := a.Store.AppendRow(main.ID, &StoredMessage{
			Message: TextMessage("user", fmt.Sprintf("第 %d 轮", i+1)), Kind: "chat",
		}); err != nil {
			t.Fatal(err)
		}
	}
	get := func(q string) (int, string, map[string]any) {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/api/sessions/"+main.ID+q, nil)
		rec := httptest.NewRecorder()
		a.apiMuxOf().ServeHTTP(rec, req)
		var body map[string]any
		if rec.Code == 200 {
			json.Unmarshal(rec.Body.Bytes(), &body)
		}
		return rec.Code, rec.Body.String(), body
	}
	code, raw, body := get("")
	if code != 200 {
		t.Fatalf("缺省应 200: %d %s", code, raw)
	}
	if _, has := body["older"]; has {
		t.Fatal("不带分页参数时不应有 older 字段（全量，旧调用方零影响）")
	}
	if len(body["messages"].([]any)) != 20 {
		t.Fatal("缺省应全量下发")
	}
	code, raw, body = get("?tail=7")
	if code != 200 {
		t.Fatalf("tail 应 200: %d %s", code, raw)
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 7 {
		t.Fatalf("tail=7 应下发 7 条: got %d", len(msgs))
	}
	first := msgs[0].(map[string]any)
	if first["content"] != "第 14 轮" {
		t.Fatalf("tail 窗口应取最近 7 条: 首条 %v", first["content"])
	}
	older := body["older"].(map[string]any)
	if older["count"].(float64) != 13 {
		t.Fatalf("older.count 应为 13: got %v", older["count"])
	}
}
