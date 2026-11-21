package main

// 远程访问图片链路的回归测试（2026-11-20）：
//  1. relayPathAllowed 放行 /attachments/（工具图片信封的附件库端点）——
//     中继与 P2P 直连共用，漏放则远程端工具卡片取图打到网关源 404；
//  2. 远程外壳（p2pShell）把 wechat-qr.png / logo-default.png 内联为 dataURL
//     ——外壳页挂在外网端，拿不到家端静态文件路由，不内联则设置页二维码裂图。

import (
	"encoding/base64"
	"io/fs"
	"strings"
	"testing"
)

func TestRelayPathAllowedAttachments(t *testing.T) {
	allow := []string{
		"/api/status",                                     // 常规接口（既有）
		"/sites", "/sites-assets/x.js", "/s/abc/",         // 企业小程序（既有）
		"/attachments/ab/cd.png", "/attachments/x.webp?a=1", // 附件库（本次新增）
	}
	deny := []string{
		"/attachments",           // 无斜杠不前缀放行（与 /s/ 同口径，防误配）
		"/attachment/x.png",      // 相似前缀
		"/attachmentsfoo",        // 相似前缀
		"/api",                   // 无斜杠（既有口径如此，回归保护）
		"/other", "/", "",
	}
	for _, p := range allow {
		if !relayPathAllowed(p) {
			t.Errorf("应放行 %q", p)
		}
	}
	for _, p := range deny {
		if relayPathAllowed(p) {
			t.Errorf("应拒绝 %q", p)
		}
	}
}

// p2pShellOnce 是包级 Once：本文件只在这一个用例里触发拼装，避免与同进程
// 其他用例共享缓存产生顺序耦合。
func TestP2PShellInlinesStaticImages(t *testing.T) {
	static, err := fs.Sub(webFS, "web")
	if err != nil {
		t.Fatal(err)
	}
	a := &App{Cfg: &Config{DataDir: t.TempDir()}, static: static}
	base, err := a.p2pShell()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(base, `src="/wechat-qr.png"`) {
		t.Error("外壳仍引用 /wechat-qr.png：远程端拿不到静态路由，二维码会裂图")
	}
	if strings.Contains(base, `src="/logo-default.png"`) {
		t.Error("外壳仍引用 /logo-default.png：关于页 LOGO 在远程端会裂图")
	}
	// 二维码 dataURL 与源文件字节一致
	qr, err := fs.ReadFile(static, "wechat-qr.png")
	if err != nil {
		t.Fatal(err)
	}
	want := `src="data:image/png;base64,` + base64.StdEncoding.EncodeToString(qr) + `"`
	if !strings.Contains(base, want) {
		t.Error("外壳未内联 wechat-qr.png 的 dataURL（或内容不一致）")
	}
	// remote-boot 内联进外壳，且隧道白名单带上 /attachments/（工具图片信封
	// 经 JX_REMOTE.assetURL 走 fetch 隧道取图；只改磁盘源码而外壳没带上等于没修）
	if !strings.Contains(base, "indexOf('/attachments/') === 0") {
		t.Error("外壳内联的 remote-boot 未放行 /attachments/ 隧道白名单")
	}
}
