//go:build darwin

package main

// 钥匙串加密链路回归：protectSecret/unprotectSecret 全链（含格式前缀与
// fail-open 语义）与 AES-GCM 原语往返。依赖登录钥匙串（security CLI），
// 无登录会话的环境会按 fail-open 降级——那也是要验证的行为之一。

import (
	"bytes"
	"strings"
	"testing"
)

func TestDPAPIDarwinRoundTrip(t *testing.T) {
	plain := "sk-测试密钥-abc123"
	enc, err := protectSecret(plain)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	if !strings.HasPrefix(enc, dpapiPrefix) {
		t.Fatalf("应带 dpapi:v1 前缀（钥匙串可用时）: %q", enc)
	}
	if got := unprotectSecret(enc); got != plain {
		t.Fatalf("往返不一致: %q", got)
	}
	// 明文与空值原样
	if unprotectSecret("明文旧值") != "明文旧值" || protectThenCheck(t, "") != "" {
		t.Fatal("明文/空值应原样")
	}
}

func protectThenCheck(t *testing.T, s string) string {
	t.Helper()
	out, err := protectSecret(s)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDPAPIDarwinPrimitive(t *testing.T) {
	blob, err := dpapiProtect([]byte("中文内容"))
	if err != nil {
		t.Skipf("钥匙串不可用（无登录会话）: %v", err)
	}
	if bytes.Equal(blob, []byte("中文内容")) {
		t.Fatal("不应是直通明文")
	}
	back, err := dpapiUnprotect(blob)
	if err != nil || string(back) != "中文内容" {
		t.Fatalf("原语往返失败: %v %q", err, back)
	}
}
