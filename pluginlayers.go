package main

// 插件兼容层注册表：主体与各插件体系之间的第一层抽象。
// 目标是兼容各种插件体系（DSH、Claude Code、…），且各家后端逻辑一致：
// 主体启动时经 discoverPluginLayers 发现全部兼容层（层在本文件 init() 里
// registerPluginLayer 自注册），此后只按 PluginLayer 契约调用——发现插件、
// 列清单、总开关都归层内实现，主体不认识任何具体插件格式。
// 新增一个体系 = 写一个层实现并注册，主体零改动；统一「插件」设置页按
// 层分组展示（行渲染可按层 id 注册更富的视图，缺省用通用行）。
// 层内各自的原有机制（DSH 的 Go 半边一体编译、Claude 的市场/安装/组件
// 桥）不在这一层——它们是各层自己的事。

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// LayerPlugin 是跨体系统一的插件视图（设置页「插件」页的行数据）。
// 公共字段全体系一致；组件计数、开关、配置等富字段仅支持的层填充
// （omitempty：DSH 行不带这些键）。
type LayerPlugin struct {
	Layer    string `json:"layer"`      // 来源体系（层 id）
	LayerName string `json:"layerName"` // 来源体系展示名
	ID       string `json:"id"`         // 层内插件标识：DSH=适配器 id；Claude=名@市场
	Name     string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	Version  string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
	Author   string `json:"author,omitempty"`
	Marketplace string `json:"marketplace,omitempty"`
	Enabled  bool   `json:"enabled"`
	Adapted  bool   `json:"adapted"` // DSH：插件/ 有原件但无适配器（灰显不可开关）
	// 会话生效视图（GET /api/plugins?s=<会话id> 时填充）：GlobalEnabled＝全局
	// 开关值、Override＝该会话是否显式覆盖过（按对话开关浮窗显示覆盖徽标用）。
	GlobalEnabled bool `json:"globalEnabled,omitempty"`
	Override      bool `json:"override,omitempty"`
	// Claude 层的组件概览（徽章计数 + 执行型组件开关状态）
	Components map[string]bool         `json:"components,omitempty"`
	Skills     int                     `json:"skills,omitempty"`
	Commands   int                     `json:"commands,omitempty"`
	Agents     int                     `json:"agents,omitempty"`
	Hooks      int                     `json:"hooks,omitempty"`
	Mods       int                     `json:"mods,omitempty"`
	MCP        int                     `json:"mcp,omitempty"`
	UserConfig map[string]ccUserConfigOpt `json:"userConfig,omitempty"`
	ConfigValues map[string]any        `json:"configValues,omitempty"`
	LoadError  string                  `json:"loadError,omitempty"`
}

// LayerInfo 是一个兼容层的自我介绍（统一清单的 layers 段）。
type LayerInfo struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Installable bool           `json:"installable"` // 本层能否安装插件（统一页安装区据此归属）
	Count       int            `json:"count"`       // 层内插件数
	Extra       map[string]any `json:"extra,omitempty"` // 层自定义元数据（如市场清单，前端按需取）
}

// PluginLayer 是一个插件兼容层向主体交出的契约。
type PluginLayer interface {
	Info() LayerInfo
	Plugins() []LayerPlugin
	SetEnabled(id string, on bool) error // 插件总开关
}

// 哨兵错误：API 层据此回 404。
var (
	errLayerNotFound  = errors.New("未知的插件体系")
	errPluginNotFound = errors.New("插件不存在")
)

// ---------- 主体侧注册表 ----------

// PluginLayerRegistry 持有主体发现的全部兼容层；方法并发安全。
type PluginLayerRegistry struct {
	mu     sync.RWMutex
	layers []PluginLayer
}

// pluginLayerFactories 是编译期自注册的层工厂（init 顺序即分组顺序）。
var pluginLayerFactories []func(a *App) PluginLayer

func registerPluginLayer(f func(a *App) PluginLayer) {
	pluginLayerFactories = append(pluginLayerFactories, f)
}

// discoverPluginLayers 主体启动时发现全部兼容层（此刻构建实例）。
func discoverPluginLayers(a *App) *PluginLayerRegistry {
	r := &PluginLayerRegistry{}
	for _, f := range pluginLayerFactories {
		if l := f(a); l != nil {
			r.layers = append(r.layers, l)
		}
	}
	return r
}

// Layers 返回全部层（遍历用，返回副本）。
func (r *PluginLayerRegistry) Layers() []PluginLayer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]PluginLayer, len(r.layers))
	copy(out, r.layers)
	return out
}

// Infos 全部层的自我介绍（按发现顺序）。
func (r *PluginLayerRegistry) Infos() []LayerInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]LayerInfo, 0, len(r.layers))
	for _, l := range r.layers {
		out = append(out, l.Info())
	}
	return out
}

// AllPlugins 按层序拼出统一插件清单（每条带 layer 来源标记）。
func (r *PluginLayerRegistry) AllPlugins() []LayerPlugin {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []LayerPlugin{}
	for _, l := range r.layers {
		out = append(out, l.Plugins()...)
	}
	return out
}

// SetEnabled 层内插件总开关。layer 为空时在全部层里按 id 唯一匹配
// （跨层重名则要求显式指定层）。
func (r *PluginLayerRegistry) SetEnabled(layer, id string, on bool) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if layer != "" {
		for _, l := range r.layers {
			if l.Info().ID == layer {
				return l.SetEnabled(id, on)
			}
		}
		return fmt.Errorf("%w: %s", errLayerNotFound, layer)
	}
	var hit PluginLayer
	for _, l := range r.layers {
		for _, p := range l.Plugins() {
			if p.ID == id {
				if hit != nil {
					return errors.New("插件 id 在多个体系里重名，请带 layer 指定")
				}
				hit = l
				break
			}
		}
	}
	if hit == nil {
		return errPluginNotFound
	}
	return hit.SetEnabled(id, on)
}

// ---------- 层实现：DSH（adapters.go 的薄包装，内部逻辑不动） ----------

type dshLayer struct{ r *PluginRegistry }

func (l dshLayer) Info() LayerInfo {
	return LayerInfo{ID: "dsh", Name: "DSH 插件", Count: len(l.r.List())}
}

func (l dshLayer) Plugins() []LayerPlugin {
	out := []LayerPlugin{}
	for _, s := range l.r.List() {
		out = append(out, LayerPlugin{
			Layer: "dsh", LayerName: "DSH 插件",
			ID: s.ID, Name: s.Name, Version: s.Version, Description: s.Description,
			Enabled: s.Enabled, Adapted: s.Adapted,
		})
	}
	return out
}

func (l dshLayer) SetEnabled(id string, on bool) error {
	if _, ok := l.r.Manifest(id); !ok {
		return errPluginNotFound
	}
	return l.r.SetEnabled(id, on)
}

// ---------- 层实现：Claude Code（ccplugin*.go 的薄包装，内部逻辑不动） ----------

type claudeLayer struct{ r *CCPluginRegistry }

func (l claudeLayer) Info() LayerInfo {
	markets := l.r.Marketplaces()
	ids := make([]string, 0, len(markets))
	for id := range markets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	list := []map[string]any{}
	for _, id := range ids {
		m := markets[id]
		list = append(list, map[string]any{"id": id, "name": m.Name, "source": m.Source, "url": m.URL, "addedAt": m.AddedAt})
	}
	return LayerInfo{
		ID: "claude", Name: "Claude Code 插件", Installable: true,
		Count: len(l.r.List()),
		Extra: map[string]any{"marketplaces": list}, // 前端市场浏览面板取
	}
}

func (l claudeLayer) Plugins() []LayerPlugin {
	out := []LayerPlugin{}
	for _, p := range l.r.List() {
		out = append(out, LayerPlugin{
			Layer: "claude", LayerName: "Claude Code 插件",
			ID: p.Key, Name: p.Name, DisplayName: p.DisplayName, Version: p.Version,
			Description: p.Description, Author: p.Author, Marketplace: p.Marketplace,
			Enabled: p.Enabled,
			Components: p.Components,
			Skills: p.HasSkills, Commands: p.HasCommands, Agents: p.HasAgents,
			Hooks: p.HasHooks, Mods: p.HasMods, MCP: p.HasMCP,
			UserConfig: p.UserConfig, ConfigValues: p.ConfigValues, LoadError: p.LoadError,
		})
	}
	return out
}

func (l claudeLayer) SetEnabled(id string, on bool) error {
	if _, ok := l.r.stateOf(id); !ok {
		return errPluginNotFound
	}
	return l.r.SetEnabled(id, on)
}

// 自注册：主体发现兼容层的唯一入口。源码顺序即统一插件页的分组顺序。
func init() {
	registerPluginLayer(func(a *App) PluginLayer {
		if a == nil || a.Plugins == nil {
			return nil
		}
		return dshLayer{r: a.Plugins}
	})
	registerPluginLayer(func(a *App) PluginLayer {
		if a == nil || a.CCPlugins == nil {
			return nil
		}
		return claudeLayer{r: a.CCPlugins}
	})
	// 独立 MCP 服务器不进统一插件页：它有自己的设置分区（设置 → MCP 连接器）
	// 与按需加载机制（mcpservers.go），与技能同位而非与插件同位。
}
