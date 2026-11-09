package main

// 模型清单解析的形态兼容验收：/v1/models 里 modalities / input_modalities /
// reasoning_levels / effort.supported_levels 这类列表字段各家写法不一——
// 老式端点是字符串数组，较新的聚合网关返回对象，还有的模型整个字段
// 不写。硬绑 []string 时，只要有一个模型写法不同，整份清单就报
// 「响应不是合法的模型清单 JSON」（用户实际踩到过：
// "modalities":{"input":["text","image"]}）。
//
// 这里钉住三条，且**三种形态都必须保留原有语义**（是适配，不是替换）：
//  1. 老式字符串数组 ["text","image"] 照旧解析，取值与改动前完全一致；
//  2. 新式对象 {"input":["text","image"]} 也能解析出同样的图片能力；
//  3. 字段缺失 / 为 null 时正常跳过，不报错、不影响其它模型。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestFlexStringsShapes 钉住归一规则本身：老式数组、新式对象、缺失/null 都要能解析。
func TestFlexStringsShapes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		// ① 老式字符串数组——改动前唯一被支持的写法，必须原样保留语义
		{"老式数组", `["text","image"]`, []string{"text", "image"}},
		{"老式空数组", `[]`, []string{}},
		// ② 新式对象——聚合网关的形态，改动前会让整份清单解析失败
		{"新式对象 input/output", `{"input":["text","image"],"output":["text"]}`, []string{"text", "image", "text"}},
		{"新式对象单键", `{"input":["text","image"]}`, []string{"text", "image"}},
		{"新式对象空值", `{"input":[]}`, []string{}},
		// ③ 缺失 / null：不报错，按「没有」处理
		{"null", `null`, nil},
		// 其它宽容写法（不改语义，只是不让它拖垮整份清单）
		{"单个字符串", `"text"`, []string{"text"}},
		{"数字等非字符串一律忽略", `123`, nil},
		{"嵌套对象", `{"input":{"modalities":["image"]}}`, []string{"image"}},
	}
	for _, c := range cases {
		var got flexStrings
		if err := json.Unmarshal([]byte(c.raw), &got); err != nil {
			t.Fatalf("%s: 不该解析失败: %v", c.name, err)
		}
		if !sameStrings([]string(got), c.want) {
			t.Fatalf("%s: 期望 %v，实际 %v", c.name, c.want, []string(got))
		}
	}
}

// TestFetchModelsToleratesFieldShapes 用假的 /v1/models 端点跑整条拉取链路：
// 老式数组、新式对象、缺失、null 四种写法混在同一份清单里，必须全部解析出来
// 且各自取值正确——尤其要保证老式数组端点的解析结果与改动前一致。
func TestFetchModelsToleratesFieldShapes(t *testing.T) {
	body := `{"object":"list","data":[
		{"id":"legacy-array","input_modalities":["text","image"],"modalities":["text","image"],"effort":{"supported_levels":["low","high"]}},
		{"id":"legacy-modalities","modalities":["text","image"],"reasoning_levels":["low","high"]},
		{"id":"legacy-text-only","modalities":["text"]},
		{"id":"obj-modalities","modalities":{"input":["text","image"],"output":["text"]}},
		{"id":"obj-input-modalities","input_modalities":{"input":["text","image"]}},
		{"id":"obj-effort","effort":{"supported_levels":{"levels":["low","high","max"]}}},
		{"id":"plain-model","context_window":131072},
		{"id":"null-model","input_modalities":null,"modalities":null,"reasoning_levels":null,"effort":{"supported_levels":null}}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	a := newCompactTestApp(t)
	got, err := a.FetchModels(&ProviderCfg{ID: "fake", Name: "假渠道", BaseURL: srv.URL + "/v1"})
	if err != nil {
		t.Fatalf("字段形态差异不该让整份清单解析失败: %v", err)
	}
	if len(got) != 8 {
		t.Fatalf("应解析出 8 个模型，实际 %d: %+v", len(got), got)
	}

	byID := map[string]ModelCfg{}
	for _, m := range got {
		byID[m.ID] = m
	}

	// ① 老式数组：input_modalities / modalities 数组写法照旧生效
	if m := byID["legacy-array"]; !m.SupportsImages || !sameStrings(m.ReasoningLevels, []string{"low", "high"}) {
		t.Fatalf("legacy-array: 老式数组写法应识别图片+档位（effort.supported_levels 数组），实际 %+v", m)
	}
	// ① 老式数组：modalities 数组 + reasoning_levels 数组
	if m := byID["legacy-modalities"]; !m.SupportsImages || !sameStrings(m.ReasoningLevels, []string{"low", "high"}) {
		t.Fatalf("legacy-modalities: 老式数组写法应识别图片+档位，实际 %+v", m)
	}
	// ① 老式数组语义不变：["text"] 仍是纯文本，不能被兜底猜测改判
	if m := byID["legacy-text-only"]; m.SupportsImages {
		t.Fatalf("legacy-text-only: 数组里没有 image 就该是纯文本，实际 %+v", m)
	}
	// ② 新式对象：modalities 对象写法（聚合网关的形态）
	if m := byID["obj-modalities"]; !m.SupportsImages {
		t.Fatalf("obj-modalities: {\"input\":[...]} 写法应识别出图片能力，实际 %+v", m)
	}
	// ② 新式对象：input_modalities 对象写法
	if m := byID["obj-input-modalities"]; !m.SupportsImages {
		t.Fatalf("obj-input-modalities: {\"input\":[...]} 写法应识别出图片能力，实际 %+v", m)
	}
	// ② 新式对象：effort.supported_levels 对象写法
	if m := byID["obj-effort"]; !sameStrings(m.ReasoningLevels, []string{"low", "high", "max"}) {
		t.Fatalf("obj-effort: 对象写法的档位应摊平，实际 %+v", m)
	}
	// ③ 缺字段：不影响其它模型，自己按猜测兜底
	if m := byID["plain-model"]; m.SupportsImages || m.ContextWindow != 131072 || len(m.ReasoningLevels) != 0 {
		t.Fatalf("plain-model: 缺 modalities 时图片能力应按猜测、上下文取真实字段，实际 %+v", m)
	}
	// ③ null：等同缺失，正常跳过
	if m := byID["null-model"]; m.SupportsImages || len(m.ReasoningLevels) != 0 {
		t.Fatalf("null-model: 字段为 null 时应正常跳过，实际 %+v", m)
	}
}
