package main

// 品牌下发的客户端半边（brand_sync.go）：差异驱动应用、LOGO 按 hash 比对
// 只在该换时下载一次、老服务端（brand=nil）不动本地、服务端删图回退默认。

import (
	"bytes"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestApplyBrand(t *testing.T) {
	// 假 LOGO 服务端：统计下载次数，验证「hash 不变绝不重复下载」
	downloads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/brand/logo.png" {
			downloads++
			_, _ = w.Write([]byte("\x89PNG-fake-logo-bytes"))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	a := &App{Cfg: &Config{DataDir: t.TempDir()}}

	// 老服务端（brand=nil）：不动本地，返回无变化
	if got := a.applyBrand(nil, srv.URL); got != "" {
		t.Fatalf("nil 公告应零动作，got %q", got)
	}

	// 首次应用：文字落盘 + LOGO 下载一次
	if got := a.applyBrand(&brandAnnounce{Name: "赛时达", HeroTitle: "更懂装备制造", LogoHash: "abc123"}, srv.URL); got == "" {
		t.Fatal("首次应用应有变更描述")
	}
	b := a.loadBrandLocal()
	if b.Name != "赛时达" || b.HeroTitle != "更懂装备制造" || b.LogoHash != "abc123" {
		t.Fatalf("brand.json 落盘不对: %+v", b)
	}
	if _, err := os.Stat(a.brandLogoPath()); err != nil {
		t.Fatal("LOGO 应已下载缓存")
	}
	if downloads != 1 {
		t.Fatalf("首次应用应恰好下载一次，got %d", downloads)
	}

	// 同 hash 重复公告：零动作、零下载
	if got := a.applyBrand(&brandAnnounce{Name: "赛时达", HeroTitle: "更懂装备制造", LogoHash: "abc123"}, srv.URL); got != "" {
		t.Fatalf("无变化公告应零动作，got %q", got)
	}
	if downloads != 1 {
		t.Fatalf("hash 未变不应重复下载，got %d", downloads)
	}

	// 只换文字：应用文字、不碰图
	if got := a.applyBrand(&brandAnnounce{Name: "赛时达2", HeroTitle: "更懂装备制造", LogoHash: "abc123"}, srv.URL); got == "" {
		t.Fatal("换文字应有变更")
	}
	if downloads != 1 {
		t.Fatalf("只换文字不应下载图，got %d", downloads)
	}

	// 服务端删图（hash 空）：本地图删除、hash 清空回退默认
	if got := a.applyBrand(&brandAnnounce{Name: "赛时达2", HeroTitle: "更懂装备制造"}, srv.URL); got == "" {
		t.Fatal("删图公告应有变更")
	}
	if _, err := os.Stat(a.brandLogoPath()); !os.IsNotExist(err) {
		t.Fatal("本地 LOGO 应已删除")
	}
	if b := a.loadBrandLocal(); b.LogoHash != "" {
		t.Fatalf("删图后 hash 应清空: %+v", b)
	}

	// 换图（新 hash）：再下载一次
	if got := a.applyBrand(&brandAnnounce{Name: "赛时达2", HeroTitle: "更懂装备制造", LogoHash: "def456"}, srv.URL); got == "" {
		t.Fatal("换图公告应有变更")
	}
	if downloads != 2 {
		t.Fatalf("换图应再下载一次，got %d", downloads)
	}
	if b := a.loadBrandLocal(); b.LogoHash != "def456" {
		t.Fatalf("新 hash 应落盘: %+v", b)
	}
}

// 下载失败（服务端无图 404）：图保持现图、hash 不落（下轮重试），文字仍应用。
func TestApplyBrandDownloadFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r) // 没有 /brand/logo.png
	}))
	t.Cleanup(srv.Close)
	a := &App{Cfg: &Config{DataDir: t.TempDir()}}

	if got := a.applyBrand(&brandAnnounce{Name: "X", LogoHash: "h1"}, srv.URL); got == "" {
		t.Fatal("应有变更（文字）")
	}
	if _, err := os.Stat(a.brandLogoPath()); !os.IsNotExist(err) {
		t.Fatal("下载失败不应落图")
	}
	if b := a.loadBrandLocal(); b.LogoHash != "" || b.Name != "X" {
		t.Fatalf("文字应应用、hash 不落: %+v", b)
	}
}

// /logo-default.png：关于页固定 LOGO——只出内置默认图（embed 的 web/logo.png），
// 即便 data/brand/ 已有服务端下发的缓存图也不跟随（走品牌的是 /logo.png）。
func TestDefaultLogoRoute(t *testing.T) {
	static, err := fs.Sub(webFS, "web")
	if err != nil {
		t.Fatal(err)
	}
	a := &App{Cfg: &Config{DataDir: t.TempDir()}, static: static}

	// 预置品牌缓存图（模拟服务端已下发换装）：/logo-default.png 仍应出内置图
	if err := os.MkdirAll(a.brandDir(), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.brandLogoPath(), []byte("\x89PNG-fake-brand-logo"), 0644); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	a.handleDefaultLogo(w, httptest.NewRequest("GET", "/logo-default.png", nil))
	if w.Code != 200 {
		t.Fatalf("状态码 got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type got %q", ct)
	}
	want, err := fs.ReadFile(static, "logo.png")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(w.Body.Bytes(), want) {
		t.Fatalf("应原样返回内置默认图（%d 字节），got %d 字节", len(want), w.Body.Len())
	}
}
