package main

// componentbridge.go —— 声明式组件桥：组件自包含后端的通用宿主。
//
// 组件在 manifest 顶层声明 "workers"（见 components.go 的 ComponentWorkerDef）：
// 主程序按声明懒拉起子进程（{python}=共享便携运行时、{dir}=组件目录），并把端点
// 统一挂到 /api/c/<组件id>/<path>：
//   - type=call：HTTP body（一行 JSON）原样写 worker stdin、读一行应答原样返回
//     ——哑管，协议归组件（json-lines 一行一答）；
//   - type=ws：WebSocket 双向哑转发（每帧一行），断开发 {"cmd":"stream_end"} 收尾。
// 后端逻辑零主程序代码：组件（含预编译产物/脚本/模型）可市场直装、免编译、
// 免主程序更新；组件关闭即杀 worker（重开懒启复活）。生命周期兜底：进程退出由
// 顶层 kill-on-close Job Object 清场（与其他子进程同口径）。
//
// —— 帧模式（worker 声明了 hooks 时启用，未声明维持上面的哑管，存量组件零影响）——
// 声明了钩子的 worker，stdin/stdout 升级为带 id 的 JSON 帧双向解复用：
//   桥 → worker  {"type":"req","id":...,"body":<原请求 JSON>}        call 端点请求
//              {"type":"event","id":...,"event":...,"data":{...}}   钩子事件下发
//              {"type":"hostres","id":...,"status":200,"body":...}  反向调用应答
//   worker → 桥 {"type":"res","id":...,"body":<应答 JSON>}           call 应答（body 原样回 HTTP）
//              {"type":"decision","id":...,"block":...,"reason":...,
//               "content":...,"additionalContext":...}              钩子事件的决定（拦截型）
//              {"type":"host","id":...,"method":"GET",
//               "path":"/api/...","body":{...}}                     反向调用主程序 API
// 拦截型事件（prompt.submit）等 decision，超时/进程死/解析错一律 fail-open（放行
// + 日志）——组件故障不能拦住主流程；通知型事件（run.end）送达即止。反向调用以
// 内部路由执行主程序 API（权限同前端，本地信任模型，仅限 /api/ 前缀）。发射编排
// 见 emitComponentHooks；事件白名单 componentHookBlocking。协议细则见 组件/README.md。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// pipeLineLogger 把子进程的逐行输出转成日志（worker stderr 用）。
type pipeLineLogger struct{ prefix string }

func (p pipeLineLogger) Write(b []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(b), "\r\n"), "\n") {
		if line != "" {
			log.Printf("%s%s", p.prefix, line)
		}
	}
	return len(b), nil
}

type bridgeWorker struct {
	key    string // 组件id/worker名
	def    ComponentWorkerDef
	dir    string // 组件绝对目录
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	out    *bufio.Scanner
	mu     sync.Mutex // 单飞行：json-lines 一行一答；ws 会话期间独占
	dead   bool
	logger pipeLineLogger
	app    *App // 反向调用（host 帧）执行主程序内部路由用

	// 帧模式（def.Hooks 非空才启用，见文件头注释）：
	framed  bool                   // true=帧协议；false=哑管（存量行为）
	wmu     sync.Mutex             // stdin 写串行化（req/event/hostres 交织）
	pmu     sync.Mutex             // pending 表锁
	pending map[string]chan string // id -> 应答行（res/decision），缓冲 1
	seq     uint64                 // 帧自增 id
	done    chan struct{}          // 读循环退出（worker 死亡广播：等待者立即报错）
}

type bridgeManager struct {
	app *App
	mu  sync.Mutex
	ws  map[string]*bridgeWorker // key -> 懒启的进程
	// onKill 组件关闭时的"组件外部代价"回调（由 App 挂接）。
	// 用途：语音组件关掉时，除了 manifest 里的 workers，还要收掉同样属于该组件、
	// 但不在 manifest 里的独立进程（语音输入法）。回调在锁外调用，避免死锁。
	onKill func(compID string)
}

func newBridgeManager(app *App) *bridgeManager {
	m := &bridgeManager{app: app, ws: map[string]*bridgeWorker{}}
	// 组件关闭 → 收掉"属于该组件但不在 manifest workers 里"的独立进程。
	// 目前只有语音组件这一项：关掉「语音（朗读与听写）」时，独立语音输入法
	// （VoiceTypeGo，占着全局键盘钩子）必须一起结束，否则用户以为关掉了其实还在跑。
	m.onKill = func(compID string) {
		if compID == voiceComponentID {
			stopVoiceTypeByComponent()
		}
	}
	return m
}

// resolveCommand 解析 argv 模板：{python}=共享便携运行时、{dir}=组件绝对目录。
// 便携运行时只在命令真用到 {python} 时才解析——预编译产物（小 exe）worker
// 不依赖也不触发运行时装配（未装配时原本会被无谓阻断）。
func (m *bridgeManager) resolveCommand(def ComponentWorkerDef, dir string) ([]string, error) {
	var py string
	for _, a := range def.Command {
		if a == "{python}" {
			var err error
			if py, err = m.pythonExe(); err != nil {
				return nil, err
			}
			break
		}
	}
	argv := make([]string, 0, len(def.Command))
	for i, a := range def.Command {
		switch a {
		case "{python}":
			argv = append(argv, py)
		case "{dir}":
			argv = append(argv, dir)
		default:
			// 相对路径参数锚到组件目录（如 asr_worker.py）
			if i > 0 && !filepath.IsAbs(a) && strings.Contains(a, ".") && !strings.HasPrefix(a, "-") {
				if _, err := os.Stat(filepath.Join(dir, a)); err == nil {
					a = filepath.Join(dir, a)
				}
			}
			argv = append(argv, a)
		}
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("worker %s 的 command 为空", def.Name)
	}
	return argv, nil
}

// pythonExe 共享便携运行时（技能/_runtime——运行时共享原则，组件不自带）。
// 运行时已随仓库分发（全新克隆即有）；异常缺失时（老克隆/被误删）借技能装配
// 机制现场装一份，并顺带装上语音 worker 需要的 sherpa-onnx 与 numpy 轮子——
// asr_worker 的 transcribe 依赖 numpy，只装 sherpa 会在转写时报缺模块。
// 运行时已在但缺轮子的场景由 worker 自身的 pip 自愈兜底（ensure_deps）。
func (m *bridgeManager) pythonExe() (string, error) {
	if py, err := m.sharedPythonPath(); err == nil {
		return py, nil
	}
	if _, perr := m.app.ensureSkillRuntime([]string{"sherpa_onnx", "numpy"}, []string{"sherpa-onnx", "numpy"}); perr == nil {
		if py, err := m.sharedPythonPath(); err == nil {
			return py, nil
		}
	}
	py, _ := m.sharedPythonPath()
	return "", fmt.Errorf("便携运行时未装配（%s）：先在设置里启用任一技能触发装配，或手动放置", py)
}

func (m *bridgeManager) sharedPythonPath() (string, error) {
	dir, err := m.app.SkillsDir()
	if err != nil {
		return "", err
	}
	py := filepath.Join(dir, "_runtime", "python", "python.exe")
	if _, err := os.Stat(py); err != nil {
		return py, err
	}
	return py, nil
}

// get 懒拉起（或复用）组件 worker。调用方不得持有 w.mu。
func (m *bridgeManager) get(compID string, def ComponentWorkerDef) (*bridgeWorker, error) {
	key := compID + "/" + def.Name
	m.mu.Lock()
	defer m.mu.Unlock()
	if w := m.ws[key]; w != nil && w.cmd.ProcessState == nil {
		return w, nil
	}
	// 组件目录锚定：按归属解析（内置=组件/<id>，用户=组件/user/<id>），
	// 两条路的 worker、钩子、端点走同一个桥，组件自己不需要知道归属。
	dir, ok := m.app.Components.CompDir(compID)
	if !ok {
		return nil, fmt.Errorf("组件 %s 不在清单里（已卸载？）", compID)
	}
	argv, err := m.resolveCommand(def, dir)
	if err != nil {
		return nil, err
	}
	logger := pipeLineLogger{prefix: "[c/" + key + "] "}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Stderr = logger
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	hideConsoleWindow(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 worker 失败: %w", err)
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 64<<20) // 应答行可能含整段转写文本，宽裕些
	w := &bridgeWorker{key: key, def: def, dir: dir, cmd: cmd, stdin: stdin, out: sc, logger: logger, app: m.app}
	if len(def.Hooks) > 0 {
		// 帧模式：专职读循环做解复用（res/decision 按 id 分发、host 就地执行）
		w.framed = true
		w.pending = map[string]chan string{}
		w.done = make(chan struct{})
		go w.readLoop()
	}
	go func() {
		_ = cmd.Wait() // 进程退出；下次 get() 重建
		_ = stdout.Close()
	}()
	m.ws[key] = w
	log.Printf("[c/%s] worker 已拉起（%s，%s）", key, def.Name, map[bool]string{true: "帧模式", false: "哑管"}[w.framed])
	return w, nil
}

// callRaw 发一行原始请求、读一行原始应答（哑管：内容归组件协议）。
func (w *bridgeWorker) callRaw(line string) (string, error) {
	if _, err := w.stdin.Write([]byte(line + "\n")); err != nil {
		return "", fmt.Errorf("worker 管道已断: %w", err)
	}
	if !w.out.Scan() {
		if err := w.out.Err(); err != nil {
			return "", fmt.Errorf("读 worker 应答失败: %v", err)
		}
		return "", fmt.Errorf("worker 无应答（已退出）")
	}
	return w.out.Text(), nil
}

// ---------- 帧模式（声明了 hooks 的 worker；帧型见文件头注释） ----------

func (w *bridgeWorker) newID() string {
	return strconv.FormatUint(atomic.AddUint64(&w.seq, 1), 10)
}

// writeFrame 帧模式写一行（stdin 串行化：req/event/hostres 可能来自不同 goroutine）。
func (w *bridgeWorker) writeFrame(line string) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	if _, err := w.stdin.Write([]byte(line + "\n")); err != nil {
		return fmt.Errorf("worker 管道已断: %w", err)
	}
	return nil
}

// waitFrame 登记等待某 id 的应答行；返回值第二个是撤销函数（超时/出错时清掉登记，
// 防止迟到的行落进下一个同 id 等待者——id 自增不会复用，这只是不泄漏表项）。
func (w *bridgeWorker) waitFrame(id string) (<-chan string, func()) {
	ch := make(chan string, 1)
	w.pmu.Lock()
	w.pending[id] = ch
	w.pmu.Unlock()
	return ch, func() {
		w.pmu.Lock()
		delete(w.pending, id)
		w.pmu.Unlock()
	}
}

// deliver 把应答行投给等待者（无人等或已离开就丢弃——缓冲 1 非阻塞投递）。
func (w *bridgeWorker) deliver(id, line string) {
	w.pmu.Lock()
	ch := w.pending[id]
	delete(w.pending, id)
	w.pmu.Unlock()
	if ch != nil {
		select {
		case ch <- line:
		default:
		}
	}
}

// readLoop 帧模式专职读循环：res/decision 按 id 分发，host 帧异步执行反向调用；
// 非帧行（worker 打印的调试输出等）进日志兜底可见。stdout 读尽即退出并广播
// done——所有在途等待者立刻报错，下次 get() 重建进程。
func (w *bridgeWorker) readLoop() {
	defer close(w.done)
	for w.out.Scan() {
		line := w.out.Text()
		var head struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &head); err != nil || head.Type == "" || head.ID == "" {
			_, _ = w.logger.Write([]byte(line))
			continue
		}
		switch head.Type {
		case "res", "decision":
			w.deliver(head.ID, line)
		case "host":
			go w.serveHost(line)
		default:
			_, _ = w.logger.Write([]byte(line))
		}
	}
	w.pmu.Lock()
	w.pending = map[string]chan string{} // 在途等待者靠 done 广播报错；表清空即可
	w.pmu.Unlock()
}

// callFrame 帧模式的 call 端点请求：body 包成 req 帧，等对应 res 帧返回其 body
// 字段（JSON 文本，原样作为 HTTP 应答）。超时口径与哑管一致（180 秒）。
func (w *bridgeWorker) callFrame(body string) (string, error) {
	if !json.Valid([]byte(body)) {
		body = strconv.Quote(body) // call 端点要求一行 JSON；异常输入按字符串兜底
	}
	id := w.newID()
	ch, unwait := w.waitFrame(id)
	defer unwait()
	if err := w.writeFrame(`{"type":"req","id":"` + id + `","body":` + body + `}`); err != nil {
		return "", err
	}
	select {
	case line := <-ch:
		var res struct {
			Body json.RawMessage `json:"body"`
		}
		if err := json.Unmarshal([]byte(line), &res); err != nil {
			return "", fmt.Errorf("worker 应答帧解析失败: %v", err)
		}
		if len(res.Body) == 0 {
			return "{}", nil
		}
		return string(res.Body), nil
	case <-time.After(180 * time.Second):
		return "", fmt.Errorf("worker 应答超时（180 秒）")
	case <-w.done:
		return "", fmt.Errorf("worker 无应答（已退出）")
	}
}

// serveHost 执行 worker 的反向调用帧：以内部路由跑主程序 API（权限同前端——本地
// 信任模型，见 README），结果 hostres 帧写回。仅限 /api/ 前缀；60 秒兜底防挂死。
func (w *bridgeWorker) serveHost(line string) {
	var f struct {
		ID     string          `json:"id"`
		Method string          `json:"method"`
		Path   string          `json:"path"`
		Body   json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal([]byte(line), &f); err != nil || f.ID == "" {
		_, _ = w.logger.Write([]byte("host 帧解析失败: " + line))
		return
	}
	method := strings.ToUpper(f.Method)
	if method == "" {
		method = "GET"
	}
	if !strings.HasPrefix(f.Path, "/api/") {
		w.hostRespond(f.ID, http.StatusForbidden, []byte(`{"error":"反向调用仅限 /api/ 前缀"}`))
		return
	}
	var rd io.Reader
	if len(f.Body) > 0 && method != "GET" && method != "HEAD" {
		rd = bytes.NewReader(f.Body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req := httptest.NewRequest(method, f.Path, rd).WithContext(ctx)
	rec := httptest.NewRecorder()
	w.app.Routes().ServeHTTP(rec, req)
	w.hostRespond(f.ID, rec.Code, rec.Body.Bytes())
}

func (w *bridgeWorker) hostRespond(id string, status int, body []byte) {
	bodyField := "null"
	if b := strings.TrimSpace(string(body)); b != "" {
		if json.Valid([]byte(b)) {
			bodyField = b // 本来就是 JSON（绝大多数 /api/ 端点）：原样内嵌
		} else {
			bodyField = strconv.Quote(b) // 非 JSON 文本：按字符串包一层
		}
	}
	if err := w.writeFrame(`{"type":"hostres","id":` + strconv.Quote(id) + `,"status":` + strconv.Itoa(status) + `,"body":` + bodyField + `}`); err != nil {
		log.Printf("[c/%s] hostres 写回失败: %v", w.key, err)
	}
}

// kill 立即终止进程（幂等）。
func (w *bridgeWorker) kill() {
	if p := w.cmd.Process; p != nil {
		_ = p.Kill()
	}
}

// KillComponent 关闭组件/卸载时杀它声明的全部 worker。
func (m *bridgeManager) KillComponent(compID string) {
	m.mu.Lock()
	var toKill []*bridgeWorker
	// key 的格式是 "组件id/worker名"（见 get()），按前缀匹配该组件的所有 worker
	prefix := compID + "/"
	for k, w := range m.ws {
		if strings.HasPrefix(k, prefix) {
			toKill = append(toKill, w)
			delete(m.ws, k)
		}
	}
	onKill := m.onKill
	m.mu.Unlock()
	for _, w := range toKill {
		w.kill()
	}
	// 组件关闭的"组件外部代价"：由 App 挂接（例如语音组件关掉时要连独立语音输入法
	// 一起收掉——它不在 manifest 的 workers 里，但同样属于这个组件的能力）。
	if onKill != nil {
		onKill(compID)
	}
}

// handleComponentBridge GET/POST /api/c/{id}/{rest...}：按启用中组件的 manifest
// 端点声明分发（未命中/未启用 404——与组件资产 404 口径一致）。固定路由在启动
// 时注册一次，之后全靠 manifest 数据驱动，无需动态改路由。
func (a *App) handleComponentBridge(w http.ResponseWriter, r *http.Request) {
	compID := r.PathValue("id")
	path := strings.Trim(r.PathValue("rest"), "/")
	if compID == "" || path == "" {
		http.NotFound(w, r)
		return
	}
	m, ok := a.Components.Manifest(compID)
	// 生效口径：全局开着，或还有任何会话显式要它（按对话开关）——桥请求不带
	// 会话（组件脚本只认自己的端点）；会话关掉的对话里组件 UI 已热卸载，不会来。
	if !ok || !a.cxoComponentLive(compID) {
		http.NotFound(w, r)
		return
	}
	for _, def := range m.Workers {
		for _, ep := range def.Endpoints {
			if !strings.EqualFold(ep.Method, r.Method) || ep.Path != path {
				continue
			}
			switch ep.Type {
			case "call":
				a.bridgeCall(w, r, compID, def)
			case "ws":
				a.bridgeWS(w, r, compID, def)
			default:
				http.NotFound(w, r)
			}
			return
		}
	}
	http.NotFound(w, r)
}

// bridgeCall HTTP 一行一答哑管：body（JSON）原样进 worker stdin，应答行原样返回。
// 帧模式 worker（声明了 hooks）走 callFrame：body 包 req 帧、等 res 帧的 body。
func (a *App) bridgeCall(w http.ResponseWriter, r *http.Request, compID string, def ComponentWorkerDef) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil || len(body) == 0 {
		writeErr(w, http.StatusBadRequest, "请求体读取失败（需一行 JSON，上限 32MB）")
		return
	}
	wk, err := a.Bridge.get(compID, def)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if wk.framed {
		line, err := wk.callFrame(string(body))
		if err != nil {
			wk.kill()
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(line))
		return
	}
	wk.mu.Lock()
	defer wk.mu.Unlock()
	type res struct {
		line string
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		line, err := wk.callRaw(string(body))
		ch <- res{line, err}
	}()
	timeout := time.After(180 * time.Second)
	select {
	case r2 := <-ch:
		if r2.err != nil {
			wk.kill()
			writeErr(w, http.StatusInternalServerError, r2.err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(r2.line))
	case <-timeout:
		wk.kill()
		writeErr(w, http.StatusGatewayTimeout, "worker 应答超时（180 秒），已重启通道")
	}
}

// bridgeWS WebSocket 双向哑转发（照搬语音流式验证过的模式）：每帧一行进 worker、
// 每行应答一帧回浏览器；断开时发 stream_end 收尾。会话期间独占该 worker。
func (a *App) bridgeWS(w http.ResponseWriter, r *http.Request, compID string, def ComponentWorkerDef) {
	conn, _, _, err := ws.UpgradeHTTP(r, w)
	if err != nil {
		return
	}
	defer conn.Close()
	wk, err := a.Bridge.get(compID, def)
	if err != nil {
		_ = wsutil.WriteServerText(conn, []byte(`{"ok":false,"err":"`+jsonEscape(err.Error())+`"}`))
		return
	}
	wk.mu.Lock()
	defer wk.mu.Unlock()
	for {
		msg, err := wsutil.ReadClientText(conn)
		if err != nil {
			break
		}
		var line string
		if wk.framed {
			// 帧模式：每帧包 req、等 res 的 body（会话独占口径与哑管一致）
			line, err = wk.callFrame(string(msg))
		} else {
			line, err = wk.callRaw(string(msg))
		}
		if err != nil {
			_ = wsutil.WriteServerText(conn, []byte(`{"ok":false,"err":"`+jsonEscape(err.Error())+`"}`))
			wk.kill()
			return
		}
		if err := wsutil.WriteServerText(conn, []byte(line)); err != nil {
			break
		}
	}
	// 断开即收尾：通知 worker 结束当前流（组件协议自理；下次 stream_start 会重置）
	_, _ = wk.callRaw(`{"cmd":"stream_end"}`)
}

// ---------- 钩子事件发射（组件后端 ⇄ 主程序后端的下行通道） ----------

// componentHookBlocking 是拦截型事件表（在表=要等 worker 的 decision，可 block/
// 改写/附加上下文）；不在表的事件都是通知型（送达即止，decision 忽略）。加新事件
// 点位时同步登记到这里 + 组件/README.md 的事件表。
var componentHookBlocking = map[string]bool{
	"prompt.submit": true, // 用户消息提交（handleChat，CC 兼容层之前）
}

// bridgeDecision 是 decision 帧的内容（拦截型钩子里组件对主流程的意志）。
type bridgeDecision struct {
	Block             bool   `json:"block"`             // true=拒绝这条消息（403 + reason）
	Reason            string `json:"reason"`            // 拒绝理由（随 403 给前端）
	Content           string `json:"content"`           // 非空=改写后的消息正文
	AdditionalContext string `json:"additionalContext"` // 非空=附加上下文（用户消息后落系统注入行）
}

// emitEvent 给一个 worker 下发事件。blocking=true 等 decision 帧；超时/进程死/
// 写失败返回 error（调用方 emitComponentHooks 按 fail-open 处理）。
func (m *bridgeManager) emitEvent(compID string, def ComponentWorkerDef, event string, data any, blocking bool) (bridgeDecision, error) {
	wk, err := m.get(compID, def)
	if err != nil {
		return bridgeDecision{}, err
	}
	if !wk.framed {
		return bridgeDecision{}, fmt.Errorf("worker %s 未声明钩子，不该收到事件", def.Name)
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return bridgeDecision{}, err
	}
	id := wk.newID()
	var ch <-chan string
	var unwait func()
	if blocking {
		ch, unwait = wk.waitFrame(id)
		defer unwait()
	}
	if err := wk.writeFrame(`{"type":"event","id":"` + id + `","event":"` + event + `","data":` + string(payload) + `}`); err != nil {
		return bridgeDecision{}, err
	}
	if !blocking {
		return bridgeDecision{}, nil
	}
	timeout := time.Duration(def.HooksTimeoutMs) * time.Millisecond
	if timeout <= 0 || timeout > 30*time.Second {
		timeout = 5 * time.Second
	}
	select {
	case line := <-ch:
		var d bridgeDecision
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			return bridgeDecision{}, fmt.Errorf("decision 帧解析失败: %v", err)
		}
		return d, nil
	case <-time.After(timeout):
		return bridgeDecision{}, fmt.Errorf("决定等待超时（%s）", timeout)
	case <-wk.done:
		return bridgeDecision{}, fmt.Errorf("worker 已退出")
	}
}

// ComponentHookOutcome 是一次钩子事件在全部订阅组件上汇总后的结果（拦截型才有
// 实质内容；通知型恒为零值）。
type ComponentHookOutcome struct {
	Block             bool
	Reason            string
	Content           string // 最后一次改写后的正文（空=没人改）
	AdditionalContext string // 各组件附加上下文以空行拼接
}

// emitComponentHooks 把白名单事件按组件 id 序发给所有订阅它的启用组件。拦截型
// （prompt.submit）串行等决定并汇总：任一 block 即止（后面的组件不再跑）；content
// 改写依次应用——后一个组件的事件 data 里看到的是前一个改过的正文；附加上下文以
// 空行拼接。任何单组件失败（超时/进程死/解析错）fail-open：记日志按未拒绝处理，
// 组件故障不能拦住主流程。通知型逐个尽力送达，失败只记日志。
func (a *App) emitComponentHooks(event string, data map[string]any) ComponentHookOutcome {
	out := ComponentHookOutcome{}
	subs := a.Components.HookSubscribers(event)
	if len(subs) == 0 {
		return out
	}
	blocking := componentHookBlocking[event]
	for _, sub := range subs {
		d, err := a.Bridge.emitEvent(sub.CompID, sub.Def, event, data, blocking)
		if err != nil {
			log.Printf("[c/%s/%s] 钩子 %s 失败（按放行处理）: %v", sub.CompID, sub.Def.Name, event, err)
			continue
		}
		if !blocking {
			continue
		}
		if d.Block {
			out.Block = true
			out.Reason = d.Reason
			log.Printf("[c/%s/%s] 钩子 %s 拒绝消息: %s", sub.CompID, sub.Def.Name, event, d.Reason)
			return out
		}
		if d.Content != "" {
			out.Content = d.Content
			data["content"] = d.Content // 改写链：后续组件看到的是改过的正文
		}
		if d.AdditionalContext != "" {
			if out.AdditionalContext != "" {
				out.AdditionalContext += "\n\n"
			}
			out.AdditionalContext += d.AdditionalContext
		}
	}
	return out
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	return strings.Trim(string(b), `"`)
}
