package main

// 排队坞增强的纯逻辑测试：拖拽排序（movePending 的锚点定序）与队列暂停
// 状态（快照下发）。不碰存储与执行体，App 只需初始化内存态的几张表。

import "testing"

func newPendingTestApp() *App {
	a := &App{}
	a.pendingSteer = map[string][]*PendingMsg{}
	a.pendingQueue = map[string][]*PendingMsg{}
	a.queuePaused = map[string]bool{}
	a.pendVer = map[string]int64{}
	a.subscribers = map[chan Event]bool{}
	return a
}

func idsOf(list []*PendingMsg) []string {
	out := make([]string, 0, len(list))
	for _, pm := range list {
		out = append(out, pm.ID)
	}
	return out
}

func eqIDs(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("队列顺序 = %v，期望 %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("队列顺序 = %v，期望 %v", got, want)
		}
	}
}

func TestMovePending(t *testing.T) {
	a := newPendingTestApp()
	a.pendingQueue["s"] = []*PendingMsg{{ID: "a"}, {ID: "b"}, {ID: "c"}}

	// 末尾项移到最前（拖拽把 c 放到 a 的上半行）
	if !a.movePending("s", "c", "a") {
		t.Fatal("c 移到 a 之前应成功")
	}
	eqIDs(t, idsOf(a.pendingQueue["s"]), "c", "a", "b")

	// before 空 = 移到队尾
	if !a.movePending("s", "c", "") {
		t.Fatal("移到队尾应成功")
	}
	eqIDs(t, idsOf(a.pendingQueue["s"]), "a", "b", "c")

	// 头项跨一项移到 c 之前
	if !a.movePending("s", "a", "c") {
		t.Fatal("a 移到 c 之前应成功")
	}
	eqIDs(t, idsOf(a.pendingQueue["s"]), "b", "a", "c")

	// 放回原位（before=自身）：无操作，视为成功
	if !a.movePending("s", "a", "a") {
		t.Fatal("原地移动应视为成功")
	}
	eqIDs(t, idsOf(a.pendingQueue["s"]), "b", "a", "c")

	// 锚点刚被消费：失败且顺序保持不动
	if a.movePending("s", "a", "zz") {
		t.Fatal("锚点不在队列应失败")
	}
	eqIDs(t, idsOf(a.pendingQueue["s"]), "b", "a", "c")

	// 移动项不在队列：失败
	if a.movePending("s", "zz", "a") {
		t.Fatal("移动项不在队列应失败")
	}

	// 只在插话队列里的项不参与排队排序
	a.pendingSteer["s"] = []*PendingMsg{{ID: "st"}}
	if a.movePending("s", "st", "a") {
		t.Fatal("插话项不应能被排队排序")
	}
}

func TestQueuePausedState(t *testing.T) {
	a := newPendingTestApp()
	if a.queuePausedOf("s") {
		t.Fatal("默认不应暂停")
	}
	a.setQueuePaused("s", true)
	if !a.queuePausedOf("s") {
		t.Fatal("暂停后应生效")
	}
	if !a.pendingOf("s").Paused {
		t.Fatal("暂停状态应随 pending 快照下发")
	}
	a.setQueuePaused("s", false)
	if a.pendingOf("s").Paused {
		t.Fatal("恢复后快照应为未暂停")
	}
	// 其他会话互不影响
	a.setQueuePaused("s2", true)
	if a.queuePausedOf("s") {
		t.Fatal("会话间暂停状态不应互相影响")
	}
}
