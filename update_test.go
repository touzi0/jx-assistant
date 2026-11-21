package main

// versionCompare 双体系版本号比较的单测：新的「日期-序号」（20261013-1）、
// 旧写法「日期+V序号」（20261008V2）与更旧的点分号（0.1.9），
// 以及两体系交界处「日期恒新」的规则。

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVersionCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		// 旧体系（原语义不回退）
		{"0.1.9", "0.1.9", 0},
		{"0.2", "0.2.0", 0},
		{"0.2.1", "0.2", 1},
		{"1.0.0", "0.9.9", 1},
		{"v0.2", "0.1.9", 1},
		// 新体系：日期比 8 位数字，再比 V 序号（V10 > V9）
		{"20261008V1", "20261008V1", 0},
		{"20261008V2", "20261008V1", 1},
		{"20261008V1", "20261008V2", -1},
		{"20261008V10", "20261008V9", 1}, // 字典序会排错，必须按整数比
		{"20261009V1", "20261008V99", 1}, // 日期优先于序号
		{"20261101V1", "20261031V1", 1},
		{"20261008v2", "20261008V10", -1}, // 小写 v 同样识别
		// 新格式：日期-序号（20261013-1），同日重出用 -2 -3
		{"20261013-1", "20261013-1", 0},
		{"20261013-2", "20261013-1", 1},
		{"20261013-10", "20261013-9", 1}, // 序号按整数比
		{"20261014-1", "20261013-99", 1}, // 日期优先于序号
		// 新旧写法混比：同体系内精确比较（V 是 - 的前身，同日同序号等价）
		{"20261013-1", "20261013V1", 0},
		{"20261013V1", "20261013-1", 0},
		{"20261013-1", "20261013V2", -1},
		{"20261014V1", "20261013-1", 1}, // 跨日混比不受写法影响
		// 交界：日期体系恒新于旧体系
		{"20260101V1", "99.99.99", 1},
		{"99.99.99", "20260101V1", -1},
	}
	for _, c := range cases {
		if got := versionCompare(c.a, c.b); got != c.want {
			t.Fatalf("versionCompare(%q, %q) = %d，期待 %d", c.a, c.b, got, c.want)
		}
		// 对称性：反向比较取反（相等除外）
		if rev := versionCompare(c.b, c.a); rev != -c.want {
			t.Fatalf("versionCompare(%q, %q) = %d，与正向 %d 不对称", c.b, c.a, rev, c.want)
		}
	}
}

// verifyEngineCarriesVersion：engine 载荷必须真带本版版本串（2026-11-19
// 内测源把 17 号二进制标成 20261119-2 发布的事故护栏）。
func TestVerifyEngineCarriesVersion(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "fake.exe")
	// 模拟 -X main.appVersion 注入的 ASCII 版本串字面量嵌在二进制里
	body := append([]byte("MZ\x90\x00\x03\x00garbage-bytes-"), []byte("20261120-1")...)
	body = append(body, 0x00, 0x00, 't', 'a', 'i', 'l')
	if err := os.WriteFile(p, body, 0644); err != nil {
		t.Fatal(err)
	}
	if err := verifyEngineCarriesVersion(p, "20261120-1"); err != nil {
		t.Fatalf("含版本串的 exe 被误拒: %v", err)
	}
	if err := verifyEngineCarriesVersion(p, "20261121-9"); err == nil {
		t.Fatal("不含版本串的 exe 应被拒绝（载荷与版本号不符）")
	}
	if err := verifyEngineCarriesVersion(p, ""); err != nil {
		t.Fatalf("空版本号应放行: %v", err)
	}
	if err := verifyEngineCarriesVersion(filepath.Join(dir, "none.exe"), "20261120-1"); err == nil {
		t.Fatal("文件读不到应报错而不是放行")
	}
}
