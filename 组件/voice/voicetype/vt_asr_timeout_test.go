package main

import (
	"testing"
	"time"
)

// vtAsrTimeoutSec：语音时长 1:1 + 8s 缓冲，下限 15s，**不设上限**。
// 线上要求（2026-11-14 用户口径）：真实场景有 210s/270s 的超长语音，
// 钳上限会把长语音误判超时——超时必须与时长线性同步（300s 语音 → 308s）；
// 下限 15s 保证极短语音也等到服务/推理完成，超时后明确提示"识别超时，请重试"。
func TestVtAsrTimeoutSec(t *testing.T) {
	cases := []struct{ audioSec, want int }{
		{0, 15},    // 误触级
		{2, 15},    // 下限内
		{7, 15},    // 7+8=15 恰好下限
		{10, 18},   // 1:1 + 8
		{30, 38},
		{60, 68},
		{210, 218}, // 用户口中的真实超长语音
		{270, 278},
		{300, 308}, // 验证要求：300s 语音超时约 308s，不被提前打断
		{1000, 1008},
	}
	for _, c := range cases {
		if got := vtAsrTimeoutSec(c.audioSec); got != c.want {
			t.Errorf("vtAsrTimeoutSec(%d) = %d, want %d", c.audioSec, got, c.want)
		}
	}
}

// vtTranscribe 的 deadline 收口：服务未就绪且 deadline 已过时必须立即返回错误，
// 不得进入等待循环（这就是"识别中"永久悬挂的病灶位，2026-11-14 修复的验收点）。
func TestVtTranscribeDeadlineExpired(t *testing.T) {
	start := time.Now()
	_, err := vtTranscribe(nil, time.Now().Add(-time.Second))
	if err == nil {
		t.Fatal("期望返回错误（识别服务未就绪），得到 nil")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("deadline 已过仍等待了 %v，应立即返回", d)
	}
}
