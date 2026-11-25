package main

// 远程帧镜像（handleBrowserFrame）的针对性测试：懒截图、内容指纹去重、
// 450ms 节流、版本用时间戳（关闭重开不回退到客户端旧版本而永远发不出帧）、
// 未运行回 running:false、形态无 CaptureFrame 实现回 501。假 target 复用
// browser_close_test.go 的 fakeBrowserTarget（嵌入扩展），不碰真浏览器。

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

// fakeFrameTarget 可编程的帧源：frames 依次弹出（取尽后重复最后一帧），
// captures 记录真实截图次数（验证节流与「没轮询就没截图」）。
type fakeFrameTarget struct {
	fakeBrowserTarget
	mu       sync.Mutex
	frames   [][]byte
	captures int
}

func (f *fakeFrameTarget) State() BrowserState { return BrowserState{Running: true, VW: 800, VH: 600} }
func (f *fakeFrameTarget) CaptureFrame() ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.frames) == 0 {
		return nil, errors.New("帧队列已空")
	}
	b := f.frames[0]
	if len(f.frames) > 1 {
		f.frames = f.frames[1:]
	}
	f.captures++
	return b, nil
}

// noFrameTarget 运行中但没有 CaptureFrame 实现（无镜像能力的面板形态）。
type noFrameTarget struct{ fakeBrowserTarget }

func (f *noFrameTarget) State() BrowserState { return BrowserState{Running: true} }

func TestBrowserFrameMirror(t *testing.T) {
	a, main := newDispatchTestApp(t)
	fake := &fakeFrameTarget{frames: [][]byte{[]byte("frame-A"), []byte("frame-B")}}
	a.panelMu.Lock()
	a.panels = map[string]browserTarget{
		browserProfileKey(main.ID): fake,
		"":                         &noFrameTarget{},
	}
	a.panelDispatch = func(f func()) { f() }
	a.panelMu.Unlock()

	get := func(sid, v string) (map[string]any, int) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/browser/frame?session="+sid+"&v="+v, nil)
		a.handleBrowserFrame(w, r)
		var m map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &m)
		return m, w.Code
	}
	captures := func() int { fake.mu.Lock(); defer fake.mu.Unlock(); return fake.captures }
	frameData := func(m map[string]any) string {
		s, _ := m["data"].(string)
		raw, _ := base64.StdEncoding.DecodeString(s)
		return string(raw)
	}
	staleCache := func() { // 把最小间隔拨过：让下一次请求真正重截（模拟 450ms 过去）
		a.frameMu.Lock()
		e := a.frameCache[browserProfileKey(main.ID)]
		a.frameMu.Unlock()
		if e == nil {
			t.Fatalf("缓存条目缺失")
		}
		e.mu.Lock()
		e.last = time.Now().Add(-2 * frameEvery)
		e.mu.Unlock()
	}

	// 1) 首次轮询：changed=true 帧全量下发，版本为时间戳
	m, code := get(main.ID, "0")
	if code != 200 || m["running"] != true || m["changed"] != true {
		t.Fatalf("首次轮询应带帧: code=%d m=%v", code, m)
	}
	if got := frameData(m); got != "frame-A" {
		t.Fatalf("首帧内容错误: %q", got)
	}
	if m["mime"] != "image/webp" {
		t.Fatalf("mime 应为 webp: %v", m["mime"])
	}
	ver, _ := m["ver"].(float64)
	if ver <= 0 {
		t.Fatalf("版本应为时间戳: %v", m["ver"])
	}
	if m["vw"] != float64(800) || m["vh"] != float64(600) {
		t.Fatalf("应带 CSS 视口尺寸: %v", m)
	}

	// 2) 450ms 内立刻再轮询：节流不重截；客户端版本未跟上时仍回缓存帧
	m, _ = get(main.ID, "0")
	if captures() != 1 {
		t.Fatalf("节流失败：间隔内不应重新截图（captures=%d）", captures())
	}
	if m["changed"] != true || frameData(m) != "frame-A" {
		t.Fatalf("间隔内应回缓存帧: %v", m)
	}
	// 客户端已持有当前版本：changed=false，不带帧（静止页面的常态）
	m, _ = get(main.ID, strconv.FormatInt(int64(ver), 10))
	if m["changed"] != false {
		t.Fatalf("版本一致时应 changed=false: %v", m)
	}
	if _, has := m["data"]; has {
		t.Fatalf("changed=false 不应携带帧数据")
	}

	// 3) 内容变化：新帧、版本前进
	staleCache()
	m, _ = get(main.ID, strconv.FormatInt(int64(ver), 10))
	if m["changed"] != true || frameData(m) != "frame-B" {
		t.Fatalf("内容变化应下发新帧: %v", m)
	}
	ver2, _ := m["ver"].(float64)
	if ver2 <= ver {
		t.Fatalf("内容变化后版本应前进: %v → %v", ver, ver2)
	}

	// 4) 内容没变（frame-B 再截一次）：版本不动，changed=false
	staleCache()
	m, _ = get(main.ID, strconv.FormatInt(int64(ver2), 10))
	if m["changed"] != false {
		t.Fatalf("同内容不应升版本: %v", m)
	}

	// 5) 无 CaptureFrame 实现的形态：501
	if _, code = get("", "0"); code != 501 {
		t.Fatalf("无帧实现的形态应 501: %d", code)
	}

	// 6) 关闭清理（frameForget）：缓存清空后重建，版本时间戳必然不同于客户端
	//    旧值——远程端不会因版本回退而永远拿不到新帧
	a.frameForget(main.ID)
	if _, code = get(main.ID, "0"); code != 200 {
		t.Fatalf("清缓存后应重新可用: %d", code)
	}
}

// TestBrowserFrameNotRunning 浏览器未打开（Running=false）：回 running:false，
// 远程端据此显示「电脑端浏览器未打开」，且不触碰截图通道。
func TestBrowserFrameNotRunning(t *testing.T) {
	a, main := newDispatchTestApp(t)
	a.panelMu.Lock()
	a.panels = map[string]browserTarget{browserProfileKey(main.ID): &fakeBrowserTarget{}}
	a.panelDispatch = func(f func()) { f() }
	a.panelMu.Unlock()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/browser/frame?session="+main.ID+"&v=0", nil)
	a.handleBrowserFrame(w, r)
	var m map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	if m["running"] != false {
		t.Fatalf("未运行应回 running=false: %v", m)
	}
	if _, has := m["data"]; has {
		t.Fatalf("未运行不应带帧")
	}
}
