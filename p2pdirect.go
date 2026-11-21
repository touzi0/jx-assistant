package main

// P2P 直连（WebRTC DataChannel，pion 应答端）——中继之上的自动加速通道。
//
// 背景：中继（remoteaccess.go）永远可用但每个字节过两跳 210；两端网络条件允许时
// （同一局域网 / 双公网 / STUN 可达）WebRTC 打洞可直连，DTLS 端到端加密、210 卸载。
//
// 信令复用中继本身：手机端外壳（remote-boot.js）起步走中继（保底可用），随后把
// SDP offer 经 POST /api/webrtc-offer 发来（该请求被 fetch 劫持封 AES 走中继隧道，
// 密文过 210）；本端应答 answer 原路返回。DataChannel 打通后第一帧 auth.pairKey
// 鉴权（常数时间比较，密钥不匹配即断），随后 req/head/chunk/end/err/abort 帧与
// 中继内层协议完全一致——桥接核心 bridgeCore 两边共用。打不通/中途断开：手机端
// 自动留在（回落）中继，一次页面加载只试一轮。
//
// 安全边界：SDP 经中继是端到端密文；DataChannel 由 WebRTC 强制 DTLS 加密；auth
// 帧挡住拿到 offer 端点但不知道配对密钥的调用方（LAN 内直连本机服务者同样过闸）。
// mDNS 候选关闭（SDP 里带真实内网 IP，反正是密文且对端已持配对密钥），换确定性。
//
// 帧协议（手机 → 家）：
//   {t:'auth', pairKey} → {t:'authok'} / {t:'authfail'}（并断开）
//   {t:'req', id, method, path, headers, body(base64)}（仅 /api/ 前缀）
//   {t:'abort', id}
// （家 → 手机）：{t:'authok'|'authfail'} 与 bridgeCore 的 head/chunk/end/err，
//   明文 JSON 文本帧（DTLS 已加密）。

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

const (
	dcSTUN1        = "stun:stun.miwifi.com:3478" // 国内可达优先（与旧配对页同源）
	dcSTUN2        = "stun:stun.qq.com:3478"
	dcMaxBody      = 256 << 10        // 单条 req 帧 body 上限（DataChannel 单消息安全区），更大让手机端走中继
	dcMaxSessions  = 6                // 并发直连会话上限（多标签页/多手机），满了走中继
	dcGatherWait   = 3 * time.Second  // ICE 收集上限：到点带已有候选应答（局域网 host 候选早已够）
	dcIdleClose    = 15 * time.Minute // 空闲回收：页面关掉后 PeerConnection 自动静默
	dcJanitorEvery = time.Minute
)

var (
	dcMu       sync.Mutex
	dcSessions = map[*dcSession]bool{}
	dcJanitor  sync.Once
	dcClient   = &http.Client{} // 无总超时：长流式响应靠 abort 帧与请求 ctx 收口（与中继桥一致）
)

// dcSession 一条 DataChannel 直连会话（一个手机页面一个）。
type dcSession struct {
	app *App
	pc  *webrtc.PeerConnection

	mu      sync.Mutex
	cancels map[string]context.CancelFunc // 帧id → 进行中桥接请求的中止句柄
	authed  bool
	closed  bool
	last    atomic.Int64 // unix ms，任意帧刷新；空闲回收依据
}

// handleWebRTCOffer 手机端直连升级的信令入口：{sdp} 进、{answer} 出。
// 可经中继隧道（远程手机，密文）也可本机/局域网直调（同样要过 auth 帧）。
func (a *App) handleWebRTCOffer(w http.ResponseWriter, r *http.Request) {
	if a.Cfg.RemoteAccess.PairKey == "" {
		writeErr(w, http.StatusForbidden, "远程接入未配置（无配对密钥）")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(body) == 0 {
		writeErr(w, http.StatusBadRequest, "读取 offer 失败")
		return
	}
	var req struct {
		SDP string `json:"sdp"`
	}
	if json.Unmarshal(body, &req) != nil || req.SDP == "" {
		writeErr(w, http.StatusBadRequest, "bad offer")
		return
	}
	dcMu.Lock()
	n := len(dcSessions)
	dcMu.Unlock()
	if n >= dcMaxSessions {
		writeErr(w, http.StatusTooManyRequests, "直连会话已满，请走中继")
		return
	}
	answer, _, err := a.newDCSession(req.SDP)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "建立直连失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"answer": answer})
}

// newDCSession 建立 PeerConnection、注册会话并返回完整 answer SDP（含已收集候选）。
func (a *App) newDCSession(offerSDP string) (string, *dcSession, error) {
	se := webrtc.SettingEngine{}
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled) // 候选带真实 IP（SDP 走密文中继）
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: []string{dcSTUN1, dcSTUN2}}},
	})
	if err != nil {
		return "", nil, err
	}
	s := &dcSession{app: a, pc: pc, cancels: map[string]context.CancelFunc{}}
	s.last.Store(time.Now().UnixMilli())

	pc.OnDataChannel(func(d *webrtc.DataChannel) {
		s.last.Store(time.Now().UnixMilli())
		d.OnMessage(func(m webrtc.DataChannelMessage) {
			s.last.Store(time.Now().UnixMilli())
			if !m.IsString {
				return // 帧协议是 JSON 文本
			}
			s.onFrame(d, string(m.Data))
		})
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		if st == webrtc.PeerConnectionStateFailed || st == webrtc.PeerConnectionStateDisconnected || st == webrtc.PeerConnectionStateClosed {
			s.shutdown()
		}
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, SDP: offerSDP,
	}); err != nil {
		_ = pc.Close()
		return "", nil, err
	}
	ans, err := pc.CreateAnswer(nil)
	if err != nil {
		_ = pc.Close()
		return "", nil, err
	}
	if err := pc.SetLocalDescription(ans); err != nil {
		_ = pc.Close()
		return "", nil, err
	}
	// 等候选收集（上限 dcGatherWait；局域网 host 候选即时可得，跨网 srflx 通常 <1.5s）
	gathered := webrtc.GatheringCompletePromise(pc)
	select {
	case <-gathered:
	case <-time.After(dcGatherWait):
	}
	dcMu.Lock()
	dcSessions[s] = true
	dcMu.Unlock()
	dcJanitor.Do(func() {
		go func() {
			for range time.Tick(dcJanitorEvery) {
				dcMu.Lock()
				for s := range dcSessions {
					if time.Since(time.UnixMilli(s.last.Load())) > dcIdleClose {
						delete(dcSessions, s)
						go s.shutdown()
					}
				}
				dcMu.Unlock()
			}
		}()
	})
	log.Printf("[远程] 直连会话已建立（当前 %d 条）", len(dcSessions))
	return pc.LocalDescription().SDP, s, nil
}

// dcInFrame 入站帧的宽松视图（各帧型字段并集，多余字段忽略）。
type dcInFrame struct {
	T       string            `json:"t"`
	ID      string            `json:"id"`
	PairKey string            `json:"pairKey"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

func (s *dcSession) onFrame(d *webrtc.DataChannel, data string) {
	var f dcInFrame
	if json.Unmarshal([]byte(data), &f) != nil || f.T == "" {
		return
	}
	s.mu.Lock()
	authed := s.authed
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return
	}
	if !authed {
		if f.T == "auth" && subtle.ConstantTimeCompare(
			[]byte(f.PairKey), []byte(s.app.Cfg.RemoteAccess.PairKey)) == 1 {
			s.mu.Lock()
			s.authed = true
			s.mu.Unlock()
			s.send(d, map[string]any{"t": "authok"})
			log.Printf("[远程] 直连通道鉴权通过")
		} else {
			s.send(d, map[string]any{"t": "authfail"})
			// 先让 authfail 刷出 SCTP 再断：立即 shutdown 会在帧落线前掐掉传输
			go func() { time.Sleep(200 * time.Millisecond); s.shutdown() }()
		}
		return
	}
	switch f.T {
	case "req":
		// 路径白名单与中继侧（remoteaccess.go）同口径：/api/ 常规接口 + /sites、
		// /s/（企业小程序内嵌代理）。2026-11-04 事故：平台代载上线时只扩了中继侧
		// 白名单，直连侧漏改——手机 P2P 直连（默认更快路径）打开平台页必 403
		// 「path not allowed」，错误文案又指向「家端不在线」，极具误导性。
		if !relayPathAllowed(f.Path) {
			s.send(d, map[string]any{"t": "err", "id": f.ID, "msg": "[403] path not allowed"})
			return
		}
		if len(f.Body) > dcMaxBody*2 { // base64 膨胀两倍计
			s.send(d, map[string]any{"t": "err", "id": f.ID, "msg": "[413] request too large (direct)"})
			return
		}
		go func() { // 每请求一协程；panic 不外溢
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[远程] 直连 %s 桥接协程异常（已恢复）: %v", f.ID, r)
				}
			}()
			s.bridge(d, relayReqFrame{
				T: "req", ID: f.ID, Method: f.Method, Path: f.Path, Headers: f.Headers, Body: f.Body,
			})
		}()
	case "abort":
		s.mu.Lock()
		if cancel, ok := s.cancels[f.ID]; ok {
			cancel()
			delete(s.cancels, f.ID)
		}
		s.mu.Unlock()
	}
}

// bridge 把一条 req 帧桥到本机回环 HTTP，响应明文帧直写 DataChannel（DTLS 已加密）。
func (s *dcSession) bridge(d *webrtc.DataChannel, fr relayReqFrame) {
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		cancel()
		return
	}
	s.cancels[fr.ID] = cancel
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		delete(s.cancels, fr.ID)
		s.mu.Unlock()
	}()
	bridgeCore(ctx, dcClient, s.app.listenPort, fr, func(v map[string]any) bool {
		return s.send(d, v)
	})
}

// send 写一帧 JSON 文本；失败视作链路已死，整体收摊。
func (s *dcSession) send(d *webrtc.DataChannel, v map[string]any) bool {
	data, err := json.Marshal(v)
	if err != nil {
		return false
	}
	if err := d.SendText(string(data)); err != nil {
		go s.shutdown()
		return false
	}
	return true
}

// shutdown 幂等收摊：注销会话、掐断在途桥接、关 PeerConnection。
func (s *dcSession) shutdown() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	cancels := make([]context.CancelFunc, 0, len(s.cancels))
	for _, c := range s.cancels {
		cancels = append(cancels, c)
	}
	s.cancels = map[string]context.CancelFunc{}
	s.mu.Unlock()
	for _, c := range cancels {
		c()
	}
	dcMu.Lock()
	delete(dcSessions, s)
	dcMu.Unlock()
	_ = s.pc.Close()
}
