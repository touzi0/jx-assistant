package main

// Claude Code 插件安装器。
// 四种来源：
//   本地目录 / 本地 zip  → 直接进缓存（市场记 local）
//   git 仓库 URL         → git clone --depth 1 后按目录处理（单插件或市场仓库）
//   已添加市场 + 插件名   → 解析 marketplace.json 条目的 source 三形态：
//                          "./相对路径"（市场克隆内复制）、"git-subdir"（克隆后取
//                          子目录）、"url"（git/zip 下载）
// 安装落位 cache/<市场>/<插件>/<版本>/，写安装记录后 Reload。
// 版本取 plugin.json 的 version（缺省 "0"），多版本共存、重装即覆盖同版本。

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ccInstallFromPath 从本地目录或 zip 安装一个插件（marketplace 记 local 或指定值）。
func (r *CCPluginRegistry) ccInstallFromPath(srcPath, marketplace string) (string, error) {
	src := strings.TrimSpace(srcPath)
	if src == "" {
		return "", fmt.Errorf("路径不能为空")
	}
	if marketplace == "" {
		marketplace = "local"
	}
	root, cleanup, err := ccStageSource(src)
	if err != nil {
		return "", err
	}
	if cleanup != nil {
		defer cleanup()
	}
	// 市场仓库误当插件装：目录本身没有清单但有 marketplace.json 时给出指引
	if !ccHasPluginManifest(root) {
		if ccHasMarketplaceManifest(root) {
			return "", fmt.Errorf("该目录是 Claude Code 插件市场（含 marketplace.json），请先添加市场再从中安装插件")
		}
		return "", fmt.Errorf("目录里找不到 .claude-plugin/plugin.json（不是有效的 Claude Code 插件）")
	}
	return r.ccInstallFromTree(root, marketplace)
}

// ccStageSource 把来源（目录/zip/git URL）物化成本地插件根目录；
// cleanup 非空时调用方用完要清理临时目录。
func ccStageSource(src string) (root string, cleanup func(), err error) {
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") || strings.HasSuffix(src, ".git") {
		if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
			if strings.HasSuffix(src, ".git") || strings.Contains(src, "github.com") {
				return ccCloneGit(src)
			}
			return ccDownloadZip(src)
		}
		return ccCloneGit(src)
	}
	info, err := os.Stat(src)
	if err != nil {
		return "", nil, fmt.Errorf("路径不存在: %s", src)
	}
	if info.IsDir() {
		return src, nil, nil
	}
	if !strings.EqualFold(filepath.Ext(src), ".zip") {
		return "", nil, fmt.Errorf("只支持插件目录、.zip 压缩包或 git/https URL: %s", src)
	}
	tmp, err := os.MkdirTemp("", "jxcc-")
	if err != nil {
		return "", nil, err
	}
	if err := unzip(src, tmp); err != nil {
		os.RemoveAll(tmp)
		return "", nil, fmt.Errorf("解压失败: %w", err)
	}
	root, err = ccFindPluginRoot(tmp)
	if err != nil {
		os.RemoveAll(tmp)
		return "", nil, err
	}
	return root, func() { os.RemoveAll(tmp) }, nil
}

// ccFindPluginRoot 在解压目录里找插件根（zip 根有清单，或唯一子目录有）。
func ccFindPluginRoot(dir string) (string, error) {
	if ccHasPluginManifest(dir) {
		return dir, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	subs := []string{}
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			subs = append(subs, e.Name())
		}
	}
	if len(subs) == 1 {
		sub := filepath.Join(dir, subs[0])
		if ccHasPluginManifest(sub) {
			return sub, nil
		}
	}
	return "", fmt.Errorf("压缩包里找不到 .claude-plugin/plugin.json")
}

// ccHasPluginManifest / ccHasMarketplaceManifest 探测目录形态。
func ccHasPluginManifest(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, ".claude-plugin", "plugin.json")); err == nil {
		return true
	}
	_, err := os.Stat(filepath.Join(dir, "plugin.json"))
	return err == nil
}

func ccHasMarketplaceManifest(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".claude-plugin", "marketplace.json"))
	return err == nil
}

// ccCloneGit 浅克隆一个 git 仓库到临时目录。
func ccCloneGit(url string) (string, func(), error) {
	tmp, err := os.MkdirTemp("", "jxccgit-")
	if err != nil {
		return "", nil, err
	}
	cmd := exec.Command("git", "clone", "--depth", "1", url, tmp)
	cmd.Env = os.Environ()
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Run(); err != nil {
		os.RemoveAll(tmp)
		if execErr := err; execErr != nil {
			if _, lookErr := exec.LookPath("git"); lookErr != nil {
				return "", nil, fmt.Errorf("本机没有 git，无法克隆 %s（可改用 zip 包或本地目录安装）", url)
			}
		}
		return "", nil, fmt.Errorf("git clone 失败: %w", err)
	}
	root := tmp
	if !ccHasPluginManifest(root) && !ccHasMarketplaceManifest(root) {
		if sub, err := ccFindPluginRoot(root); err == nil {
			root = sub
		}
	}
	return root, func() { os.RemoveAll(tmp) }, nil
}

// ccDownloadZip 下载一个 zip URL 并解压。URL 来自市场清单（内网服务端），
// 故走 companyTransport（内网 https 一般没有可验证的证书）。
func ccDownloadZip(url string) (string, func(), error) {
	resp, err := newCompanyHTTPClient(5 * time.Minute).Get(url)
	if err != nil {
		return "", nil, fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", nil, fmt.Errorf("下载失败: HTTP %d", resp.StatusCode)
	}
	tmp, err := os.MkdirTemp("", "jxccdl-")
	if err != nil {
		return "", nil, err
	}
	zipPath := filepath.Join(tmp, "dl.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		os.RemoveAll(tmp)
		return "", nil, err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.RemoveAll(tmp)
		return "", nil, fmt.Errorf("下载写入失败: %w", err)
	}
	f.Close()
	out := filepath.Join(tmp, "unz")
	if err := unzip(zipPath, out); err != nil {
		os.RemoveAll(tmp)
		return "", nil, fmt.Errorf("解压失败: %w", err)
	}
	root, err := ccFindPluginRoot(out)
	if err != nil {
		os.RemoveAll(tmp)
		return "", nil, err
	}
	return root, func() { os.RemoveAll(tmp) }, nil
}

// ccInstallFromTree 把一个已就位的插件目录装进缓存并登记。
func (r *CCPluginRegistry) ccInstallFromTree(root, marketplace string) (string, error) {
	man, err := ccReadManifest(root)
	if err != nil {
		return "", err
	}
	name := ccSanitizeName(man.Name)
	version := strings.TrimSpace(man.Version)
	if version == "" {
		version = "0"
	}
	dst := filepath.Join(r.root, "cache", ccSanitizeName(marketplace), name, ccSanitizeName(version))
	if abs, err := filepath.Abs(root); err == nil && filepathCleanEqual(abs, dst) {
		// 来源就是缓存本体（重载场景）：不复制
	} else {
		if err := os.RemoveAll(dst); err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return "", err
		}
		if err := copyDir(root, dst); err != nil {
			return "", fmt.Errorf("复制插件文件失败: %w", err)
		}
	}
	key := name + "@" + ccSanitizeName(marketplace)
	rel, err := filepath.Rel(r.root, dst)
	if err != nil {
		rel = filepath.Join("cache", ccSanitizeName(marketplace), name, ccSanitizeName(version))
	}
	r.mu.Lock()
	if r.state.Plugins == nil {
		r.state.Plugins = map[string]*ccPluginState{}
	}
	r.state.Plugins[key] = &ccPluginState{
		Marketplace: ccSanitizeName(marketplace), Version: version, Path: filepath.ToSlash(rel),
	}
	err = r.saveState()
	r.mu.Unlock()
	if err != nil {
		return key, err
	}
	r.Reload()
	log.Printf("[CC插件] 已安装 %s（版本 %s）", key, version)
	return key, nil
}

// ccReadManifest 读插件清单。
func ccReadManifest(root string) (*ccPluginManifest, error) {
	manPath := filepath.Join(root, ".claude-plugin", "plugin.json")
	if _, err := os.Stat(manPath); err != nil {
		manPath = filepath.Join(root, "plugin.json")
	}
	data, err := os.ReadFile(manPath)
	if err != nil {
		return nil, fmt.Errorf("读 plugin.json 失败: %w", err)
	}
	var m ccPluginManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("plugin.json 不是合法 JSON: %w", err)
	}
	if strings.TrimSpace(m.Name) == "" {
		return nil, fmt.Errorf("plugin.json 缺少 name 字段")
	}
	return &m, nil
}

// ccSanitizeName 清洗插件/市场/版本名（只留安全文件名字符，防路径穿越）。
func ccSanitizeName(name string) string {
	name = strings.TrimSpace(name)
	var b strings.Builder
	lastDash := false
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
		if !ok {
			if !lastDash {
				b.WriteRune('-')
				lastDash = true
			}
			continue
		}
		b.WriteRune(r)
		lastDash = false
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		out = "unnamed"
	}
	return out
}

// filepathCleanEqual 比较两个路径规范化后是否同一位置。
func filepathCleanEqual(a, b string) bool {
	ab, _ := filepath.Abs(a)
	bb, _ := filepath.Abs(b)
	return strings.EqualFold(filepath.Clean(ab), filepath.Clean(bb))
}

// Uninstall 卸载一个插件（删缓存目录与记录）。
func (r *CCPluginRegistry) Uninstall(key string) error {
	r.mu.Lock()
	st, ok := r.state.Plugins[key]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("插件 %s 未安装", key)
	}
	root := st.Path
	if !filepath.IsAbs(root) {
		root = filepath.Join(r.root, root)
	}
	delete(r.state.Plugins, key)
	err := r.saveState()
	r.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.RemoveAll(root); err != nil {
		return err
	}
	os.RemoveAll(filepath.Join(r.root, "data", key))
	r.Reload()
	return nil
}

// ---------- 市场 ----------

// ccMarketEntry 是 marketplace.json 里的一条插件条目。
type ccMarketEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Author      struct {
		Name string `json:"name"`
	} `json:"author"`
	Category string `json:"category"`
	Version  string `json:"version"`
	Source   struct {
		Source string `json:"source"` // ./rel | git-subdir | url | github | filesystem
		URL    string `json:"url"`
		Path   string `json:"path"`
		Ref    string `json:"ref"`
	} `json:"source"`
}

// ccMarketplaceFile 是 .claude-plugin/marketplace.json 的解析结构。
type ccMarketplaceFile struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Plugins     []ccMarketEntry `json:"plugins"`
}

// AddMarketplaceFromURL 添加一个市场：git URL 克隆或 marketplace.json 的 URL 下载。
func (r *CCPluginRegistry) AddMarketplaceFromURL(url string) (string, error) {
	root, cleanup, err := ccCloneGit(url)
	if err != nil {
		return "", err
	}
	if !ccHasMarketplaceManifest(root) {
		if cleanup != nil {
			cleanup()
		}
		return "", fmt.Errorf("仓库里没有 .claude-plugin/marketplace.json（不是有效的插件市场）")
	}
	// 市场克隆留在 marketplaces/<id>/ 下（安装 ./相对路径 条目时从这里取）
	return r.addMarketplaceFromTree(root, "git", url, cleanup)
}

// AddMarketplaceFromPath 添加本地目录市场。
func (r *CCPluginRegistry) AddMarketplaceFromPath(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("目录不存在: %s", path)
	}
	if !ccHasMarketplaceManifest(path) {
		return "", fmt.Errorf("目录里没有 .claude-plugin/marketplace.json")
	}
	return r.addMarketplaceFromTree(path, "local", path, nil)
}

// addMarketplaceFromTree 读市场清单并把市场树复制/保留到 marketplaces/<id>/。
func (r *CCPluginRegistry) addMarketplaceFromTree(tree, kind, srcURL string, cleanup func()) (string, error) {
	data, err := os.ReadFile(filepath.Join(tree, ".claude-plugin", "marketplace.json"))
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return "", err
	}
	var mf ccMarketplaceFile
	if err := json.Unmarshal(data, &mf); err != nil {
		if cleanup != nil {
			cleanup()
		}
		return "", fmt.Errorf("marketplace.json 解析失败: %w", err)
	}
	id := ccSanitizeName(mf.Name)
	if id == "" || id == "unnamed" {
		id = ccSanitizeName(srcURL)
	}
	dst := filepath.Join(r.root, "marketplaces", id)
	_ = os.RemoveAll(dst)
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		if cleanup != nil {
			cleanup()
		}
		return "", err
	}
	// 市场树复制进数据区（本地目录市场也复制：源目录用户可能随手删）
	if err := copyDir(tree, dst); err != nil {
		if cleanup != nil {
			cleanup()
		}
		return "", fmt.Errorf("复制市场失败: %w", err)
	}
	if cleanup != nil {
		cleanup() // 临时克隆不再需要
	}
	r.mu.Lock()
	if r.state.Marketplaces == nil {
		r.state.Marketplaces = map[string]*ccMarketplaceState{}
	}
	r.state.Marketplaces[id] = &ccMarketplaceState{
		Source: kind, URL: srcURL, Name: mf.Name, AddedAt: time.Now().Format(time.RFC3339),
	}
	err = r.saveState()
	r.mu.Unlock()
	if err != nil {
		return id, err
	}
	log.Printf("[CC插件] 已添加市场 %s（插件 %d 个）", id, len(mf.Plugins))
	return id, nil
}

// MarketplaceBrowse 浏览一个市场：返回条目清单（供安装选择）。
func (r *CCPluginRegistry) MarketplaceBrowse(id string) ([]map[string]any, error) {
	tree := filepath.Join(r.root, "marketplaces", ccSanitizeName(id))
	data, err := os.ReadFile(filepath.Join(tree, ".claude-plugin", "marketplace.json"))
	if err != nil {
		return nil, fmt.Errorf("市场 %s 不存在（先添加市场）", id)
	}
	var mf ccMarketplaceFile
	if err := json.Unmarshal(data, &mf); err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, p := range mf.Plugins {
		installed := false
		if st, ok := r.stateOf(p.Name + "@" + ccSanitizeName(id)); ok {
			installed = st.Version != ""
		}
		out = append(out, map[string]any{
			"name": p.Name, "description": p.Description, "author": p.Author.Name,
			"category": p.Category, "version": p.Version, "installed": installed,
		})
	}
	return out, nil
}

// stateOf 查一条安装记录（只读）。
func (r *CCPluginRegistry) stateOf(key string) (*ccPluginState, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	st, ok := r.state.Plugins[key]
	return st, ok
}

// rootOf 返回一个已装插件的插件根目录（绝对路径；${CLAUDE_PLUGIN_ROOT}，
// 市场上传时从这里现场打包）。安装记录缺失时 ok=false。
func (r *CCPluginRegistry) rootOf(key string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	st, ok := r.state.Plugins[key]
	if !ok {
		return "", false
	}
	root := st.Path
	if !filepath.IsAbs(root) {
		root = filepath.Join(r.root, root)
	}
	return root, true
}

// InstallFromMarketplace 从已添加市场安装一个插件（解析条目 source 三形态）。
func (r *CCPluginRegistry) InstallFromMarketplace(marketplaceID, pluginName string) (string, error) {
	marketplaceID = ccSanitizeName(marketplaceID)
	tree := filepath.Join(r.root, "marketplaces", marketplaceID)
	data, err := os.ReadFile(filepath.Join(tree, ".claude-plugin", "marketplace.json"))
	if err != nil {
		return "", fmt.Errorf("市场 %s 不存在（先添加市场）", marketplaceID)
	}
	var mf ccMarketplaceFile
	if err := json.Unmarshal(data, &mf); err != nil {
		return "", err
	}
	for _, p := range mf.Plugins {
		if p.Name != pluginName {
			continue
		}
		return r.installMarketplaceEntry(marketplaceID, tree, p)
	}
	return "", fmt.Errorf("市场 %s 里没有插件 %s", marketplaceID, pluginName)
}

// installMarketplaceEntry 按条目 source 形态安装：
// ./相对路径（市场树内复制）| git-subdir（克隆后取子目录）| url（git/zip 下载）。
func (r *CCPluginRegistry) installMarketplaceEntry(marketplaceID, tree string, p ccMarketEntry) (string, error) {
	src := p.Source
	switch {
	case strings.HasPrefix(src.Source, "./") || src.Source == "" || src.Source == "filesystem":
		rel := strings.TrimPrefix(src.Source, "./")
		if rel == "" {
			rel = strings.TrimPrefix(src.Path, "./")
		}
		if rel == "" {
			rel = "plugins/" + p.Name
		}
		local := filepath.Join(tree, filepath.FromSlash(rel))
		if !ccHasPluginManifest(local) {
			// 常见布局兜底：external_plugins/<name>
			alt := filepath.Join(tree, "external_plugins", p.Name)
			if ccHasPluginManifest(alt) {
				local = alt
			}
		}
		return r.ccInstallFromPath(local, marketplaceID)
	case src.Source == "git-subdir":
		if src.URL == "" {
			return "", fmt.Errorf("插件 %s 的市场条目缺少 url", p.Name)
		}
		root, cleanup, err := ccCloneGit(src.URL)
		if err != nil {
			return "", err
		}
		if cleanup != nil {
			defer cleanup()
		}
		sub := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(src.Path, "./")))
		if !ccHasPluginManifest(sub) {
			return "", fmt.Errorf("仓库 %s 里找不到子目录 %s", src.URL, src.Path)
		}
		return r.ccInstallFromTree(sub, marketplaceID)
	case src.Source == "url" || src.Source == "github":
		if src.URL == "" {
			return "", fmt.Errorf("插件 %s 的市场条目缺少 url", p.Name)
		}
		root, cleanup, err := ccStageSource(src.URL)
		if err != nil {
			return "", err
		}
		if cleanup != nil {
			defer cleanup()
		}
		return r.ccInstallFromTree(root, marketplaceID)
	}
	return "", fmt.Errorf("不支持的市场条目来源: %s", src.Source)
}
