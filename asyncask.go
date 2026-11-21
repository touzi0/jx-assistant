package main

// 后台询问：把「提问 / 权限审批 / 计划审批」三类请求挂到后台，模型立刻返回、
// 继续干别的；用户处理之后，结果以一次**合成工具回执**自动回到会话里——复用
// run_python 后台回报那条路（startToolReportRound：模型正在吐正文就打断、
// 正在执行工具就下一步注入、一轮已结束就当续跑轮接上），形态是当初那次调用
// （ask_user_question / request_permission / exit_plan_mode）的后续结果：
// assistant(tool_calls)+tool 两行，与真实工具调用同路径同折叠（2026-11-18
// 起；此前落一条 aiuser 用户消息，旧会话行照旧渲染不迁移）。
//
// 三条边界：
//   - 归属会话、不归属"轮"：一轮跑完它继续挂着，重启软件也不丢（存会话附属数据）；
//   - 没有默认定时：模型不传超时 = 一直挂着，直到有人处理或模型自己撤销；
//     传了就到期产生一条"未答复"回执（只回执，不替模型做业务决定）；
//   - 结果只走回执投递，不塞环境快照。
//
// 界面形态（2026-11-18 起）：挂出即向全部前端广播 question_required（提问卡
// 直接弹在所属会话的输入框上方，可折叠成一行；其他对话开着则进角标堆），
// 刷新/重连后由 /api/questions（pendingQuestions 纳入后台等待项）把卡找回；
// 处理/超时/撤销沿用 question_resolved 收卡。此前只躺在进度面板等「查看」。
//
// 与同步通道的关系：一问一答、同步等结果的老通道（requestUserAnswer /
// requestApproval / exit_plan_mode 不带 background）一行未改。

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

const (
	asyncKindAsk  = "async-ask"  // 用户询问
	asyncKindPerm = "async-perm" // 权限审批
	asyncKindPlan = "async-plan" // 计划审批

	asyncPending   = "pending"   // 等用户处理
	asyncAnswered  = "answered"  // 已回答
	asyncApproved  = "approved"  // 已批准
	asyncDenied    = "denied"    // 已拒绝 / 未通过
	asyncCancelled = "cancelled" // 已撤销（模型撤销或用户取消）
	asyncExpired   = "expired"   // 到期未答复

	// asyncDataKey 是会话附属数据里的键（与 progress / pendingplan 同一层）
	asyncDataKey = "asyncasks"

	asyncMaxPending = 8 // 单会话同时在等的上限（防模型挂一堆没人管）
)

// asyncMergeWindow 是同一会话多条结果合并成一条消息的窗口（测试里可缩短）。
var asyncMergeWindow = 800 * time.Millisecond

// asyncAsk 是一条挂在后台等人的请求。
type asyncAsk struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	Kind      string `json:"kind"`

	Question string   `json:"question,omitempty"` // 询问 / 给用户看的一句话
	Options  []string `json:"options,omitempty"`
	Plan     string   `json:"plan,omitempty"`   // 计划正文
	Target   string   `json:"target,omitempty"` // 权限申请的目标档位

	CreatedAt string `json:"createdAt"`
	// DeadlineMs/TimeoutSec：模型可选设置，0 = 不限时（默认）
	DeadlineMs int64 `json:"deadlineMs,omitempty"`
	TimeoutSec int   `json:"timeoutSec,omitempty"`

	Status     string `json:"status"`
	Answer     string `json:"answer,omitempty"`
	AnsweredAt string `json:"answeredAt,omitempty"`
}

// asyncAskView 是给前端面板 / 进度工具看的形态（带已等秒数）。
type asyncAskView struct {
	asyncAsk
	WaitingSec int `json:"waitingSec"`
}

var (
	asyncMu     sync.Mutex
	asyncOut    = map[string]*asyncOutbox{} // 会话 → 待投递的结果缓冲
	asyncTimers = map[string]*time.Timer{}  // 请求 id → 到期定时器（仅内存）
)

// asyncOutbox 把一个会话在合并窗口内的多条回执攒成一批（一次 flush 逐条投递，
// 避免逐条各自拉起投递逻辑）。
type asyncOutbox struct {
	items []*PendingMsg
	wake  bool // 至少有一条要叫醒会话（用户主动处理过）
	timer *time.Timer
}

func (a *App) asyncLoad(sessionID string) []asyncAsk {
	raw := a.Store.GetSessionData(sessionID, asyncDataKey)
	if raw == "" {
		return nil
	}
	out := []asyncAsk{}
	if json.Unmarshal([]byte(raw), &out) != nil {
		return nil
	}
	return out
}

func (a *App) asyncSave(sessionID string, list []asyncAsk) {
	if len(list) == 0 {
		_ = a.Store.SetSessionData(sessionID, asyncDataKey, "")
		return
	}
	data, err := json.Marshal(list)
	if err != nil {
		log.Printf("[后台询问] 会话 %s 序列化失败: %v", sessionID, err)
		return
	}
	if err := a.Store.SetSessionData(sessionID, asyncDataKey, string(data)); err != nil {
		log.Printf("[后台询问] 会话 %s 落库失败: %v", sessionID, err)
	}
}

// asyncAskQuestionOf 把一条挂起的后台请求转成提问卡形态（question_required
// 事件与 /api/questions 清单共用）：kind 用存储原值（async-ask/async-perm/
// async-plan），前端按它分卡渲染；Owner=async 标记来源。后台项没有「超时视为
// 放行」的语义（到期只回一条「超时未处理」，不替模型做决定），所以一律不带
// 倒计时——用户慢慢看，答完以工具回执送回模型。
func asyncAskQuestionOf(it asyncAsk, sessionTitle string) *UserQuestion {
	return &UserQuestion{
		ID: it.ID, SessionID: it.SessionID, SessionTitle: sessionTitle,
		Kind: it.Kind, Question: it.Question, Plan: it.Plan, Options: it.Options,
		AllowFree: true, NoTimeout: true, CreatedAt: it.CreatedAt, Owner: "async",
	}
}

// asyncAdd 登记一条后台请求并（可选）排上到期定时器。
func (a *App) asyncAdd(item asyncAsk) (*asyncAsk, error) {
	item.ID = "aq-" + newID()
	item.CreatedAt = now()
	item.Status = asyncPending
	if item.TimeoutSec > 0 {
		item.DeadlineMs = time.Now().Add(time.Duration(item.TimeoutSec) * time.Second).UnixMilli()
	}
	asyncMu.Lock()
	list := a.asyncLoad(item.SessionID)
	pending := 0
	for _, it := range list {
		if it.Status == asyncPending {
			pending++
		}
	}
	if pending >= asyncMaxPending {
		asyncMu.Unlock()
		return nil, fmt.Errorf("本会话已有 %d 条后台请求在等用户处理（上限 %d）：先处理或撤销几条，或这一次直接同步询问", pending, asyncMaxPending)
	}
	list = append(list, item)
	a.asyncSave(item.SessionID, list)
	if item.TimeoutSec > 0 {
		id := item.ID
		asyncTimers[id] = time.AfterFunc(time.Duration(item.TimeoutSec)*time.Second, func() {
			a.expireAsyncAsk(id)
		})
	}
	asyncMu.Unlock()
	a.broadcast(Event{Type: "async_ask_changed", SessionID: item.SessionID})
	// 挂出即弹卡（2026-11-18 起）：后台请求不再只躺在进度面板等人点「查看」——
	// 与同步提问同一张卡直接出现在所属会话的输入框上方（可折叠成一行不挡对话），
	// 别的对话正开着时收进右下角角标堆。回答走同一个 POST /api/questions/{id}，
	// 处理/超时/撤销的 question_resolved 广播照常收卡。
	title := ""
	if m, err := a.Store.Get(item.SessionID); err == nil {
		title = m.Title
	}
	uq := asyncAskQuestionOf(item, title)
	a.broadcast(Event{Type: "question_required", QuestionID: uq.ID, SessionID: uq.SessionID,
		Title: uq.SessionTitle, Kind: uq.Kind, Summary: uq.Question, Plan: uq.Plan, Options: uq.Options})
	return &item, nil
}

// asyncPendingOf 返回会话里还在等用户处理的条目（进度面板 / 清单工具用）。
func (a *App) asyncPendingOf(sessionID string) []asyncAskView {
	out := []asyncAskView{}
	for _, it := range a.asyncLoad(sessionID) {
		if it.Status != asyncPending {
			continue
		}
		wait := int(time.Since(asyncCreatedTime(it)).Seconds())
		if wait < 0 {
			wait = 0
		}
		out = append(out, asyncAskView{asyncAsk: it, WaitingSec: wait})
	}
	// 按挂出时间排序（同一时刻挂出的按 id，输出确定性）
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].CreatedAt < out[j-1].CreatedAt; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// asyncCreatedTime 把 CreatedAt 还原成时间（解析失败就当"刚刚"，只影响展示）。
func asyncCreatedTime(it asyncAsk) time.Time {
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", it.CreatedAt, time.Local); err == nil {
		return t
	}
	return time.Now()
}

// asyncFind 按 id 找到条目与它的会话（逐个会话扫，点击频次极低，够用）。
func (a *App) asyncFind(id string) (asyncAsk, *SessionMeta, error) {
	sessions, err := a.Store.List()
	if err != nil {
		return asyncAsk{}, nil, err
	}
	for _, sess := range sessions {
		for _, it := range a.asyncLoad(sess.ID) {
			if it.ID == id {
				m, gerr := a.Store.Get(sess.ID)
				if gerr != nil {
					return asyncAsk{}, nil, fmt.Errorf("这条后台请求所属的会话已不存在")
				}
				return it, m, nil
			}
		}
	}
	return asyncAsk{}, nil, fmt.Errorf("后台请求 %s 不存在（或会话已删除）", id)
}

// resolveAsyncAsk 收口一条后台请求：先落实真实效果（权限真的提、计划真的标记），
// 再把结果投回会话。
func (a *App) resolveAsyncAsk(id, answer string) error {
	item, sess, err := a.asyncFind(id)
	if err != nil {
		return err
	}
	if item.Status != asyncPending {
		return fmt.Errorf("这条后台请求已经处理过了（%s）", asyncStatusLabel(item.Status))
	}
	answer = strings.TrimSpace(answer)
	nowStr := now()
	text := ""
	// 活跃对象优先：本轮正在跑时，提权 / 计划模式判定读的都是它；只改库里那份
	// （asyncFind 取回的新副本），当前这一轮下一步仍按旧状态走。
	base := sess
	if live := a.liveSession(sess.ID); live != nil {
		base = live
	}

	switch item.Kind {
	case asyncKindPerm:
		if answer == "__approve__" {
			target := canonPermTarget(item.Target)
			wasMode, wasTools := permModeOf(base), base.Tools
			if sandboxPolicyRank(target) > sandboxPolicyRank(wasMode) {
				a.switchSessionPerm(base, target)
				a.landPermChange(base, permSrcApproval, wasMode, wasTools, nil)
			}
			item.Status = asyncApproved
			item.Answer = "允许"
			text = fmt.Sprintf("【后台审批·已批准】%s\n申请：把会话权限提到「%s」\n结果：用户已批准，权限立即生效（当前会话权限：%s）。后续动作按新档执行，不必再逐次申请；但不要扩大用途。",
				item.ID, permModeLabel(target), permModeLabel(permModeOf(base)))
		} else {
			item.Status = asyncDenied
			item.Answer = "拒绝"
			text = fmt.Sprintf("【后台审批·已拒绝】%s\n申请：把会话权限提到「%s」\n结果：用户拒绝了，会话保持「%s」。不要重试、不要绕过；换一条不需要该权限的路，或向用户说明这件事做不了。",
				item.ID, permModeLabel(canonPermTarget(item.Target)), permModeLabel(permModeOf(base)))
		}

	case asyncKindPlan:
		switch answer {
		case "__approve__":
			item.Status = asyncApproved
			item.Answer = "通过"
			modeNote := "本会话不在计划模式，模式保持不变"
			if base.PlanMode {
				base.PlanMode = false
				_ = a.Store.SaveMeta(base)
				modeNote = "本会话已退出计划模式"
				a.broadcast(Event{Type: "session_updated", SessionID: sess.ID})
			}
			text = fmt.Sprintf("【后台计划·已通过】%s\n计划：%s\n结果：用户批准了这份计划（%s）。按它继续执行；执行中写文件与执行命令仍受当前权限模式约束。",
				item.ID, planFirstLine(item.Plan), modeNote)
		case "__ignore__":
			item.Status = asyncCancelled
			item.Answer = "用户取消"
			text = fmt.Sprintf("【后台计划·已取消】%s\n计划：%s\n结果：用户取消了这份计划，不必再按它执行。计划本身没有被否定——想推进就另说。",
				item.ID, planFirstLine(item.Plan))
		default:
			item.Status = asyncDenied
			item.Answer = answer
			if answer == "" {
				item.Answer = "未通过"
			}
			text = fmt.Sprintf("【后台计划·未通过】%s\n计划：%s\n结果：用户没有批准。用户意见：%s\n请据此修订（保留仍有效的部分），改完再提交。",
				item.ID, planFirstLine(item.Plan), item.Answer)
		}

	default: // asyncKindAsk
		item.Status = asyncAnswered
		item.Answer = answer
		if answer == "" {
			item.Answer = "（用户跳过了这个问题）"
		}
		text = fmt.Sprintf("【后台询问·已回答】%s\n问：%s\n答（用户 %s）：%s\n（这是你先前挂出的后台询问，现在有答案了；它不是新的用户指令。）",
			item.ID, truncateRunes(item.Question, 200), time.Now().Format("15:04"), truncateRunes(item.Answer, 500))
	}

	item.AnsweredAt = nowStr
	asyncMu.Lock()
	list := a.asyncLoad(sess.ID)
	for i := range list {
		if list[i].ID == item.ID {
			list[i] = item
		}
	}
	a.asyncSave(sess.ID, list)
	if t := asyncTimers[item.ID]; t != nil {
		t.Stop()
		delete(asyncTimers, item.ID)
	}
	asyncMu.Unlock()

	a.broadcast(Event{Type: "question_resolved", QuestionID: item.ID, SessionID: sess.ID})
	a.broadcast(Event{Type: "async_ask_changed", SessionID: sess.ID})
	a.asyncQueue(sess, asyncReceiptPair(item, text), true) // 用户处理过：允许叫醒会话
	log.Printf("[后台询问] %s（%s）已处理：%s", item.ID, asyncKindLabel(item.Kind), asyncStatusLabel(item.Status))
	return nil
}

// cancelAsyncAsk 撤销一条后台请求（模型主动"不要这个答案了"）。
// byUser=false 表示模型撤销；用户那边的取消/拒绝走 resolveAsyncAsk。
func (a *App) cancelAsyncAsk(sessionID, id string) error {
	asyncMu.Lock()
	list := a.asyncLoad(sessionID)
	found := -1
	for i := range list {
		if list[i].ID == id {
			found = i
			break
		}
	}
	if found < 0 {
		asyncMu.Unlock()
		return fmt.Errorf("后台请求 %s 不存在（可能已处理；用 action=list 看当前还有哪些）", id)
	}
	if list[found].Status != asyncPending {
		st := asyncStatusLabel(list[found].Status)
		asyncMu.Unlock()
		return fmt.Errorf("后台请求 %s 已经是「%s」，无需再撤销", id, st)
	}
	list[found].Status = asyncCancelled
	list[found].AnsweredAt = now()
	a.asyncSave(sessionID, list)
	if t := asyncTimers[id]; t != nil {
		t.Stop()
		delete(asyncTimers, id)
	}
	asyncMu.Unlock()
	a.broadcast(Event{Type: "question_resolved", QuestionID: id, SessionID: sessionID})
	a.broadcast(Event{Type: "async_ask_changed", SessionID: sessionID})
	return nil
}

// expireAsyncAsk 到期处理：只产生一条"未答复"回执（不替模型做业务决定），
// 且**不叫醒会话**（模型自己设的定时，它要的是下次跑起来时看到进度）。
func (a *App) expireAsyncAsk(id string) {
	item, sess, err := a.asyncFind(id)
	if err != nil || item.Status != asyncPending {
		return
	}
	asyncMu.Lock()
	list := a.asyncLoad(sess.ID)
	for i := range list {
		if list[i].ID == id {
			list[i].Status = asyncExpired
			list[i].AnsweredAt = now()
		}
	}
	a.asyncSave(sess.ID, list)
	delete(asyncTimers, id)
	asyncMu.Unlock()
	a.broadcast(Event{Type: "question_resolved", QuestionID: id, SessionID: sess.ID})
	a.broadcast(Event{Type: "async_ask_changed", SessionID: sess.ID})
	text := fmt.Sprintf("【后台%s·超时未处理】%s\n内容：%s\n结果：到点仍没有人处理。按你当初的判断继续（该跳过就跳过、该换路就换路），不要一直等。",
		asyncKindLabel(item.Kind), item.ID, truncateRunes(asyncSummary(item), 200))
	a.asyncQueue(sess, asyncReceiptPair(item, text), false) // 不叫醒
}

// asyncReceiptPair 把一条后台请求的回执文本组装成合成工具回执对：函数名回显
// 当初挂后台的原工具，参数回显识别信息——模型看到的是「那次调用的后续结果
// 到了」，前端按普通工具行折叠（详见 runtime.go landToolPair）。
func asyncReceiptPair(item asyncAsk, text string) *PendingMsg {
	switch item.Kind {
	case asyncKindPerm:
		args, _ := json.Marshal(map[string]any{"target": canonPermTarget(item.Target), "background": true})
		return toolReportPair("request_permission", string(args), text)
	case asyncKindPlan:
		// args 回显当初调用的完整识别参数（含计划正文）：模型据此认出是哪份
		// 计划的回执；前端 exit_plan_mode 回执升格的计划卡也从这里拿计划全文
		args, _ := json.Marshal(map[string]any{"plan": item.Plan, "background": true})
		return toolReportPair("exit_plan_mode", string(args), text)
	default:
		args, _ := json.Marshal(map[string]any{"question": truncateRunes(item.Question, 200), "background": true})
		return toolReportPair("ask_user_question", string(args), text)
	}
}

// asyncQueue 把一条回执对排进会话的合并窗口（同一会话 800ms 内的多条一次
// flush 逐条投递）。
func (a *App) asyncQueue(sess *SessionMeta, pm *PendingMsg, wake bool) {
	if pm == nil || strings.TrimSpace(pm.Content) == "" {
		return
	}
	asyncMu.Lock()
	ob := asyncOut[sess.ID]
	if ob == nil {
		ob = &asyncOutbox{}
		asyncOut[sess.ID] = ob
	}
	ob.items = append(ob.items, pm)
	ob.wake = ob.wake || wake
	if ob.timer == nil {
		id := sess.ID
		ob.timer = time.AfterFunc(asyncMergeWindow, func() { a.flushAsyncOut(id) })
	}
	asyncMu.Unlock()
}

// flushAsyncOut 把攒下的回执对逐条投回会话：用户主动处理过 → 走
// startToolReportRound（第一条起轮、其余插话进同一轮，忙则全部插话）；
// 只是到期提示 → 走 NoKick（忙则插话、闲则只落对，不叫醒）。
func (a *App) flushAsyncOut(sessionID string) {
	asyncMu.Lock()
	ob := asyncOut[sessionID]
	if ob == nil {
		asyncMu.Unlock()
		return
	}
	items := ob.items
	wake := ob.wake
	delete(asyncOut, sessionID)
	asyncMu.Unlock()

	sess, err := a.Store.Get(sessionID)
	if err != nil {
		log.Printf("[后台询问] 会话 %s 已不存在，结果丢弃", sessionID)
		return
	}
	for _, pm := range items {
		if wake {
			a.startToolReportRound(sess, pm)
			continue
		}
		a.deliverToolReportNoKick(sess, pm)
	}
}

// ---------- 展示与文案 ----------

func asyncKindLabel(kind string) string {
	switch kind {
	case asyncKindPerm:
		return "审批"
	case asyncKindPlan:
		return "计划"
	}
	return "询问"
}

func asyncStatusLabel(status string) string {
	switch status {
	case asyncPending:
		return "待处理"
	case asyncAnswered:
		return "已回答"
	case asyncApproved:
		return "已批准"
	case asyncDenied:
		return "已拒绝"
	case asyncCancelled:
		return "已取消"
	case asyncExpired:
		return "已超时"
	}
	return status
}

// asyncSummary 是给清单/回执用的一句话摘要。
func asyncSummary(it asyncAsk) string {
	switch it.Kind {
	case asyncKindPerm:
		return "把会话权限提到「" + permModeLabel(canonPermTarget(it.Target)) + "」"
	case asyncKindPlan:
		return planFirstLine(it.Plan)
	}
	return it.Question
}

// planFirstLine 取计划的第一行有效内容（去掉 Markdown 标题符号），空则给占位。
func planFirstLine(plan string) string {
	for _, line := range strings.Split(plan, "\n") {
		s := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
		if s != "" {
			return truncateRunes(s, 80)
		}
	}
	return "（空计划）"
}

// canonPermTarget 归一化权限档位的别名。
func canonPermTarget(t string) string {
	switch strings.TrimSpace(t) {
	case permReadOnly:
		return permReadOnly
	case "danger-full-access", permFull:
		return permFull
	}
	return permWorkspace
}

// asyncWaitNote 是后台请求返回串里那句"到时怎么办"的说明（没有定时也写清楚）。
func asyncWaitNote(sec int) string {
	if sec <= 0 {
		return "没有设置超时，这条会一直挂着等你。"
	}
	return fmt.Sprintf("超过 %d 秒仍没人处理，你会收到一条「超时未处理」提示（不会替你做决定）。", sec)
}

// clampBackgroundTimeoutSec 钳制后台请求的可选超时（0 = 不限时，由调用方处理）。
func clampBackgroundTimeoutSec(sec int) int {
	if sec < 30 {
		return 30
	}
	if sec > 3600 {
		return 3600
	}
	return sec
}
