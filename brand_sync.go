// brand_sync.go —— 品牌下发的客户端半边（服务端 jxaiserver brand.go → /announce.brand）。
//
// 品牌标识（LOGO 图 + 品牌名/首页大标题文字）由公司服务端管理页统一配置下发，
// 客户端**没有配置入口**、只消费：本地缓存在 data/brand/（brand.json 文字 + logo.png
// 图片），渲染全程读本地——断网、出公司，界面品牌照样显示。
//
// 流量口径（用户明确要求）：LOGO 只在**首次**与**服务端换图**（logoHash 变化）时
// 下载一次，其余每 5 分钟的公告轮询只多约百字节的 brand 元数据；hash 一致绝不上网。
//
// 应用点：
//   - sync.go applyAnnounce 收到公告调 applyBrand（差异驱动）；
//   - server.go 静态路由拦截 /logo.png：有本地缓存图优先服务，否则 embed 默认图；
//   - GET /api/brand 供前端取文字（前端启动拉一次，brand 事件到达时刷新）；
//   - GET /logo-default.png 关于页专用：只出内置默认图，永不跟随品牌；
//   - 老服务端（公告无 brand 字段）不动本地现值。
package main

import (
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// brandAnnounce /announce 载荷里的 brand 字段（服务端始终下发，空值=回退默认）。
type brandAnnounce struct {
	Name      string `json:"name"`
	HeroTitle string `json:"heroTitle"`
	LogoHash  string `json:"logoHash"`
}

// brandLocal data/brand/brand.json 的落盘形态（LogoHash 是已成功落地图的内容哈希，
// 与公告比对决定要不要重新下载；下载失败时保留旧值下一轮重试）。
type brandLocal struct {
	Name      string `json:"name"`
	HeroTitle string `json:"heroTitle"`
	LogoHash  string `json:"logoHash,omitempty"`
}

func (a *App) brandDir() string      { return filepath.Join(a.Cfg.DataDir, "brand") }
func (a *App) brandFile() string     { return filepath.Join(a.brandDir(), "brand.json") }
func (a *App) brandLogoPath() string { return filepath.Join(a.brandDir(), "logo.png") }

// loadBrandLocal 读本地品牌缓存（读不到给零值=用内置默认）。
func (a *App) loadBrandLocal() brandLocal {
	var f brandLocal
	if data, err := os.ReadFile(a.brandFile()); err == nil {
		_ = json.Unmarshal(data, &f)
	}
	return f
}

// applyBrand 应用一份品牌公告（差异驱动）：文字与 hash 完全一致则零动作；
// hash 变化才联网换图（一次）。返回变更描述（空=无变化）。落盘独立于 config 保存。
func (a *App) applyBrand(b *brandAnnounce, server string) string {
	if b == nil {
		return "" // 老服务端未下发 brand 字段：保持本地现值
	}
	cur := a.loadBrandLocal()
	if cur.Name == b.Name && cur.HeroTitle == b.HeroTitle && cur.LogoHash == b.LogoHash {
		return ""
	}
	logoChanged := cur.LogoHash != b.LogoHash
	if logoChanged {
		if b.LogoHash == "" {
			_ = os.Remove(a.brandLogoPath()) // 服务端删图：回退内置默认
		} else if !a.downloadBrandLogo(server) {
			logoChanged = false // 下载失败：图保持现图、hash 不落（下一轮同步重试）
		}
	}
	nb := brandLocal{Name: b.Name, HeroTitle: b.HeroTitle, LogoHash: cur.LogoHash}
	if logoChanged {
		nb.LogoHash = b.LogoHash
	}
	if err := os.MkdirAll(a.brandDir(), 0755); err == nil {
		if data, err := json.MarshalIndent(nb, "", "  "); err == nil {
			if err := writeFileAtomic(a.brandFile(), data); err != nil {
				log.Printf("[品牌] 缓存落盘失败：%v", err)
			}
		}
	}
	log.Printf("[品牌] 已跟随服务端：名称=%q 大标题=%q 换图=%v", b.Name, b.HeroTitle, logoChanged)
	a.broadcast(Event{Type: "brand"}) // 前端刷新文字（图走 /logo.png 自然换装）
	if b.Name != "" {
		return "品牌「" + b.Name + "」"
	}
	return "品牌"
}

// downloadBrandLogo 从公告来源下载 LOGO 一次（companyTransport 兼容内网自签证书）。
func (a *App) downloadBrandLogo(server string) bool {
	if strings.TrimSpace(server) == "" {
		return false
	}
	u := strings.TrimRight(server, "/") + "/brand/logo.png"
	client := http.Client{Timeout: 30 * time.Second, Transport: companyTransport}
	resp, err := client.Get(u)
	if err != nil {
		log.Printf("[品牌] LOGO 下载失败（%s）：%v", u, err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("[品牌] LOGO 下载失败（%s）：HTTP %d", u, resp.StatusCode)
		return false
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil || len(data) == 0 {
		log.Printf("[品牌] LOGO 读取失败：%v", err)
		return false
	}
	if err := os.MkdirAll(a.brandDir(), 0755); err != nil {
		return false
	}
	return writeFileAtomic(a.brandLogoPath(), data) == nil
}

// handleBrand GET /api/brand：当前生效的品牌文字（本地缓存；未下发=空值，
// 前端拿空值用内置默认「坚祥数字助理」）。
func (a *App) handleBrand(w http.ResponseWriter, r *http.Request) {
	b := a.loadBrandLocal()
	_, hasLogo := os.Stat(a.brandLogoPath())
	writeJSON(w, 200, map[string]any{
		"name": b.Name, "heroTitle": b.HeroTitle,
		"logo": hasLogo == nil,
	})
}

// serveBrandLogo 静态路由 /logo.png 的品牌覆盖：有服务端下发的缓存图优先服务，
// 返回 false 走 embed 默认图。
func (a *App) serveBrandLogo(w http.ResponseWriter, r *http.Request) bool {
	data, err := os.ReadFile(a.brandLogoPath())
	if err != nil || len(data) == 0 {
		return false
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
	return true
}

// handleDefaultLogo GET /logo-default.png：只返回内置默认 LOGO（embed 的
// web/logo.png；-webdir 开发模式下即磁盘上的那张，与 /logo.png 的回退源同源），
// 不看 data/brand/ 缓存。设置·关于页专用（2026-11-19 用户口径：关于页是软件
// 自身的基础信息介绍，名称与 LOGO 都不随服务端品牌变化；/logo.png 的品牌
// 换装行为保持原样，左上角/首页 hero 仍在用）。
func (a *App) handleDefaultLogo(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(a.static, "logo.png")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}
