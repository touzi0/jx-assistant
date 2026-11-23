package main

// 模型清单解析的两类验收：
//  1. 列表字段的 JSON 形态差异（modalities / input_modalities /
//     reasoning_levels / effort.supported_levels 有数组、对象、缺失三种写法）
//     不能拖垮整份清单；
//  2. 只返回 id 的老式端点（智谱 /models）按型号登记的猜测元数据要跟得上新
//     型号——glm-5.4 曾经因为猜测表里没有它而落到 glm-5 通用分支，1M 上下文
//     被写成 128K、视觉也没标上。

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
		t.Fatalf("plain-model: 缺 modalities 时图片能力按猜测为否、上下文取真实字段，实际 %+v", m)
	}
	// ③ null：等同缺失，正常跳过
	if m := byID["null-model"]; m.SupportsImages || len(m.ReasoningLevels) != 0 {
		t.Fatalf("null-model: 字段为 null 时应正常跳过，实际 %+v", m)
	}
}

// TestGuessModelMetaGLM54 智谱 /models 只回 id（无上下文、无模态字段），
// 元数据全靠 guessModelMeta 的型号登记。glm-5.4 上线时漏登记，落进
// 「glm-5 通用」分支被报成 128K 纯文本——这里钉住它的真实规格，
// 同时钉住邻近型号不被顺手改坏。
func TestGuessModelMetaGLM54(t *testing.T) {
	cases := []struct {
		id     string
		ctx    int
		images bool
		why    string
	}{
		{"glm-5.4", 1048576, true, "5.4 模型卡片：1M 上下文 + 视觉"},
		{"glm-5.3", 1048576, false, "5.3 上下文 1M，纯文本"},
		{"glm-5.3-flash", 1048576, true, "5.3-flash 多模态"},
		{"glm-5", 131072, false, "5 系早期型号维持既有 128K 口径"},
		{"glm-4.5", 131072, false, "4.x 维持 128K 纯文本"},
	}
	for _, c := range cases {
		ctx, img := guessModelMeta(c.id)
		if ctx != c.ctx || img != c.images {
			t.Errorf("%s（%s）：得到 ctx=%d images=%v，期望 ctx=%d images=%v",
				c.id, c.why, ctx, img, c.ctx, c.images)
		}
	}
}
