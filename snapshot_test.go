package main

// 快照超龄清理（snapshotJanitor → PruneCheckpointsOlderThan）的回归钉子：
// 截止时刻与 checkpoints.created_at 在 SQL 里是字符串比较，两边的格式必须逐字
// 一致。created_at 由 now() 落库（空格分隔）；截止时刻曾写成带「T」的形态，
// 空格(0x20) < T(0x54)，截止日**当天**的快照被整日判成超龄——最多提前近一天
// 被删（2026-11-26 00:13 跨天后的首次启动，11-19 全天 308 条快照被清掉）。

import (
	"reflect"
	"testing"
	"time"
)

func TestSnapshotPruneCutoffKeepsSameDaySnapshots(t *testing.T) {
	// 「现在」钉在 11-26 00:13:02，保留 7 天 → 截止 11-19 00:13:02
	at := time.Date(2026, 11, 26, 0, 13, 2, 0, time.Local)
	cutoff := snapshotCutoff(at, 7)

	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	const sid = "sess-cutoff"
	if err := st.Create(&SessionMeta{ID: sid, Mode: "task"}); err != nil {
		t.Fatal(err)
	}
	// 落库格式与 recordCheckpoint 一致（now()）：空格分隔、秒级
	add := func(path, created string) {
		t.Helper()
		if _, err := st.AppendCheckpoint(&Checkpoint{SessionID: sid, Seq: 1, Path: path, CreatedAt: created, BeforeSize: 1}); err != nil {
			t.Fatal(err)
		}
	}
	add("old.txt", "2026-11-18 23:59:59")  // 截止日前一天：超龄
	add("edge.txt", "2026-11-19 00:13:01") // 截止时刻前一秒：超龄
	add("same.txt", "2026-11-19 00:13:02") // 正好等于截止：未超龄（>= 截止即保留）
	add("noon.txt", "2026-11-19 14:00:00") // 截止日当天、晚于截止时刻：未满 7 天，必须保留
	add("fresh.txt", "2026-11-25 23:00:00")
	// 同一文件：旧的一份超龄，但当天还有一份更新的 → 按文件原子清理，整组保留
	add("pair.txt", "2026-11-18 10:00:00")
	add("pair.txt", "2026-11-19 20:00:00")

	ids, err := st.PruneCheckpointsOlderThan(sid, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	// 只该删 old.txt、edge.txt（自增 id 1、2）
	if got, want := ids, []int64{1, 2}; !reflect.DeepEqual(got, want) {
		left, _ := st.CheckpointsAfter(sid, 0)
		t.Fatalf("超龄清理删掉的快照 = %v，期望 %v；清理后还剩 %d 条（截止时刻 %q）", got, want, len(left), cutoff)
	}
}
