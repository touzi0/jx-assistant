package main

// 语音组件宿主半边（voiceapi.go）的测试（朗读 = Edge readaloud 通道）：
//   - 朗读协议：httptest 起假微软 WebSocket 服务器（gobwas 升级握手），验证
//     上行帧形态（speech.config / SSML）、下行二进制帧拼装与 turn.end 收尾；
//   - 音色清单（voices/list HTTP mock）：过滤/排序/中文名/缓存。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// newVoiceTestApp 建一个带启用中 voice 组件的 App（组件目录用临时目录拼）。
func newVoiceTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	compDir := filepath.Join(dir, "组件")
	if err := os.MkdirAll(filepath.Join(compDir, "voice"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"id":"voice","name":"语音","version":"0","description":"t","client":{"js":"client.js"}}`
	if err := os.WriteFile(filepath.Join(compDir, "voice", "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return &App{
		Cfg:        &Config{},
		Components: InitComponents(compDir, filepath.Join(dir, "components.json")),
	}
}

func doJSON(t *testing.T, h http.HandlerFunc, method, target string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	w := httptest.NewRecorder()
	h(w, req)
	return w
}

// newEdgeTTSMock 起一个假 Edge 朗读 WebSocket 服务器：记录上行文本帧，
// 回两块二进制音频 + turn.end。返回服务器与取帧函数。鉴权参数兼容两代协议
//（v1 TrustedClientToken / v2 Ocp-Apim-Subscription-Key）。
func newEdgeTTSMock(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	frames := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("TrustedClientToken") != edgeToken &&
			r.URL.Query().Get("Ocp-Apim-Subscription-Key") != edgeToken {
			w.WriteHeader(403)
			return
		}
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			return
		}
		defer conn.Close()
		// 读两条上行文本帧（speech.config + ssml）
		for i := 0; i < 2; i++ {
			data, err := wsutil.ReadClientText(conn)
			if err != nil {
				return
			}
			mu.Lock()
			frames = append(frames, string(data))
			mu.Unlock()
		}
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		// 下行：turn.start → 两块音频（2 字节大端头长 + 头 + mp3 数据）→ turn.end
		_ = wsutil.WriteServerText(conn, []byte("X-RequestId:1\r\nPath:turn.start\r\n\r\n"))
		frame := func(payload string) []byte {
			head := "X-RequestId:1\r\nPath:audio\r\nContent-Type:audio/mpeg\r\n\r\n"
			b := make([]byte, 2+len(head)+len(payload))
			b[0] = byte(len(head) >> 8)
			b[1] = byte(len(head))
			copy(b[2:], head)
			copy(b[2+len(head):], payload)
			return b
		}
		_ = wsutil.WriteServerMessage(conn, ws.OpBinary, frame("\xff\xfbPART1"))
		_ = wsutil.WriteServerMessage(conn, ws.OpBinary, frame("-PART2"))
		_ = wsutil.WriteServerText(conn, []byte("X-RequestId:1\r\nPath:turn.end\r\n\r\n"))
	}))
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, len(frames))
		copy(out, frames)
		return out
	}
}

func TestVoiceTTS(t *testing.T) {
	srv, getFrames := newEdgeTTSMock(t)
	defer srv.Close()
	mock := func() string { return "ws://" + srv.Listener.Addr().String() + "/edge/v1" }
	oldFB, old := edgeTTSBaseFallback, edgeTTSBase
	edgeTTSBaseFallback, edgeTTSBase = mock, mock // 主/后备都指向 mock：任一通道命中
	defer func() { edgeTTSBaseFallback, edgeTTSBase = oldFB, old }()

	a := newVoiceTestApp(t) // 无需任何配置
	w := doJSON(t, a.handleVoiceTTS, "POST", "/api/voice/tts",
		strings.NewReader(`{"text":"含<标签>&引号\"的文本","rate":1.3,"voice":""}`))
	if w.Code != 200 {
		t.Fatalf("应 200，得 %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "audio/mpeg" {
		t.Fatalf("Content-Type 应为 audio/mpeg: %s", ct)
	}
	if got := w.Body.String(); got != "\xff\xfbPART1-PART2" {
		t.Fatalf("音频块应按序拼接，得 %q", got)
	}
	frames := getFrames()
	if len(frames) != 2 {
		t.Fatalf("应上行两帧，得 %d", len(frames))
	}
	if !strings.Contains(frames[0], "Path:speech.config") || !strings.Contains(frames[0], edgeTTSClause) {
		t.Fatalf("speech.config 帧不对: %s", frames[0])
	}
	ssml := frames[1]
	if !strings.Contains(ssml, "Path:ssml") || !strings.Contains(ssml, "zh-CN-XiaoxiaoNeural") {
		t.Fatalf("ssml 帧不对: %s", ssml)
	}
	if !strings.Contains(ssml, "rate='+30%'") {
		t.Fatalf("语速 1.3 应折成 +30%%: %s", ssml)
	}
	if !strings.Contains(ssml, "&lt;标签&gt;") || !strings.Contains(ssml, "&amp;") || !strings.Contains(ssml, "&quot;") {
		t.Fatalf("SSML 未正确转义: %s", ssml)
	}

	// 指定音色 + 倍率钳制（5 → +200%）
	w2 := doJSON(t, a.handleVoiceTTS, "POST", "/api/voice/tts",
		strings.NewReader(`{"text":"hello","rate":5,"voice":"en-US-GuyNeural"}`))
	if w2.Code != 200 {
		t.Fatalf("应 200，得 %d", w2.Code)
	}
	frames = getFrames()
	if len(frames) != 4 {
		t.Fatalf("第二轮应上行两帧，得 %d", len(frames))
	}
	ssml = frames[3]
	if !strings.Contains(ssml, "en-US-GuyNeural") || !strings.Contains(ssml, "rate='+200%'") ||
		!strings.Contains(ssml, "xml:lang='en-US'") {
		t.Fatalf("音色/语言/倍率钳制不对: %s", ssml)
	}
}

func TestVoiceTTSErrors(t *testing.T) {
	a := newVoiceTestApp(t)
	// 空 text / 非法 JSON
	if w := doJSON(t, a.handleVoiceTTS, "POST", "/api/voice/tts", strings.NewReader(`{"text":"  "}`)); w.Code != 400 {
		t.Fatalf("空 text 应 400，得 %d", w.Code)
	}
	if w := doJSON(t, a.handleVoiceTTS, "POST", "/api/voice/tts", strings.NewReader("xx")); w.Code != 400 {
		t.Fatalf("非法 JSON 应 400，得 %d", w.Code)
	}
	// 端点不可达：一个字节都没下发过，失败如实回 502 JSON（旧版不写错误码、
	// 回 200 空体——前端把 0 字节 blob 当成功音频去播，播放失败即整条朗读链
	// 被砍，是「读几句就停」的放大器）——两代通道全挂
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
	}))
	defer srv.Close()
	mock := func() string { return "ws://" + srv.Listener.Addr().String() + "/edge/v1" }
	oldFB, old := edgeTTSBaseFallback, edgeTTSBase
	edgeTTSBaseFallback, edgeTTSBase = mock, mock
	defer func() { edgeTTSBaseFallback, edgeTTSBase = oldFB, old }()
	w := doJSON(t, a.handleVoiceTTS, "POST", "/api/voice/tts", strings.NewReader(`{"text":"hi"}`))
	if w.Code != 502 || !strings.Contains(w.Body.String(), "朗读服务暂时不可用") {
		t.Fatalf("未下发字节的失败应回 502 带错误信息，得 %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("错误响应应为 JSON，得 %s", ct)
	}
}

func TestVoiceVoices(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.URL.Path != "/voices/list" {
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("User-Agent") != edgeUA {
			w.WriteHeader(403)
			return
		}
		_, _ = w.Write([]byte(`[
			{"ShortName":"zh-CN-YunxiNeural","Gender":"Male","Locale":"zh-CN","Status":"GA"},
			{"ShortName":"zh-CN-XiaoxiaoNeural","Gender":"Female","Locale":"zh-CN","Status":"GA"},
			{"ShortName":"en-US-AriaNeural","Gender":"Female","Locale":"en-US","Status":"GA"},
			{"zh-HK-HiuMaanNeural":"x"},
			{"ShortName":"zh-TW-HsiaoChenNeural","Gender":"Female","Locale":"zh-TW","Status":"GA"},
			{"ShortName":"zh-CN-PreviewVoiceNeural","Gender":"Female","Locale":"zh-CN","Status":"Preview"}
		]`))
	}))
	defer srv.Close()
	mock := func() string { return srv.URL + "/voices/list" }
	oldFB, old := edgeVoicesBaseFallback, edgeVoicesBase
	edgeVoicesBaseFallback, edgeVoicesBase = mock, mock
	defer func() { edgeVoicesBaseFallback, edgeVoicesBase = oldFB, old }()

	a := newVoiceTestApp(t)
	w := doJSON(t, a.handleVoiceVoices, "GET", "/api/voice/voices", nil)
	if w.Code != 200 {
		t.Fatalf("应 200，得 %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Voices []voiceView `json:"voices"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	// 预览音色滤掉、坏条目跳过：4 个；zh-CN 最前（同区按名称 Xiaoxiao < Yunxi）
	if len(out.Voices) != 4 {
		t.Fatalf("应 4 个音色，得 %d: %v", len(out.Voices), out.Voices)
	}
	want := []string{"zh-CN-XiaoxiaoNeural", "zh-CN-YunxiNeural", "zh-TW-HsiaoChenNeural", "en-US-AriaNeural"}
	for i, name := range want {
		if out.Voices[i].Name != name {
			t.Fatalf("排序第 %d 位应为 %s，得 %s", i, name, out.Voices[i].Name)
		}
	}
	if out.Voices[0].Display != "晓晓（zh-CN · 女）" {
		t.Fatalf("中文音色应显示中文名: %s", out.Voices[0].Display)
	}
	if out.Voices[3].Display != "Aria（en-US · 女）" {
		t.Fatalf("非中文音色取末段短名: %s", out.Voices[3].Display)
	}
	// 缓存：第二次不再打上游
	w2 := doJSON(t, a.handleVoiceVoices, "GET", "/api/voice/voices", nil)
	if w2.Code != 200 || atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("应命中缓存（hits=%d）", atomic.LoadInt32(&hits))
	}
}
