package main

// 待生效队列快照版本号（PendingState.Ver）的验收测试：插话虚线框幽灵残留
// 的根因之一是「取值早、投递晚的旧快照回放」——interject 响应/pending 广播
// 在途时排水可能已完成，旧快照晚到把已落库的插话重新画成「插话中」虚线框。
// 版本号是快照的取值时刻标记：每次队列变更（入队/排水/出队/移动/暂停/清空）
// 都必须在锁内递增，前端只接受比已应用更新的快照。这里验收：
//  1. 每个变更点都前进 ver（漏 bump = 旧快照可回放，等于白做）；
//  2. ver 与快照内容在锁内原子对应（并发下不会出现「ver 新、内容旧」）；
//  3. 并发插话+排水：排空后取到的任何快照 ver 都大于排空前取到的快照 ver；
//  4. ver 是毫秒时间戳基底（进程重启不回卷：挂在旧基线上的页面不会被
//     重启后的新快照永久拦截）。

import (
	"sync"
	"testing"
	"time"
)

func newTestAppForVer() *App {
	// 与 newPendingTestApp 同款：不走 initRuntime（那要 Cfg），只建内存表
	a := &App{}
	a.pendingSteer = map[string][]*PendingMsg{}
	a.pendingQueue = map[string][]*PendingMsg{}
	a.queuePaused = map[string]bool{}
	a.pendVer = map[string]int64{}
	a.subscribers = map[chan Event]bool{}
	return a
}

// TestPendingVerBumpsOnEveryMutation 每个变更点都前进 ver（时间戳基底、严格单调）。
func TestPendingVerBumpsOnEveryMutation(t *testing.T) {
	a := newTestAppForVer()
	const sid = "s1"
	prev := int64(0)
	step := func(what string) int64 {
		t.Helper()
		got := a.pendingOf(sid).Ver
		if got <= prev {
			t.Fatalf("%s 后 ver=%d，未前进（前值 %d）", what, got, prev)
		}
		prev = got
		return got
	}
	if v := a.pendingOf(sid).Ver; v != 0 {
		t.Fatalf("初始 ver=%d，want 0", v)
	}
	pm := a.pushPending(sid, "steer", "插话", kindChat, "interject")
	step("插话入队")
	a.pushPending(sid, "queue", "排队", kindChat, "")
	step("排队入队")
	got := a.drainSteer(sid)
	if len(got) != 1 || got[0].ID != pm.ID {
		t.Fatalf("drainSteer 取走内容不对: %+v", got)
	}
	step("插话排水")
	if a.popQueue(sid) == nil {
		t.Fatal("popQueue 应取出排队项")
	}
	step("排队出队")
	a.pushPending(sid, "queue", "再排一条", kindChat, "")
	last := a.pushPending(sid, "queue", "又一条", kindChat, "")
	step("两次入队")
	if !a.movePending(sid, last.ID, "") {
		t.Fatal("movePending 移到队尾应成功")
	}
	step("拖拽排序")
	a.setQueuePaused(sid, true)
	step("暂停")
	noBump := a.pendingOf(sid).Ver
	a.updatePending(sid, "不存在", "remove") // 不在队列：不 bump
	if v := a.pendingOf(sid).Ver; v != noBump {
		t.Fatalf("无效操作不应前进 ver: %d -> %d", noBump, v)
	}
	a.dropPending(sid)
	step("清空")
}

// TestPendingVerSnapshotAtomic ver 与内容在锁内原子对应（并发压测不出「ver 新、内容旧」的倒挂）。
func TestPendingVerSnapshotAtomic(t *testing.T) {
	a := newTestAppForVer()
	const sid = "s2"
	const workers = 8
	var mutWG, readWG sync.WaitGroup
	stop := make(chan struct{})
	// 快照读者：ver 只增不减（锁内 ++），任何回退观察都是实现 bug。读者各自
	// 本地记最大值校验——不经 channel 汇总（多读者投递乱序会制造假回退）。
	for i := 0; i < workers; i++ {
		readWG.Add(1)
		go func() {
			defer readWG.Done()
			localMax := int64(-1)
			for {
				select {
				case <-stop:
					return
				default:
					v := a.pendingOf(sid).Ver
					if v < localMax {
						t.Errorf("观察到 ver 回退：先见 %d 后见 %d——版本门前提被破坏", localMax, v)
						return
					}
					localMax = v
				}
			}
		}()
	}
	// 变更者：并发插话/排水/排队/暂停，每次变更 ver 都要前进
	for i := 0; i < workers; i++ {
		mutWG.Add(1)
		go func() {
			defer mutWG.Done()
			for j := 0; j < 200; j++ {
				a.pushPending(sid, "steer", "插话", kindChat, "interject")
				a.drainSteer(sid)
				a.pushPending(sid, "queue", "排队", kindChat, "")
				a.popQueue(sid)
				a.setQueuePaused(sid, j%2 == 0)
			}
		}()
	}
	mutWG.Wait()
	close(stop) // 变更全部完成：放读者收工
	readWG.Wait()
	if a.pendingOf(sid).Ver <= 0 {
		t.Fatal("并发压测后 ver 应已递增")
	}
}

// TestPendingVerDrainAdvances 排水后的快照 ver 必然大于排水前取到的快照 ver：
// 这是「旧快照（含插话）晚到会被前端版本门丢弃」的直接依据。
func TestPendingVerDrainAdvances(t *testing.T) {
	a := newTestAppForVer()
	const sid = "s3"
	a.pushPending(sid, "steer", "打断流式的插话", kindChat, "interject")
	before := a.pendingOf(sid) // 模拟 interject 响应在途取走的旧快照
	if len(before.Steer) != 1 || before.Ver == 0 {
		t.Fatalf("入队后快照应含 1 条插话且 ver>0: %+v", before)
	}
	a.drainSteer(sid) // 模拟主循环排水（插话落库生效）
	after := a.pendingOf(sid)
	if len(after.Steer) != 0 {
		t.Fatalf("排水后快照应为空: %+v", after.Steer)
	}
	if after.Ver <= before.Ver {
		t.Fatalf("排水后 ver=%d 必须大于排水前=%d，否则旧快照回放拦不住", after.Ver, before.Ver)
	}
}

// TestPendingVerTimestampBase ver 以毫秒时间戳为基底：重启后（模拟：换一个
// 全新 App、同一个会话）新 bump 出来的 ver 仍大于旧值——计数器基底会回卷，
// 挂在旧基线上的前端把重启后的所有快照拦掉，插话/排队状态同会话内失明。
func TestPendingVerTimestampBase(t *testing.T) {
	const sid = "s4"
	old := newTestAppForVer()
	old.pushPending(sid, "steer", "旧进程里的插话", kindChat, "interject")
	prev := old.pendingOf(sid).Ver
	if prev < 1_700_000_000_000 { // 2023-11 之后：时间戳量级，不是小计数
		t.Fatalf("ver=%d 不是时间戳量级（基底退化了）", prev)
	}
	// 模拟进程重启：全新 App（计数器会从 0 开始，时间戳基底则从当前时刻开始）
	time.Sleep(5 * time.Millisecond) // 真实重启至少隔毫秒级；同毫秒撞值时等于拦一拍，可忽略
	restarted := newTestAppForVer()
	restarted.pushPending(sid, "steer", "重启后的插话", kindChat, "interject")
	if v := restarted.pendingOf(sid).Ver; v <= prev {
		t.Fatalf("重启后 ver=%d 未超过旧进程的 %d：前端旧基线会永久拦掉新快照", v, prev)
	}
}
