//go:build windows

package main

// 演示记录器 Windows 实现的实机测试：起录（装钩子+开录音+worker）→ 让窗口
// 轮询与首帧截屏自然发生 → 停录 → 验证材料产物与格式。真实点击不便在测试里
// 合成（钩子会过滤注入事件），靠自动事件（首帧截图/窗口轮询）覆盖链路主干。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecorderRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := recorderStart(dir); err != nil {
		t.Fatalf("recorderStart: %v", err)
	}
	// 起录后立即再开一次：应被拒绝（互斥）
	if err := recorderStart(t.TempDir()); err == nil {
		t.Fatal("重复起录未被拒绝")
	}
	if got := recorderStatusText(); !strings.Contains(got, "记录中") {
		t.Fatalf("状态文本异常: %s", got)
	}
	// 等 ticker（400ms）跑几轮：首帧截图 + 窗口事件落盘
	time.Sleep(1500 * time.Millisecond)
	stats, err := recorderStop()
	if err != nil {
		t.Fatalf("recorderStop: %v", err)
	}
	if stats.Dir != dir {
		t.Fatalf("目录不一致: %s != %s", stats.Dir, dir)
	}
	if stats.Shots < 1 {
		t.Fatalf("没有产出截图（shots=%d）", stats.Shots)
	}
	if stats.Events < 1 {
		t.Fatalf("没有事件（events=%d）", stats.Events)
	}
	// 二次停录应报「没有在录」
	if _, err := recorderStop(); err == nil {
		t.Fatal("重复停录未被拒绝")
	}

	// 事件日志：每行可解析、内部字段不落盘
	raw, err := os.ReadFile(filepath.Join(dir, recorderEventsFn))
	if err != nil {
		t.Fatalf("读事件日志: %v", err)
	}
	lines := 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var ev recEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("事件行不是合法 JSON: %q (%v)", line, err)
		}
		if ev.VK != 0 || ev.Up {
			t.Fatalf("内部字段泄漏到事件日志: %q", line)
		}
		lines++
	}
	if lines == 0 {
		t.Fatal("事件日志为空")
	}

	// 截图文件存在且是 PNG
	name := "001.png"
	if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
		// 编号从 1 起；若有窗口轮询首帧编号应为 001
		ents, _ := os.ReadDir(dir)
		pngs := []string{}
		for _, e := range ents {
			if strings.HasSuffix(e.Name(), ".png") {
				pngs = append(pngs, e.Name())
			}
		}
		if len(pngs) == 0 {
			t.Fatal("目录里没有 PNG")
		}
		name = pngs[0]
	}
	png, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil || len(png) < 8 || string(png[:4]) != "\x89PNG" {
		t.Fatalf("截图不是合法 PNG: %v (len=%d)", err, len(png))
	}

	// INDEX 生成 + SRT 对照解析
	if err := buildRecorderIndex(stats); err != nil {
		t.Fatalf("buildRecorderIndex: %v", err)
	}
	idx, err := os.ReadFile(filepath.Join(dir, recorderIndexFn))
	if err != nil {
		t.Fatalf("读 INDEX: %v", err)
	}
	if !strings.Contains(string(idx), "演示记录对照表") || !strings.Contains(string(idx), "| 时间 |") {
		t.Fatalf("INDEX 内容异常:\n%s", idx)
	}
	cues := parseSRT("1\n00:00:01,000 --> 00:00:03,500\n先点右上角导出\n\n2\n00:00:05,000 --> 00:00:06,000\n再确认\n")
	if len(cues) != 2 || cues[0].StartMs != 1000 || cues[0].EndMs != 3500 || cues[1].Text != "再确认" {
		t.Fatalf("SRT 解析异常: %+v", cues)
	}
}

// TestRecorderASRHookNil：默认不接线转写（voiceasr.go 的 NewApp 注册才有），
// 未接线时录制照常、只是不出字幕——测试环境保持这个前提。
func TestRecorderASRHookNil(t *testing.T) {
	if recorderASR != nil {
		t.Fatal("测试环境不应注册转写钩子")
	}
}

// TestRecorderCollectASRBackground 停录不等识别（recorderCollectASR）：识别 worker
// 还有段没转完时停录——立即返回、ASRPending=true；worker 把在途段转完（讲解.srt
// 落盘）后 asrDone 关闭，后台收尾经 recorderASRDone 钩子带着最终段数回调并重建
// INDEX.md。不开麦不挂钩子：手工搭识别队列与 worker，桩住 recorderASR 控制识别
// 何时完成。
func TestRecorderCollectASRBackground(t *testing.T) {
	oldASR, oldDone := recorderASR, recorderASRDone
	defer func() { recorderASR, recorderASRDone = oldASR, oldDone }()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, recorderEventsFn), []byte("{\"t\":500,\"type\":\"click\",\"x\":1,\"y\":2}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	newSession := func() *recAudioSession {
		s := &recAudioSession{r: &demoRec{dir: dir, start: time.Now()}, vad: newRecVAD(),
			asrCh: make(chan recSegment, 16), asrDone: make(chan struct{})}
		go s.asrWorker()
		return s
	}
	seg := func(i int) *recSegment {
		return &recSegment{wav: recPcmToWav(make([]byte, recFrameBytes*4)), startMs: i * 1000, endMs: i*1000 + 700}
	}
	release := make(chan struct{})
	recorderASR = func([]byte) (string, error) { <-release; return " 后台转完的段 ", nil }
	done := make(chan int, 1)
	recorderASRDone = func(stats *recorderStats, cues int) {
		_ = buildRecorderIndex(stats)
		done <- cues
	}
	s := newSession()
	for i := 0; i < 2; i++ { // 两段在途：识别桩卡着不返回
		s.enqueue(seg(i))
	}
	close(s.asrCh)
	stats := &recorderStats{Dir: dir, AudioOK: true}
	t0 := time.Now()
	recorderCollectASR(stats, s)
	if time.Since(t0) > 500*time.Millisecond {
		t.Fatalf("停录收尾不该等识别，却花了 %s", time.Since(t0))
	}
	if !stats.ASRPending || stats.Cues != 0 || stats.asrDone != s.asrDone {
		t.Fatalf("在途段未转完时应报后台识别中: %+v", stats)
	}
	if note := demoRecordStopNote(stats); !strings.Contains(note, "字幕：后台识别中") {
		t.Fatalf("通知应写后台识别中：%s", note)
	}
	select {
	case <-done:
		t.Fatal("识别没放行前不该收尾")
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	select {
	case n := <-done:
		if n != 2 {
			t.Fatalf("收尾应带最终段数 2，got %d", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("后台收尾未回调")
	}
	<-s.asrDone
	if s.pending.Load() != 0 {
		t.Fatalf("全部转完后 pending 应为 0，got %d", s.pending.Load())
	}
	cues := recorderReadCues(dir)
	if len(cues) != 2 || cues[0].Text != "后台转完的段" || cues[1].StartMs != 1000 {
		t.Fatalf("讲解.srt 落盘异常: %+v", cues)
	}
	idx, err := os.ReadFile(filepath.Join(dir, recorderIndexFn))
	if err != nil || !strings.Contains(string(idx), "后台转完的段") || !strings.Contains(string(idx), "（2 段") {
		t.Fatalf("后台收尾应重建含字幕的 INDEX.md: err=%v\n%s", err, idx)
	}

	// 没有在途段：ASRPending=false，不起后台收尾
	s2 := newSession()
	close(s2.asrCh)
	<-s2.asrDone
	stats2 := &recorderStats{Dir: dir, AudioOK: true}
	recorderCollectASR(stats2, s2)
	if stats2.ASRPending {
		t.Fatal("没有在途段不应报后台识别中")
	}
	// 没有识别线（语音组件未启用）：段直接不进队列，不算在途
	recorderASR = nil
	s3 := newSession()
	s3.enqueue(seg(0))
	if s3.pending.Load() != 0 {
		t.Fatalf("无识别线时不应计在途段，got %d", s3.pending.Load())
	}
	close(s3.asrCh)
	<-s3.asrDone
	// 识别失败的段：pending 照减、不写字幕
	recorderASR = func([]byte) (string, error) { return "", os.ErrPermission }
	s4 := newSession()
	s4.enqueue(seg(0))
	close(s4.asrCh)
	<-s4.asrDone
	if s4.pending.Load() != 0 || s4.cueCount() != 0 {
		t.Fatalf("失败段应丢弃且 pending 归零: pending=%d cues=%d", s4.pending.Load(), s4.cueCount())
	}
	// 队列满（识别严重滞后）：丢段保录制，不计在途——不起 worker（队列没人消费）
	// 才能确定性地把容量 1 的队列填满
	recorderASR = func([]byte) (string, error) { return "", nil }
	s5 := &recAudioSession{r: &demoRec{dir: dir}, vad: newRecVAD(), asrCh: make(chan recSegment, 1), asrDone: make(chan struct{})}
	s5.enqueue(seg(0))
	s5.enqueue(seg(1)) // 第二段超出容量：丢
	if s5.pending.Load() != 1 || len(s5.asrCh) != 1 {
		t.Fatalf("队列满应丢段不计在途，got pending=%d queued=%d", s5.pending.Load(), len(s5.asrCh))
	}
}
