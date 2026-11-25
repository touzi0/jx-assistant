package main

// componentbridge.go 帧协议与钩子机制的集成测试：临时组件目录 + Python worker
//（共享便携运行时 技能/_runtime，缺则跳过）走真实桥链路——
//   · 帧模式 call 端点（req/res 按 id 解复用）
//   · 反向调用（host/hostres：worker 在处理 req 时向主程序要 /api/components）
//   · prompt.submit 拦截型决定：附加上下文 / 改写 / 拒绝
//   · 超时 fail-open（worker 睡过等待上限，主流程按放行返回且不陪着等）
//   · HookSubscribers 订阅匹配（含未启用剔除与 id 升序）

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testPython 找一个可用的 Python 解释器：优先共享便携运行时（与生产 {python}
// 同一份；与产品同源走 runtimePyInside（Windows: python/python.exe，macOS:
// macos/bin/python3——内置 mac 运行时入库后仓库里两份都在），PATH 兜底
//（mac 用 python3）；都没有就跳过集成测试（纯单元场景不受影响）。
func testPython(t *testing.T) string {
	t.Helper()
	if p := runtimePyInside(filepath.Join("技能", "_runtime")); func() bool { _, err := os.Stat(p); return err == nil }() {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
	}
	if p, err := exec.LookPath("python3"); err == nil {
		return p
	}
	if p, err := exec.LookPath("python"); err == nil {
		return p
	}
	t.Skip("没有可用的 Python 运行时（技能/_runtime 未装配且 PATH 无 python）：跳过桥钩子集成测试")
	return ""
}

// hookWorkerPy 是测试 worker：帧协议全实现。req 处理中先反向调用主程序
// /api/components（host 帧）拿组件数再回 res；prompt.submit 按正文关键词回
// 不同决定；收到未知事件一律安静忽略。慢速版（sleep）由测试另行拼装。
const hookWorkerPy = `# 测试 worker：桥帧协议（req/res/event/decision/host/hostres）
import json, sys

def send(obj):
    sys.stdout.write(json.dumps(obj, ensure_ascii=False) + "\n")
    sys.stdout.flush()

def wait_hostres(want):
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            f = json.loads(line)
        except Exception:
            continue
        if f.get("type") == "hostres" and f.get("id") == want:
            return f
    return None

for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        f = json.loads(line)
    except Exception:
        continue
    t = f.get("type")
    if t == "req":
        send({"type": "host", "id": "h1", "method": "GET", "path": "/api/components"})
        hr = wait_hostres("h1")
        n = -1
        if hr and hr.get("status") == 200:
            n = len((hr.get("body") or {}).get("components") or [])
        send({"type": "res", "id": f["id"], "body": {"ok": True, "components": n, "echo": (f.get("body") or {}).get("text", "")}})
    elif t == "event":
        ev = f.get("event")
        if ev == "prompt.submit":
            body = (f.get("data") or {}).get("content", "")
            if "拒绝" in body:
                send({"type": "decision", "id": f["id"], "block": True, "reason": "测试拒绝"})
            elif "改写" in body:
                send({"type": "decision", "id": f["id"], "content": body + "（组件改写）"})
            else:
                send({"type": "decision", "id": f["id"], "additionalContext": "组件附加：累计输入 " + str(len(body)) + " 字"})
        elif ev == "run.end":
            send({"type": "decision", "id": f["id"], "block": False})
`

const hookSlowWorkerPre = `# 慢速 worker：拦截型事件上睡过等待上限，验证主流程 fail-open 不陪着等
import json, sys, time

def send(obj):
    sys.stdout.write(json.dumps(obj, ensure_ascii=False) + "\n")
    sys.stdout.flush()

for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        f = json.loads(line)
    except Exception:
        continue
    t = f.get("type")
    if t == "event" and f.get("event") == "prompt.submit":
        time.sleep(2.0)
        send({"type": "decision", "id": f["id"], "additionalContext": "迟到的上下文"})
`

// newHookTestApp 建一个带两个测试组件（正常 + 慢速）的 App：hooktest 启用，
// hookslow 启用（超时验证用它），另一个 hookoff 显式停用（订阅剔除验证）。
func newHookTestApp(t *testing.T) *App {
	t.Helper()
	py := testPython(t)
	dir := t.TempDir()
	write := func(comp, worker string, hooks []string, timeoutMs int) {
		t.Helper()
		cd := filepath.Join(dir, comp)
		if err := os.MkdirAll(cd, 0755); err != nil {
			t.Fatalf("建组件目录失败: %v", err)
		}
		if err := os.WriteFile(filepath.Join(cd, "worker.py"), []byte(worker), 0644); err != nil {
			t.Fatalf("写 worker 失败: %v", err)
		}
		m := ComponentManifest{ID: comp, Name: comp, Version: "0"}
		m.Workers = []ComponentWorkerDef{{
			Name:           "w",
			Command:        []string{py, "worker.py"},
			Hooks:          hooks,
			HooksTimeoutMs: timeoutMs,
		}}
		data, _ := json.Marshal(m)
		if err := os.WriteFile(filepath.Join(cd, "manifest.json"), data, 0644); err != nil {
			t.Fatalf("写 manifest 失败: %v", err)
		}
	}
	write("hooktest", hookWorkerPy, []string{"prompt.submit", "run.end"}, 0)
	write("hookslow", hookSlowWorkerPre, []string{"prompt.submit"}, 800)
	write("hookoff", hookWorkerPy, []string{"prompt.submit"}, 0)

	a := &App{}
	a.Components = InitComponents(dir, filepath.Join(dir, "components.json"))
	a.Bridge = newBridgeManager(a)
	// hookoff 显式停用（未记录的缺省启用）
	if err := a.Components.SetEnabled("hookoff", false); err != nil {
		t.Fatalf("停用 hookoff 失败: %v", err)
	}
	t.Cleanup(func() {
		a.Bridge.KillComponent("hooktest")
		a.Bridge.KillComponent("hookslow")
		a.Bridge.KillComponent("hookoff")
	})
	return a
}

func TestBridgeHookSubscribers(t *testing.T) {
	a := newHookTestApp(t)
	subs := a.Components.HookSubscribers("prompt.submit")
	if len(subs) != 2 { // hookoff 停用被剔除；hooktest、hookslow 都订阅
		t.Fatalf("prompt.submit 应有 2 个订阅（hookoff 停用剔除），得到 %d", len(subs))
	}
	if subs[0].CompID != "hookslow" || subs[1].CompID != "hooktest" {
		t.Fatalf("订阅应按组件 id 升序（hookslow < hooktest），得到 %s, %s", subs[0].CompID, subs[1].CompID)
	}
	if got := a.Components.HookSubscribers("run.end"); len(got) != 1 || got[0].CompID != "hooktest" {
		t.Fatalf("run.end 应只有 hooktest 订阅，得到 %v", got)
	}
	if got := a.Components.HookSubscribers("no.such.event"); len(got) != 0 {
		t.Fatalf("未订阅的事件应返回空，得到 %v", got)
	}
}

func TestBridgeFrameCallWithHost(t *testing.T) {
	a := newHookTestApp(t)
	m, ok := a.Components.Manifest("hooktest")
	if !ok || len(m.Workers) != 1 {
		t.Fatal("hooktest manifest 缺失")
	}
	// 帧模式 call：body 包 req 帧；worker 处理中先反向调用 /api/components
	//（host 帧）拿组件数再回 res——一次往返同时验证 req/res 解复用与反向调用
	rec := hookCallEndpoint(t, a, "hooktest", m.Workers[0], `{"text":"你好"}`)
	var res struct {
		OK         bool `json:"ok"`
		Components int  `json:"components"`
		Echo       string `json:"echo"`
	}
	if err := json.Unmarshal(rec, &res); err != nil {
		t.Fatalf("res 帧应答解析失败: %v（%s）", err, rec)
	}
	if !res.OK || res.Echo != "你好" {
		t.Fatalf("帧模式 call 应答不符: %+v", res)
	}
	if res.Components < 3 { // 三个测试组件都应被 /api/components 数到
		t.Fatalf("反向调用 /api/components 应数到 3 个组件，得到 %d", res.Components)
	}
}

func hookCallEndpoint(t *testing.T, a *App, compID string, def ComponentWorkerDef, body string) []byte {
	t.Helper()
	// 直接走 bridgeCall（handleComponentBridge 分发后的同一处理函数）
	req := httptest.NewRequest("POST", "/api/c/"+compID+"/x", strings.NewReader(body))
	rec := httptest.NewRecorder()
	a.bridgeCall(rec, req, compID, def)
	if rec.Code != 200 {
		t.Fatalf("call 端点应 200，得到 %d（%s）", rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

func TestBridgeHookTimeoutFailOpen(t *testing.T) {
	a := newHookTestApp(t)
	// 本用例只留慢速订阅者：hooktest 也订阅 prompt.submit，它的健康应答会盖住
	// fail-open 的语义，先停用（测完恢复，后续用例不受伤）
	if err := a.Components.SetEnabled("hooktest", false); err != nil {
		t.Fatalf("停用 hooktest 失败: %v", err)
	}
	t.Cleanup(func() { _ = a.Components.SetEnabled("hooktest", true) })
	t0 := time.Now()
	out := a.emitComponentHooks("prompt.submit", map[string]any{"sessionID": "s1", "content": "慢慢想"})
	elapsed := time.Since(t0)
	if out.Block || out.Content != "" || out.AdditionalContext != "" {
		t.Fatalf("超时应按放行（fail-open）返回零决定: %+v", out)
	}
	// hookslow 的等待上限 800ms（worker 睡 2s）——主流程不该陪着等完
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("超时后应立即放行，实际等了 %s", elapsed)
	}
}

func TestBridgeRunEndNotification(t *testing.T) {
	a := newHookTestApp(t)
	// 通知型：送达即止（不等 decision），零值返回即正确
	out := a.emitComponentHooks("run.end", map[string]any{"sessionID": "s1", "status": "done", "error": ""})
	if out.Block || out.Content != "" || out.AdditionalContext != "" {
		t.Fatalf("通知型事件应返回零值: %+v", out)
	}
}
