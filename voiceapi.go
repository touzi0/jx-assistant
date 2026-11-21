package main

// 语音组件（组件/voice）的宿主半边。
//
// 分工（2026-10-07 晚定稿，两轮反馈后）：
//   - 朗读（TTS）+ 音色清单：微软 Edge「大声朗读」同款接口（speech.platform.
//     bing.com 的 readaloud 通道，无密钥、无需任何配置），与 Edge/WebView2
//     浏览器内置朗读同一套服务——由后端调用，任何浏览器/远程手机端行为一致
//     （浏览器 speechSynthesis 在部分手机浏览器上缺失，是旧版远程朗读不可用
//     的根源）。参考实现：聊天服务 chat-server/server.js 的纯 Node 版（手机端
//     实测可用），本次逐协议移植为 Go（gobwas/ws）。
//   - 听写（STT）：浏览器自带的 SpeechRecognition（WebView2/Edge 内置的微软
//     识别接口），在组件的浏览器半边实现，不经过本文件。曾试验"后端起无头
//     Edge 用假麦克风喂音频"（2026-10-07）：音频确实能进 getUserMedia（哔声/
//     WAV 音量实测有效），但 Chromium 的 SpeechRecognition 绑定真实默认输入
//     设备、不消费假设备（实测零转写，已知限制）——该路不通，结论记录于
//     语音后端化-研究与改动说明.md。
//
// 端点（组件关掉时一并 404，按 Components.IsEnabled("voice") 自查）：
//
//	GET  /api/voice/voices  Edge 神经音色清单（朗读音色下拉，进程内缓存 6 小时）
//	POST /api/voice/tts           文本 → mp3 音频流（Edge 朗读，边合成边下发，前端 blob 播放）
//
// 中继适配：朗读的 mp3 按帧流式下发；听写按「请求体一次性整包上送」分段 POST
//（远程外壳的 fetch 隧道本就没有上行流）——两者都是隧道天然支持的形态。

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

const (
	voiceComponentID  = "voice"
	speechDefaultLang = "zh-CN"
	// voiceTTSMaxRunes 单次朗读文本上限。
	voiceTTSMaxRunes = 30000
	// voiceVoicesTTL 音色清单缓存时长（目录极少变化）。
	voiceVoicesTTL = 6 * time.Hour
)

// ---------- Edge「大声朗读」通道（无密钥） ----------
//
// 协议与 chat-server/server.js 的 Node 实现逐项对齐（该实现已在手机端可用）：
// 握手 URL 带 TrustedClientToken + Sec-MS-GEC（5 分钟窗的 DRM 令牌）+ 版本与
// 连接号；握手头带 Origin（Edge 朗读扩展）/UA（Edge）/随机 muid Cookie，缺任一
// 会被 403。上行两条文本帧（speech.config 指定 mp3 输出 + SSML 合成请求），
// 下行二进制帧（前 2 字节大端头长度，其后为 MP3 数据）直到文本帧 Path:turn.end。

const (
	edgeToken     = "6A5AA1D4EAFF4E9FB37E23D68491D6F4"
	edgeChromium  = "143.0.3650.75" // 需跟随上游升级：版本过旧会被 403
	edgeOrigin    = "chrome-extension://jdiccldimpdaibmpdkjnbmckianbfold"
	edgeUA        = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36 Edg/143.0.0.0"
	edgeTTSClause = "audio-24khz-48kbitrate-mono-mp3" // speech.config 指定的输出格式
)

// 2026-10-10 起微软把朗读服务从 speech.platform.bing.com 迁到
// api.msedgeservices.com（分区域灰度，旧域名握手开始 403，见
// github.com/rany2/edge-tts issue #490）：域名与密钥参数名变了
//（TrustedClientToken → Ocp-Apim-Subscription-Key），Sec-MS-GEC 令牌
// 算法未变（v1 同款照用，实测 2026-10-13）。主走新域名、旧域名后备
//（灰度未完成的区域旧域名仍可用）。端点可注入（测试换 httptest 服务器）。
var (
	edgeTTSBase = func() string {
		return "wss://api.msedgeservices.com/tts/cognitiveservices/websocket/v1"
	}
	edgeVoicesBase = func() string {
		return "https://api.msedgeservices.com/tts/cognitiveservices/voices/list"
	}
	edgeTTSBaseFallback = func() string {
		return "wss://speech.platform.bing.com/consumer/speech/synthesize/readaloud/edge/v1"
	}
	edgeVoicesBaseFallback = func() string {
		return "https://speech.platform.bing.com/consumer/speech/synthesize/readaloud/voices/list"
	}
)

// voiceHTTPClient 访问音色清单的普通 HTTP 客户端。
var voiceHTTPClient = newCompanyHTTPClient(60 * time.Second)

// edgeGecToken Sec-MS-GEC 鉴权令牌（对齐 edge-tts 7.2.8 的算法）：
// (unix秒+11644473600) 向下取整 5 分钟 → 转 100ns ticks →
// SHA256(ticks数字串 + TrustedClientToken) 大写 hex。
func edgeGecToken() string {
	secs := time.Now().Unix() + 11644473600 // Unix → Windows 纪元
	secs -= secs % 300
	ticks := secs * 10000000
	sum := sha256.Sum256([]byte(strconv.FormatInt(ticks, 10) + edgeToken))
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// randHex 生成 n 个十六进制字符的随机串（连接号/请求号用）。
func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

// edgeHandshakeHeader 与 Edge 朗读扩展一致的握手头（缺 Origin/UA/Cookie 会 403）。
func edgeHandshakeHeader() http.Header {
	h := http.Header{}
	h.Set("Pragma", "no-cache")
	h.Set("Cache-Control", "no-cache")
	h.Set("Origin", edgeOrigin)
	h.Set("User-Agent", edgeUA)
	h.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	h.Set("Accept-Language", "en-US,en;q=0.9")
	h.Set("Cookie", "muid="+strings.ToUpper(randHex(32))+";")
	return h
}

// synthesizeEdge 连接 Edge 朗读通道合成一段文本：每收到一块 MP3 就经 onAudio
// 下发（边合成边流），收到 turn.end 返回 nil。新域名优先、旧域名后备——握手
// 失败（403 等）自动换下一个；已出过音频后的中断不再换道（音频已在路上）。
func synthesizeEdge(ctx context.Context, text, voice, rateAttr string, onAudio func([]byte) error) error {
	gec := edgeGecToken()
	var lastErr error
	for _, at := range []struct{ name, url string }{
		{
			name: "新域名",
			url: edgeTTSBase() + "?Ocp-Apim-Subscription-Key=" + edgeToken +
				"&Sec-MS-GEC=" + gec + "&Sec-MS-GEC-Version=1-" + edgeChromium +
				"&ConnectionId=" + randHex(32),
		},
		{
			name: "旧域名",
			url: edgeTTSBaseFallback() + "?TrustedClientToken=" + edgeToken +
				"&Sec-MS-GEC=" + gec + "&Sec-MS-GEC-Version=1-" + edgeChromium +
				"&ConnectionId=" + randHex(32),
		},
	} {
		err := synthesizeEdgeOnce(ctx, at.url, edgeHandshakeHeader(), text, voice, rateAttr, onAudio)
		if err == nil {
			return nil
		}
		log.Printf("[语音] 朗读通道 %s 失败: %v", at.name, err)
		lastErr = err
	}
	return lastErr
}

// synthesizeEdgeOnce 单通道一次完整合成（协议两代同构：上行 speech.config +
// SSML 两条文本帧，下行二进制帧剥头即 MP3，turn.end 收尾）。
func synthesizeEdgeOnce(ctx context.Context, connURL string, header http.Header,
	text, voice, rateAttr string, onAudio func([]byte) error) error {
	dialer := ws.Dialer{Timeout: 15 * time.Second, Header: ws.HandshakeHeaderHTTP(header)}
	conn, _, _, err := dialer.Dial(ctx, connURL)
	if err != nil {
		return fmt.Errorf("连接朗读服务失败: %w", err)
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(60 * time.Second)
	}
	_ = conn.SetDeadline(deadline)

	ts := time.Now().Format(time.RFC3339Nano)
	// 1) speech.config：mp3 输出、不要元数据
	cfg := "X-Timestamp:" + ts + "\r\nContent-Type:application/json; charset=utf-8\r\nPath:speech.config\r\n\r\n" +
		`{"context":{"synthesis":{"audio":{"metadataoptions":{"sentenceBoundaryEnabled":"false","wordBoundaryEnabled":"false"},` +
		`"outputFormat":"` + edgeTTSClause + `"}}}}`
	if err := wsutil.WriteClientText(conn, []byte(cfg)); err != nil {
		return fmt.Errorf("发送配置失败: %w", err)
	}
	// 2) SSML 合成请求
	reqFrame := "X-RequestId:" + randHex(32) + "\r\nContent-Type:application/ssml+xml\r\nX-Timestamp:" + ts +
		"\r\nPath:ssml\r\n\r\n" + buildSpeechSSML(text, voice, rateAttr)
	if err := wsutil.WriteClientText(conn, []byte(reqFrame)); err != nil {
		return fmt.Errorf("发送合成请求失败: %w", err)
	}

	// 读循环：文本帧找 turn.end，二进制帧剥 2 字节大端头长后是 MP3 数据
	rd := wsutil.NewReader(conn, ws.StateClientSide)
	got := false
	for {
		hdr, err := rd.NextFrame()
		if err != nil {
			if got {
				return nil // 已出过音频后连接收尾异常：按完成处理（音频已在路上）
			}
			return fmt.Errorf("朗读服务连接中断: %w", err)
		}
		if hdr.OpCode == ws.OpClose {
			if got {
				return nil
			}
			return fmt.Errorf("朗读服务提前关闭连接")
		}
		payload, err := io.ReadAll(rd)
		if err != nil {
			return fmt.Errorf("读取朗读数据失败: %w", err)
		}
		if hdr.OpCode == ws.OpText {
			if bytes.Contains(payload, []byte("Path:turn.end")) {
				return nil
			}
			continue // turn.start / response 消息：忽略
		}
		if hdr.OpCode == ws.OpBinary && len(payload) > 2 {
			headerLen := int(payload[0])<<8 | int(payload[1])
			if headerLen+2 <= len(payload) {
				data := payload[2+headerLen:]
				if len(data) == 0 {
					continue
				}
				got = true
				if err := onAudio(data); err != nil {
					return err // 下行写失败（客户端已断）：立即收
				}
			}
		}
	}
}

// buildSpeechSSML 组 Edge 朗读的 SSML（属性用单引号，与参考实现一致）。
func buildSpeechSSML(text, voice, rateAttr string) string {
	return "<speak version='1.0' xmlns='http://www.w3.org/2001/10/synthesis' xml:lang='" + voiceLangOf(voice) +
		"'><voice name='" + voice + "'><prosody pitch='+0Hz' rate='" + rateAttr + "' volume='+0%'>" +
		speechXMLEscape(text) + "</prosody></voice></speak>"
}

// speechXMLEscape SSML 文本段转义（控制字符丢弃，避免打爆解析器）。
func speechXMLEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&apos;")
		default:
			if r < 0x20 && r != '\n' && r != '\t' && r != '\r' {
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ---------- HTTP 处理器 ----------

// voiceOn 组件开关自查（组件关掉时语音端点一并消失，口径同 todo/git 面板）。
// 生效口径含会话级覆盖：某会话把全局关着的语音打开时，端点要能服务
// （请求不带会话，按「全局或任一会话要它」放行；关掉的对话里按钮已热卸载）。
func (a *App) voiceOn(w http.ResponseWriter) bool {
	if a.Components == nil || !a.cxoComponentLive(voiceComponentID) {
		writeErr(w, 404, "组件已停用或不存在")
		return false
	}
	return true
}

// handleVoiceTTS 文本 → mp3 音频流（Edge 朗读通道，无需任何配置）。
// body: {text, rate(0.5–3，1=原速), voice(Edge 音色 ShortName，空=按语言挑默认)}。
func (a *App) handleVoiceTTS(w http.ResponseWriter, r *http.Request) {
	if !a.voiceOn(w) {
		return
	}
	var body struct {
		Text  string  `json:"text"`
		Rate  float64 `json:"rate"`
		Voice string  `json:"voice"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体不是合法 JSON")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, "text 不能为空")
		return
	}
	if len([]rune(text)) > voiceTTSMaxRunes {
		writeErr(w, 400, fmt.Sprintf("文本过长（上限 %d 字）", voiceTTSMaxRunes))
		return
	}
	voice := strings.TrimSpace(body.Voice)
	if voice == "" {
		voice = defaultSpeechVoice(speechDefaultLang)
	}
	rate := body.Rate
	if rate < 0.5 {
		rate = 0.5
	}
	if rate > 3 {
		rate = 3
	}
	rateAttr := fmt.Sprintf("%+d%%", int((rate-1)*100))

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "no-store")
	flusher, _ := w.(http.Flusher)
	wrote := false
	err := synthesizeEdge(ctx, text, voice, rateAttr, func(chunk []byte) error {
		wrote = true
		if _, werr := w.Write(chunk); werr != nil {
			return werr
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	})
	if err != nil {
		log.Printf("[语音] 朗读失败: %v", err)
		// 一个字节都没下发过（典型：微软对新域名限流 429、后备旧域名 403）：
		// 响应头还没真正发出，如实把错误码回报前端。旧版在这里什么都不写，
		// net/http 自动补 200 空体——前端把 0 字节 blob 当成功音频去播，播放
		// 失败即整条朗读链被砍（「读几句就停」的放大器）。已经发过音频的
		// 中途失败仍只能断流（头与部分数据已在路上，状态码改不了了）。
		if !wrote {
			writeErr(w, 502, "朗读服务暂时不可用："+err.Error())
		}
	}
}

// ---------- 音色清单（Edge 朗读通道，无密钥） ----------

// voiceView 下发给前端的音色项。
type voiceView struct {
	Name    string `json:"name"`    // ShortName（zh-CN-XiaoxiaoNeural）
	Display string `json:"display"` // 下拉显示（晓晓（zh-CN · 女））
	Locale  string `json:"locale"`
	Gender  string `json:"gender"`
}

type voiceCache struct {
	mu      sync.Mutex
	expires time.Time // 过期时刻（成功远期、失败 1 分钟）
	voices  []voiceView
	err     error
}

var voiceVoicesCache voiceCache

// handleVoiceVoices Edge 神经音色清单（与朗读同一套服务、同一批音色）：
// 只留 GA 神经音色、中文区排前、同区按名称排。成功缓存 6 小时，失败 1 分钟
// （避免设置页每开一次就打一次微软，也不会把瞬时故障钉死）。
func (a *App) handleVoiceVoices(w http.ResponseWriter, r *http.Request) {
	if !a.voiceOn(w) {
		return
	}
	voiceVoicesCache.mu.Lock()
	if time.Now().Before(voiceVoicesCache.expires) {
		voices, err := voiceVoicesCache.voices, voiceVoicesCache.err
		voiceVoicesCache.mu.Unlock()
		if err != nil {
			writeErr(w, 502, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"voices": voices})
		return
	}
	voiceVoicesCache.mu.Unlock()

	// 拉取 + 解析（失败立即重试一次：清单接口偶发 4xx/压缩体抽风，重试能自愈）
	var voices []voiceView
	var fetchErr error
	for attempt := 0; attempt < 2; attempt++ {
		voices, fetchErr = fetchEdgeVoices(r.Context())
		if fetchErr == nil {
			break
		}
		if attempt == 0 {
			log.Printf("[语音] 音色清单获取失败（将重试一次）: %v", fetchErr)
		}
	}
	if fetchErr != nil {
		log.Printf("[语音] 音色清单获取失败: %v", fetchErr)
		cacheVoiceList(nil, fetchErr, time.Minute)
		writeErr(w, 502, fetchErr.Error())
		return
	}
	cacheVoiceList(voices, nil, voiceVoicesTTL)
	writeJSON(w, 200, map[string]any{"voices": voices})
}

// fetchEdgeVoices 拉取并过滤 Edge 音色清单（GA 神经音色；新域名优先、旧域名后备）。
func fetchEdgeVoices(ctx context.Context) ([]voiceView, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	gec := edgeGecToken()
	var lastErr error
	for _, at := range []struct{ name, url string }{
		{"新域名", edgeVoicesBase() + "?Ocp-Apim-Subscription-Key=" + edgeToken +
			"&Sec-MS-GEC=" + gec + "&Sec-MS-GEC-Version=1-" + edgeChromium},
		{"旧域名", edgeVoicesBaseFallback() + "?TrustedClientToken=" + edgeToken +
			"&Sec-MS-GEC=" + gec + "&Sec-MS-GEC-Version=1-" + edgeChromium},
	} {
		voices, err := fetchEdgeVoicesOnce(ctx, at.url)
		if err == nil {
			return voices, nil
		}
		log.Printf("[语音] 音色清单通道 %s 失败: %v", at.name, err)
		lastErr = err
	}
	return nil, lastErr
}

// fetchEdgeVoicesOnce 单通道拉取并解析音色清单。
func fetchEdgeVoicesOnce(ctx context.Context, listURL string) ([]voiceView, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return nil, fmt.Errorf("构造音色清单请求失败: %w", err)
	}
	// 同一套握手头里取 UA/Cookie 等；Origin 是 WebSocket 专用（普通 HTTPS 请求带
	// 上会被拒），Accept-Encoding 也不能手动带——手动声明 br/zstd 时 Go 传输层
	// 不再做透明 gzip 解压，上游一旦回压缩体就是乱码解析失败
	for k, vv := range edgeHandshakeHeader() {
		if k == "Origin" || k == "Accept-Encoding" {
			continue
		}
		for i, v := range vv {
			if i == 0 {
				req.Header.Set(k, v)
			} else {
				req.Header.Add(k, v)
			}
		}
	}

	resp, err := voiceHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("音色服务不可达: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("音色服务返回 HTTP %d%s", resp.StatusCode, azureErrTail(resp.Body))
	}
	var raw []struct {
		ShortName string `json:"ShortName"`
		Gender    string `json:"Gender"`
		Locale    string `json:"Locale"`
		Status    string `json:"Status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("音色清单解析失败: %w", err)
	}
	voices := []voiceView{}
	for _, v := range raw {
		if v.ShortName == "" || !strings.HasSuffix(v.ShortName, "Neural") {
			continue // 只给神经音色
		}
		if v.Status != "" && v.Status != "GA" {
			continue // 预览态音色不进下拉
		}
		voices = append(voices, voiceView{
			Name:    v.ShortName,
			Display: edgeVoiceDisplay(v.ShortName, v.Locale, v.Gender),
			Locale:  v.Locale,
			Gender:  v.Gender,
		})
	}
	sort.Slice(voices, func(i, j int) bool { return speechVoiceLess(voices[i], voices[j]) })
	return voices, nil
}

// edgeVoiceNamesZH 常见中文音色给中文名（目录接口的 Name 字段是英文全称，
// 不如下划线前的短名可读）。
var edgeVoiceNamesZH = map[string]string{
	"zh-CN-XiaoxiaoNeural": "晓晓", "zh-CN-XiaoyiNeural": "晓伊", "zh-CN-YunjianNeural": "云健",
	"zh-CN-YunxiNeural": "云希", "zh-CN-YunxiaNeural": "云夏", "zh-CN-YunyangNeural": "云扬",
	"zh-TW-HsiaoChenNeural": "曉臻", "zh-TW-YunJheNeural": "雲哲",
	"zh-HK-HiuMaanNeural": "曉曼", "zh-HK-WanLungNeural": "雲龍",
}

// edgeVoiceDisplay 拼下拉显示名：zh-CN-XiaoxiaoNeural → 晓晓（zh-CN · 女）；
// 未收录的取去掉 Neural 后缀的末段短名（en-US-AriaNeural → Aria）。
func edgeVoiceDisplay(shortName, locale, gender string) string {
	name := shortName
	if zh, ok := edgeVoiceNamesZH[shortName]; ok {
		name = zh
	} else {
		trimmed := strings.TrimSuffix(shortName, "Neural")
		if parts := strings.Split(trimmed, "-"); len(parts) > 0 && parts[len(parts)-1] != "" {
			name = parts[len(parts)-1]
		}
	}
	return name + "（" + locale + " · " + speechGenderCN(gender) + "）"
}

// cacheVoiceList 落缓存：成功远期缓存，失败只缓存 1 分钟。
func cacheVoiceList(voices []voiceView, err error, ttl time.Duration) {
	voiceVoicesCache.mu.Lock()
	defer voiceVoicesCache.mu.Unlock()
	voiceVoicesCache.expires = time.Now().Add(ttl)
	voiceVoicesCache.voices, voiceVoicesCache.err = voices, err
}

// speechVoiceLess 中文区最前（zh-CN 先于其他 zh），随后英文区，再其余按区域。
func speechVoiceLess(a, b voiceView) bool {
	rank := func(v voiceView) int {
		switch {
		case v.Locale == "zh-CN":
			return 0
		case strings.HasPrefix(v.Locale, "zh"):
			return 1
		case strings.HasPrefix(v.Locale, "en"):
			return 2
		default:
			return 3
		}
	}
	if ra, rb := rank(a), rank(b); ra != rb {
		return ra < rb
	}
	if a.Locale != b.Locale {
		return a.Locale < b.Locale
	}
	return a.Name < b.Name
}

func speechGenderCN(g string) string {
	switch strings.ToLower(g) {
	case "female":
		return "女"
	case "male":
		return "男"
	}
	return g
}

// ---------- 默认音色 ----------

// defaultSpeechVoiceByLang 常用语言的默认神经音色（未指定音色时按语言挑；
// zh-CN 给晓晓——与旧版「自动优先中文」的听感对齐）。Edge 通道同款音色名。
var defaultSpeechVoiceByLang = map[string]string{
	"zh-CN": "zh-CN-XiaoxiaoNeural",
	"zh-TW": "zh-TW-HsiaoChenNeural",
	"zh-HK": "zh-HK-HiuMaanNeural",
	"en-US": "en-US-AriaNeural",
	"en-GB": "en-GB-SoniaNeural",
	"ja-JP": "ja-JP-NanamiNeural",
	"ko-KR": "ko-KR-SunHiNeural",
}

func defaultSpeechVoice(lang string) string {
	if v, ok := defaultSpeechVoiceByLang[lang]; ok {
		return v
	}
	if l, _, ok := strings.Cut(strings.ToLower(lang), "-"); ok && strings.HasPrefix(l, "zh") {
		return defaultSpeechVoiceByLang["zh-CN"]
	}
	return defaultSpeechVoiceByLang["en-US"]
}

// voiceLangOf 从音色名（zh-CN-XiaoxiaoNeural / en-US-AriaNeural）取语言前缀。
func voiceLangOf(voice string) string {
	parts := strings.Split(voice, "-")
	if len(parts) >= 2 {
		return parts[0] + "-" + parts[1]
	}
	return speechDefaultLang
}

// azureErrTail 读一段上游错误响应正文附到错误消息里（鉴权类问题的现场线索）。
func azureErrTail(body io.Reader) string {
	b, err := io.ReadAll(io.LimitReader(body, 2048))
	if err != nil || len(bytes.TrimSpace(b)) == 0 {
		return ""
	}
	return "：" + strings.TrimSpace(string(b))
}

// statusToProxy 上游返回码 → 我方对前端的返回码：鉴权类问题如实透传
// （401 密钥无效 / 403 权限或被拒 / 429 限流），其余按网关错误 502 归并。
func statusToProxy(status int) int {
	if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusTooManyRequests {
		return status
	}
	return http.StatusBadGateway
}
