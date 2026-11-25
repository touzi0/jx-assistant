package main

// canonicalFor 的归一化回归：NFC/NFD 同名等价（macOS 文件系统形态漂移）
// 与符号链接解析（全平台）。这两个漂移会让字面 filepath.Rel 把工作区内
// 的文件误判成工作区外（快照键、交付核对都会错乱）。

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/text/unicode/norm"
)

func TestCanonicalForNFCEquivalence(t *testing.T) {
	if !isDarwinRuntime {
		t.Skip("NFD 形态漂移仅 darwin 文件系统真实存在")
	}
	dir := t.TempDir()
	// 注意选名：纯汉字在 Unicode 里没有分解式（NFC==NFD，实测确认），真正会
	// 漂移的是带附加符号的拉丁字母（é→e+◌́）、韩文等——用混合名覆盖真实场景
	nfc := "café季度报告.txt"
	p := filepath.Join(dir, nfc)
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	nfd := norm.NFD.String(nfc)
	if nfd == nfc {
		t.Skip("文件名无分解式差异（NFD==NFC）")
	}
	a := canonicalFor(filepath.Join(dir, nfc))
	b := canonicalFor(filepath.Join(dir, nfd))
	if a != b {
		t.Fatalf("NFC 与 NFD 形态应归一相等：\nNFC→%s\nNFD→%s", a, b)
	}
}

func TestCanonicalForSymlink(t *testing.T) {
	if isWindowsRuntime {
		t.Skip("Windows 建符号链接需要特权，原生路径由 canonicalNative 另行覆盖")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "真身.txt")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "别名.txt")
	if err := os.Symlink(p, link); err != nil {
		t.Skip("环境不允许建符号链接: " + err.Error())
	}
	if canonicalFor(link) != canonicalFor(p) {
		t.Fatalf("符号链接应解析到真身：\nlink→%s\nreal→%s", canonicalFor(link), canonicalFor(p))
	}
}
