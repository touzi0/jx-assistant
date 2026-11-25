package main

// 分批加载（超长对话）切窗逻辑的验收测试：
//  1. 回合边界向前对齐：窗口起点落在回合中间时向前扩到回合起点，逐批加载
//     （before_seq 接力）并集 = 全量，无重复无遗漏（向右缩会让残段永远丢失）；
//  2. 快照行瘦身只按引擎记录的 chg 决定（TestSlimRowsForUI）：无变化掏空、变化
//     全文、窗口内首条保留全文、老行逐字比较补判；切窗只报切点前条数；
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
	"strings"
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

// ② 快照行瘦身按引擎记录的 chg 决定（这里不比对）：无变化 → 空壳；有变化 → 全文
// 带回 chg；窗口内第一条完整快照不论变没变都保留全文（前端据此交代首见）；
// 老行（无 chg）回退到与上一条逐字比较。
func TestSlimRowsForUI(t *testing.T) {
	row := func(seq int64, sys, chg string, tool string) *StoredMessage {
		if chg != "" {
			chg = `,"chg":` + chg
		}
		return &StoredMessage{Message: Message{Content: `{"sys":"` + sys + `","tools":[{"type":"function","function":{"name":"` + tool + `"}}]` + chg + `}`}, Seq: seq, Kind: "prompt"}
	}
	msgs := []*StoredMessage{
		{Message: TextMessage("user", "第一轮"), Seq: 1, Kind: "chat"},
		row(2, "A", `{"first":true}`, "a"),
		{Message: TextMessage("assistant", "答"), Seq: 3, Kind: "chat"},
		{Message: TextMessage("user", "第二轮"), Seq: 4, Kind: "chat"},
		row(5, "A", `{}`, "a"), // 无变化
		{Message: TextMessage("assistant", "答2"), Seq: 6, Kind: "chat"},
		{Message: TextMessage("user", "第三轮"), Seq: 7, Kind: "chat"},
		row(8, "B", `{"sys":true,"prevSys":"A","toolsDel":["a"],"toolsAdd":["b"]}`, "b"), // 有变化
		{Message: TextMessage("assistant", "答3"), Seq: 9, Kind: "chat"},
		{Message: TextMessage("user", "第四轮"), Seq: 10, Kind: "chat"},
		row(11, "B", `{}`, "b"), // 无变化
		{Message: TextMessage("assistant", "答4"), Seq: 12, Kind: "chat"},
		{Message: Message{Content: "摘要"}, Seq: 13, Kind: "summary", CoversUpTo: 6},
	}
	content := func(seq int64) any {
		for _, m := range msgs {
			if m.Seq == seq {
				return m.Content
			}
		}
		return nil
	}
	slimRowsForUI(msgs, 0, len(msgs)) // 全量：首条全文、无变化掏空、变化全文
	if content(2) == "{}" || content(5) != "{}" || content(8) == "{}" || content(11) != "{}" {
		t.Fatalf("全量瘦身形态不对: 2=%q 5=%q 8=%q 11=%q", content(2), content(5), content(8), content(11))
	}
	// 窗口从第三轮开始：窗口首条（seq 8，有变化）保留全文；之后无变化掏空
	msgs[4].Content = `{"sys":"A","tools":[{"type":"function","function":{"name":"a"}}],"chg":{}}`
	msgs[10].Content = `{"sys":"B","tools":[{"type":"function","function":{"name":"b"}}],"chg":{}}`
	slimRowsForUI(msgs, 6, len(msgs))
	if content(8) == "{}" || content(11) != "{}" {
		t.Fatalf("窗口首条应保留全文、其后无变化掏空: 8=%q 11=%q", content(8), content(11))
	}
	// 窗口首条是无变化行（seq 5）：同样保留全文（前端据此画首见）
	msgs[10].Content = `{"sys":"B","tools":[{"type":"function","function":{"name":"b"}}],"chg":{}}`
	slimRowsForUI(msgs, 3, 8)
	if content(5) == "{}" || content(8) == "{}" {
		t.Fatalf("窗口首条无变化也应保留全文: 5=%q 8=%q", content(5), content(8))
	}
	// 老行（无 chg）：与上一条逐字比较补判；窗口之前的行只作基线、不进响应
	legacy := []*StoredMessage{
		row(1, "A", "", "a"),
		row(2, "A", "", "a"),
		row(3, "B", "", "a"),
		row(4, "B", "", "b"),
	}
	slimRowsForUI(legacy, 0, len(legacy))
	if legacy[0].Content == "{}" || legacy[1].Content != "{}" || legacy[2].Content == "{}" || legacy[3].Content == "{}" {
		t.Fatal("老行应按逐字比较瘦身")
	}
	// 老行补判的结果补记进响应副本：前端拿到的形态与新行一致
	if c := legacy[2].Content.(string); !strings.Contains(c, `"sys":true`) || !strings.Contains(c, `"prevSys":"A"`) {
		t.Fatalf("老行 sys 变化应补记 chg.sys/prevSys: %s", c)
	}
	if c := legacy[3].Content.(string); !strings.Contains(c, `"toolsAdd":["b"]`) || !strings.Contains(c, `"toolsDel":["a"]`) {
		t.Fatalf("老行工具增删应补记 chg.toolsAdd/toolsDel: %s", c)
	}
	if c := legacy[0].Content.(string); !strings.Contains(c, `"first":true`) {
		t.Fatalf("老行首条应补记 chg.first: %s", c)
	}
	legacy = []*StoredMessage{
		row(1, "A", "", "a"),
		row(2, "A", "", "a"),
		row(3, "A", "", "a"),
	}
	slimRowsForUI(legacy, 1, len(legacy)) // 窗口 [1,3)：首条（与窗口前相同）保留全文，再后一条掏空
	if legacy[1].Content == "{}" || legacy[2].Content != "{}" {
		t.Fatalf("老行窗口首条保留全文: 2=%q 3=%q", legacy[1].Content, legacy[2].Content)
	}
}

// ②-b 切窗只报切点前的条数（基线不再由服务端接力：快照形态由 chg 决定）。
func TestSliceMsgWindowCount(t *testing.T) {
	msgs := tailMsgs(10)
	win, info := sliceMsgWindow(msgs, 9, len(msgs))
	if info.Count != 8 { // 向前对齐到 seq=9 的回合起点（索引 8）
		t.Fatalf("older.count 应为对齐后的切点: got %d", info.Count)
	}
	if win[0].Seq != 9 {
		t.Fatalf("窗口应从回合起点开始: got seq %d", win[0].Seq)
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
	if _, has := older["sys"]; has {
		t.Fatal("older 不再携带 sys/tools 基线（快照形态由引擎记录的 chg 决定）")
	}
}

// ⑥ HTTP 层：env 行附 render（与 buildContext 同一函数算出：首条整份、后续差分），
// prompt 行按 chg 瘦身且全文行带回 chg。
func TestHandleGetSessionEnvRender(t *testing.T) {
	a, main := newDispatchTestApp(t)
	app := func(m *StoredMessage) {
		if err := a.Store.AppendRow(main.ID, m); err != nil {
			t.Fatal(err)
		}
	}
	app(&StoredMessage{Message: TextMessage("user", "第一轮"), Kind: "chat"})
	app(&StoredMessage{Message: Message{Content: `{"sys":"A","tools":[],"chg":{"first":true}}`}, Kind: "prompt"})
	app(&StoredMessage{Message: TextMessage("system", "# 运行环境快照\n- 时间：10:00"), Kind: "env"})
	app(&StoredMessage{Message: TextMessage("user", "第二轮"), Kind: "chat"})
	app(&StoredMessage{Message: Message{Content: `{"sys":"A","tools":[],"chg":{}}`}, Kind: "prompt"})
	app(&StoredMessage{Message: TextMessage("system", "# 运行环境快照\n- 时间：10:00\n- 权限：只读"), Kind: "env"})
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/api/sessions/"+main.ID, nil)
	rec := httptest.NewRecorder()
	a.apiMuxOf().ServeHTTP(rec, req)
	var body struct {
		Messages []struct {
			Kind    string `json:"kind"`
			Content string `json:"content"`
			Render  *struct {
				Mode string `json:"mode"`
				Text string `json:"text"`
			} `json:"render"`
		} `json:"messages"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
		t.Fatalf("应 200 且可解析: %d %s", rec.Code, rec.Body.String())
	}
	var prompts, envs int
	for _, m := range body.Messages {
		switch m.Kind {
		case "prompt":
			prompts++
			if prompts == 1 && (m.Content == "{}" || !strings.Contains(m.Content, `"first":true`)) {
				t.Fatalf("首条快照应全文且带 chg.first: %s", m.Content)
			}
			if prompts == 2 && m.Content != "{}" {
				t.Fatalf("无变化快照应掏成空壳: %s", m.Content)
			}
		case "env":
			envs++
			if m.Render == nil {
				t.Fatalf("env 行应附 render: %s", m.Content)
			}
			if envs == 1 && (m.Render.Mode != "full" || m.Render.Text != m.Content) {
				t.Fatalf("首条 env 应整份渲染: %+v", m.Render)
			}
			if envs == 2 && (m.Render.Mode != "diff" || !strings.Contains(m.Render.Text, "- 权限：只读")) {
				t.Fatalf("第二条 env 应差分渲染: %+v", m.Render)
			}
		}
	}
	if prompts != 2 || envs != 2 {
		t.Fatalf("应下发 2 条 prompt 与 2 条 env: got %d/%d", prompts, envs)
	}
}
