package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------- 附件库：入库体检 + 内联预算（2026-11-27 超大图真 bug 的回归） ----------
//
// bug 现场：一张 5MB+ 的高分辨率图经 read_image 入库后原样 base64 内联，请求体近
// 10MB，视觉模型的网关直接拒收（400/413）；图留在上下文里就步步失败；长期助理
// 读那段历史时信封又被内联，连别的会话也跟着崩。守三道闸：入库缩到预算内、
// 内联处兜底、历史读取只给引用不给信封。

// genNoiseImage 造一张噪点图（各通道独立随机）：JPEG/PNG 都压不动，是逼近
// 预算上限的最坏情形。
func genNoiseImage(w, h int, seed int64) image.Image {
	rng := rand.New(rand.NewSource(seed))
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = uint8(rng.Intn(256))
	}
	return img
}

func encodePNGBytes(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// pngHeaderOnly 造一个只有 IHDR 的 PNG（尺寸巨大、字节数很小）：DecodeConfig
// 能解出宽高，但 Decode 会失败——用来测像素上限的拒绝分支，不必真分配几百 MB。
func pngHeaderOnly(t *testing.T, w, h int) []byte {
	t.Helper()
	one := encodePNGBytes(t, image.NewNRGBA(image.Rect(0, 0, 1, 1)))
	// PNG 签名 8 字节 + IHDR 长度 4 字节 + "IHDR" + 宽 4 + 高 4；CRC 在数据后重算
	one[16], one[17], one[18], one[19] = byte(w>>24), byte(w>>16), byte(w>>8), byte(w)
	one[20], one[21], one[22], one[23] = byte(h>>24), byte(h>>16), byte(h>>8), byte(h)
	crc := crc32.ChecksumIEEE(one[12:29])
	one[29], one[30], one[31], one[32] = byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc)
	return one
}

// imagePartsCount 数一条 user 消息里的图片块数与 base64 总字符数。
func imagePartsCount(m Message) (images, b64Chars int) {
	parts, ok := m.Content.([]ContentPart)
	if !ok {
		return 0, 0
	}
	for _, p := range parts {
		if p.Type == "image_url" && p.ImageURL != nil {
			images++
			if i := strings.Index(p.ImageURL.URL, "base64,"); i >= 0 {
				b64Chars += len(p.ImageURL.URL) - i - len("base64,")
			}
		}
	}
	return images, b64Chars
}

func countInlined(msgs []Message) (images, b64 int) {
	for _, m := range msgs {
		n, c := imagePartsCount(m)
		images += n
		b64 += c
	}
	return images, b64
}

// TestStoreAttachmentShrinksBigImageToFitBudget 大图入库必须缩到预算内：字节与
// 边长双超的 JPEG（用户现场：5MB+、8000×6000）入库后 base64 不超预算、边长不超
// 上限；经 inlineImagesForRequest 组装后请求体是几百 KB 级而不是近 10MB。
func TestStoreAttachmentShrinksBigImageToFitBudget(t *testing.T) {
	a := newCompactTestApp(t)
	raw := encodeJPEG(genNoiseImage(3200, 2400, 1), 90)
	if base64Len(len(raw)) <= attachmentMaxInlineB64 {
		t.Fatalf("测试图应超预算才有意义：原图 %d 字节", len(raw))
	}
	t0 := time.Now()
	att, err := a.storeAttachment(raw, "image/jpeg")
	if err != nil {
		t.Fatalf("大图入库失败：%v", err)
	}
	t.Logf("原图 %s → 入库 %s %dx%d %s（%s）耗时 %s", humanSize(int64(len(raw))), att.MediaType, att.Width, att.Height, humanSize(int64(att.Bytes)), att.ShrunkFrom, time.Since(t0).Round(time.Millisecond))
	if base64Len(att.Bytes) > attachmentMaxInlineB64 {
		t.Fatalf("入库后 base64 仍超预算：%d 字节", att.Bytes)
	}
	if max(att.Width, att.Height) > attachmentMaxEdge {
		t.Fatalf("入库后边长仍超上限：%dx%d", att.Width, att.Height)
	}
	if att.ShrunkFrom == "" {
		t.Fatalf("缩过的图应记原图规模（向模型交代）")
	}
	// 入库文件与描述一致
	data, mt, err := a.loadAttachment(att.File)
	if err != nil || len(data) != att.Bytes || mt != att.MediaType {
		t.Fatalf("入库文件与描述不一致：err=%v len=%d mt=%s", err, len(data), mt)
	}
	// 组装：信封在工具结果里 → 内联成一张图，体积在预算内
	msgs := []Message{
		TextMessage("user", "看看这张图"),
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Type: "function", Function: ToolCallFunc{Name: "read_image", Arguments: `{"path":"big.jpg"}`}}}},
		{Role: "tool", ToolCallID: "c1", Content: imageEnvelope(att, "big.jpg") + "\n来源文件：big.jpg"},
	}
	out := inlineImagesForRequest(a, msgs, LLMTarget{SupportsImages: true})
	images, b64 := countInlined(out)
	if images != 1 {
		t.Fatalf("应内联 1 张图，got %d", images)
	}
	body, _ := json.Marshal(ChatRequest{Model: "m", Messages: out})
	t.Logf("内联后请求体 %s（base64 %d 字符）", humanSize(int64(len(body))), b64)
	if b64 > attachmentMaxInlineB64 {
		t.Fatalf("内联的 base64 超预算：%d", b64)
	}
	if len(body) > 1<<20 {
		t.Fatalf("单图请求体应在 1MB 内，got %d", len(body))
	}
	// 纯文本模型：不内联，原样返回
	if out2 := inlineImagesForRequest(a, msgs, LLMTarget{}); len(out2) != len(msgs) {
		t.Fatalf("纯文本模型不应插入图片消息")
	}
	// read_image 结果文案交代了缩放
	note := inlineNote(att, LLMTarget{SupportsImages: true})
	if !strings.Contains(note, "原图") || !strings.Contains(note, "已缩到") {
		t.Fatalf("read_image 应向模型说明已缩放：%q", note)
	}
}

// TestStoreAttachmentKeepsSmallOriginal 预算内的小图原样入库：字节、格式、
// 尺寸一字不改（正常图片功能不受影响）。
func TestStoreAttachmentKeepsSmallOriginal(t *testing.T) {
	a := newCompactTestApp(t)
	raw := encodePNGBytes(t, genNoiseImage(200, 150, 2)) // 噪点 PNG 约 120KB，预算内
	att, err := a.storeAttachment(raw, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	data, _, _ := a.loadAttachment(att.File)
	if !bytes.Equal(data, raw) || att.MediaType != "image/png" || att.Width != 200 || att.Height != 150 || att.ShrunkFrom != "" {
		t.Fatalf("小图应原样入库：mt=%s %dx%d shrunk=%q same=%v", att.MediaType, att.Width, att.Height, att.ShrunkFrom, bytes.Equal(data, raw))
	}
	if note := inlineNote(att, LLMTarget{SupportsImages: true}); note != "" {
		t.Fatalf("未缩放的图不该有多余说明：%q", note)
	}
	if note := inlineNote(att, LLMTarget{}); !strings.Contains(note, "不支持图片") {
		t.Fatalf("纯文本模型应提示不支持图片：%q", note)
	}
}

// TestStoreAttachmentResizesHugeFlatPNG 尺寸巨大但字节很小的 PNG（4000×3000 的
// 平滑色图）：字节没超预算也要缩边——视觉模型按像素计 token、有的厂商按像素拒收；
// 缩后仍保持 PNG（文字无损）。
func TestStoreAttachmentResizesHugeFlatPNG(t *testing.T) {
	a := newCompactTestApp(t)
	img := image.NewNRGBA(image.Rect(0, 0, 4000, 3000))
	for y := 0; y < 3000; y++ {
		for x := 0; x < 4000; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 200, G: uint8(x >> 4), B: 255, A: 255})
		}
	}
	raw := encodePNGBytes(t, img)
	if base64Len(len(raw)) > attachmentMaxInlineB64 {
		t.Fatalf("测试图字节应在预算内：%d", len(raw))
	}
	att, err := a.storeAttachment(raw, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if att.MediaType != "image/png" || att.Width != attachmentMaxEdge || att.Height != 1536 {
		t.Fatalf("应等比缩到 2048×1536 且保持 PNG：got %s %dx%d", att.MediaType, att.Width, att.Height)
	}
}

// TestResizeDownAreaAverage 面积平均缩图：2×2 色块缩成 1×1 取四色均值；不放大。
func TestResizeDownAreaAverage(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	src.SetNRGBA(0, 0, color.NRGBA{R: 0, G: 0, B: 0, A: 255})
	src.SetNRGBA(1, 0, color.NRGBA{R: 100, G: 0, B: 0, A: 255})
	src.SetNRGBA(0, 1, color.NRGBA{R: 0, G: 100, B: 0, A: 255})
	src.SetNRGBA(1, 1, color.NRGBA{R: 100, G: 100, B: 0, A: 255})
	out := resizeDown(src, 1)
	r, g, b, _ := out.At(0, 0).RGBA()
	if r>>8 != 50 || g>>8 != 50 || b>>8 != 0 {
		t.Fatalf("面积平均应得 (50,50,0)，got (%d,%d,%d)", r>>8, g>>8, b>>8)
	}
	if again := resizeDown(out, 10); again != out {
		t.Fatalf("不超边长的图应原样返回（只缩不放）")
	}
}

// jpegWithExifOrientation 给 JPEG 塞一个只含方向标签的 EXIF APP1 段（小端 TIFF）。
func jpegWithExifOrientation(jpg []byte, o int) []byte {
	tiff := []byte{'I', 'I', 42, 0, 8, 0, 0, 0, // 字节序 + 42 + IFD0 偏移 8
		1, 0, // 1 个条目
		0x12, 0x01, 3, 0, 1, 0, 0, 0, byte(o), 0, 0, 0, // tag 0x0112 SHORT 1 = o
		0, 0, 0, 0} // 下一个 IFD 偏移 0
	app1 := append([]byte("Exif\x00\x00"), tiff...)
	seg := make([]byte, 4+len(app1))
	seg[0], seg[1], seg[2], seg[3] = 0xFF, 0xE1, byte((len(seg))>>8), byte(len(seg))
	copy(seg[4:], app1)
	out := append([]byte{}, jpg[:2]...)
	out = append(out, seg...)
	return append(out, jpg[2:]...)
}

// TestExifOrientation 手机照片的 EXIF 方向：重编码的副本要转正（解码器不理 EXIF），
// 原样入库的小图不动。
func TestExifOrientation(t *testing.T) {
	// 2×1：左红右蓝；EXIF 6（顺时针 90 转正）→ 1×2：上红下蓝
	src := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	src.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 255})
	src.SetNRGBA(1, 0, color.NRGBA{B: 255, A: 255})
	got := orientImage(src, 6)
	if got.Bounds().Dx() != 1 || got.Bounds().Dy() != 2 {
		t.Fatalf("转正后应为 1×2，got %v", got.Bounds())
	}
	if r, _, _, _ := got.At(0, 0).RGBA(); r>>8 != 255 {
		t.Fatalf("转正后左上应为红")
	}
	if _, _, b, _ := got.At(0, 1).RGBA(); b>>8 != 255 {
		t.Fatalf("转正后左下应为蓝")
	}
	// 8 种方向都不该丢像素；1 原样返回
	for o := 1; o <= 8; o++ {
		im := orientImage(src, o)
		if o >= 5 && (im.Bounds().Dx() != 1 || im.Bounds().Dy() != 2) || o < 5 && (im.Bounds().Dx() != 2 || im.Bounds().Dy() != 1) {
			t.Fatalf("方向 %d 尺寸不对 %v", o, im.Bounds())
		}
	}
	if orientImage(src, 1) != src {
		t.Fatalf("方向 1 应原样返回")
	}
	// 解析：大端/小端都认，非 JPEG 与无 EXIF 回 1
	jpg := encodeJPEG(src, 90)
	if exifOrientation(jpegWithExifOrientation(jpg, 6)) != 6 {
		t.Fatalf("应读出 EXIF 方向 6")
	}
	if exifOrientation(jpg) != 1 || exifOrientation([]byte("not a jpeg")) != 1 {
		t.Fatalf("无 EXIF 应回 1")
	}
	// 端到端：超边长的 EXIF 6 照片入库 → 转正后的尺寸（宽高互换）
	a := newCompactTestApp(t)
	wide := image.NewNRGBA(image.Rect(0, 0, 3000, 1000))
	att, err := a.storeAttachment(jpegWithExifOrientation(encodeJPEG(wide, 90), 6), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if att.Width != 683 || att.Height != 2048 {
		t.Fatalf("EXIF 6 的 3000×1000 应缩成 683×2048（转正），got %dx%d", att.Width, att.Height)
	}
}

// TestStoreAttachmentRejectsOversized 原始字节或像素超上限：明确报错，不崩不静默。
func TestStoreAttachmentRejectsOversized(t *testing.T) {
	a := newCompactTestApp(t)
	_, err := a.storeAttachment(make([]byte, attachmentMaxInputBytes+1), "image/png")
	if err == nil || !strings.Contains(err.Error(), "图片过大") || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("超字节上限应明确报错，got %v", err)
	}
	_, err = a.storeAttachment(pngHeaderOnly(t, 20000, 10000), "image/png")
	if err == nil || !strings.Contains(err.Error(), "尺寸过大") {
		t.Fatalf("超像素上限应明确报错，got %v", err)
	}
	// 解不了码（只有头）但尺寸合法的小文件：原样返回不报错，内联处按体积决定
	if _, _, err := fitForInline(pngHeaderOnly(t, 100, 100), "image/png"); err != nil {
		t.Fatalf("解码失败不该报错（交给体积判断），got %v", err)
	}
}

// TestInlineSkipsUnshrinkableWebp webp 没有标准库解码器、缩不动：超预算的不内联
// （read_image 文案会说明），预算内的照常内联。
func TestInlineSkipsUnshrinkableWebp(t *testing.T) {
	a := newCompactTestApp(t)
	webp := func(n int) []byte {
		b := make([]byte, n)
		copy(b, "RIFF\x00\x00\x00\x00WEBPVP8 ")
		return b
	}
	att, err := a.storeAttachment(webp(attachmentMaxInlineB64), "image/webp") // 原始 400KB → base64 533K，超预算
	if err != nil {
		t.Fatal(err)
	}
	if att.MediaType != "image/webp" || att.Bytes != attachmentMaxInlineB64 {
		t.Fatalf("webp 应原样入库：%s %d", att.MediaType, att.Bytes)
	}
	msgs := func(att *Attachment) []Message {
		return []Message{{Role: "tool", ToolCallID: "c", Content: imageEnvelope(att, "x.webp")}}
	}
	out := inlineImagesForRequest(a, msgs(att), LLMTarget{SupportsImages: true})
	if n, _ := countInlined(out); n != 0 || len(out) != 1 {
		t.Fatalf("超预算的 webp 不应内联：%+v", out)
	}
	if note := inlineNote(att, LLMTarget{SupportsImages: true}); !strings.Contains(note, "没有内联") {
		t.Fatalf("read_image 应说明未内联：%q", note)
	}
	small, _ := a.storeAttachment(webp(1000), "image/webp")
	out = inlineImagesForRequest(a, msgs(small), LLMTarget{SupportsImages: true})
	if n, _ := countInlined(out); n != 1 {
		t.Fatalf("预算内的 webp 应照常内联")
	}
}

// TestInlineShrinksLegacyOversized 老版本入库、没经过体检的大图（直接写进附件库）：
// 内联时现场缩一次并缓存，老会话里躺着的大图不再让每一步都炸。
func TestInlineShrinksLegacyOversized(t *testing.T) {
	a := newCompactTestApp(t)
	raw := encodeJPEG(genNoiseImage(2800, 2100, 3), 92)
	rel := "ab/legacy-big.jpg"
	full := filepath.Join(a.attachmentsDir(), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, raw, 0644); err != nil {
		t.Fatal(err)
	}
	att := &Attachment{File: rel, MediaType: "image/jpeg", Bytes: len(raw)}
	msgs := []Message{{Role: "tool", ToolCallID: "c", Content: imageEnvelope(att, "legacy.jpg")}}
	for i := 0; i < 2; i++ { // 第二次走缓存，结果一致
		out := inlineImagesForRequest(a, msgs, LLMTarget{SupportsImages: true})
		n, b64 := countInlined(out)
		if n != 1 || b64 > attachmentMaxInlineB64 {
			t.Fatalf("老版本大图应缩后内联（第 %d 次）：n=%d b64=%d", i+1, n, b64)
		}
	}
	if _, hit := inlineCache.Load(rel); !hit {
		t.Fatalf("缩过的结果应缓存")
	}
	inlineCache.Delete(rel)
}

// TestInlinePerRequestCap 张数上限：第 9 张起留在信封里不内联（最老的先内联）。
func TestInlinePerRequestCap(t *testing.T) {
	a := newCompactTestApp(t)
	raw := encodePNGBytes(t, genNoiseImage(20, 20, 4))
	att, _ := a.storeAttachment(raw, "image/png")
	var msgs []Message
	for i := 0; i < attachmentMaxPerRequest+2; i++ {
		msgs = append(msgs, Message{Role: "tool", ToolCallID: "c", Content: imageEnvelope(att, "p.png")})
	}
	out := inlineImagesForRequest(a, msgs, LLMTarget{SupportsImages: true})
	if total, _ := countInlined(out); total != attachmentMaxPerRequest {
		t.Fatalf("应恰好内联 %d 张，got %d", attachmentMaxPerRequest, total)
	}
}

// TestHistoryToolsStripImageEnvelopes 历史查询只给引用不给信封：read_session_messages
// （窗口 / 单条 / 倒序三条路）与 search_sessions 的片段里都不残留
// `<image attachment=…>`，内联正则无从匹配——长期助理读别的会话不会再把原图
// 带进自己的请求。
func TestHistoryToolsStripImageEnvelopes(t *testing.T) {
	a := newCompactTestApp(t)
	sess := &SessionMeta{ID: "s-img", Title: "带图的会话", Mode: "task", Workspace: a.Cfg.Workspace, CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	env := `<image attachment="ed/edd548.jpg" mediaType="image/jpeg" width="8000" height="6000" bytes="6230000" path="tmp/大图.jpg" />` + "\n来源文件：tmp/大图.jpg\n格式：image/jpeg"
	if err := a.Store.AppendRow(sess.ID, &StoredMessage{Message: TextMessage("user", "看图"), Kind: kindChat, CreatedAt: now()}); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.AppendRow(sess.ID, &StoredMessage{Message: Message{Role: "tool", ToolCallID: "c", Content: env}, Kind: kindChat, CreatedAt: now()}); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.AppendRow(sess.ID, &StoredMessage{Message: TextMessage("assistant", "这是张 8000×6000 的照片"), Kind: kindChat, CreatedAt: now()}); err != nil {
		t.Fatal(err)
	}
	tc := &toolContext{app: a, sess: sess}
	outs := map[string]string{}
	var err error
	outs["window"], err = runReadSessionMessages(tc, json.RawMessage(`{"session_id":"s-img","from_seq":1}`))
	if err != nil {
		t.Fatal(err)
	}
	outs["single"], err = runReadSessionMessages(tc, json.RawMessage(`{"session_id":"s-img","seq":2}`))
	if err != nil {
		t.Fatal(err)
	}
	outs["tail"], err = runReadSessionMessages(tc, json.RawMessage(`{"session_id":"s-img"}`))
	if err != nil {
		t.Fatal(err)
	}
	outs["search"], err = runSearchSessions(tc, json.RawMessage(`{"query":"image attachment","context":400}`))
	if err != nil {
		t.Fatal(err)
	}
	for name, out := range outs {
		if n := len(imageEnvelopeRe.FindAllString(out, -1)); n != 0 {
			t.Fatalf("%s 路径仍含 %d 个信封（会被内联）：\n%s", name, n, out)
		}
		if !strings.Contains(out, "tmp/大图.jpg") || !strings.Contains(out, "历史读取不带原图") {
			t.Fatalf("%s 路径应保留图片引用与说明：\n%s", name, out)
		}
	}
}

// TestUpstreamErrTextImageHints 上游报错带图片线索：413 说清是请求体过大（网关的
// HTML 页不原样甩给用户），带图的 400/422 补一句可能被图片拖累；其余原样。
// 413/415/422 是请求级错误，不重试。
func TestUpstreamErrTextImageHints(t *testing.T) {
	s := upstreamErrText(413, []byte("<html><body>413 Request Entity Too Large</body></html>"), 1)
	if !strings.Contains(s, "413") || !strings.Contains(s, "请求体过大") || strings.Contains(s, "<html") {
		t.Fatalf("413 应给人话而非 HTML：%q", s)
	}
	if s := upstreamErrText(400, []byte(`{"error":{"message":"invalid image"}}`), 2); !strings.Contains(s, "invalid image") || !strings.Contains(s, "2 张图片") {
		t.Fatalf("带图 400 应附图片线索：%q", s)
	}
	if s := upstreamErrText(401, []byte(`{"message":"bad key"}`), 2); strings.Contains(s, "张图片") || !strings.Contains(s, "bad key") {
		t.Fatalf("401 不该乱加图片线索：%q", s)
	}
	if s := upstreamErrText(400, []byte(`{"message":"bad request"}`), 0); strings.Contains(s, "张图片") {
		t.Fatalf("无图请求不该加图片线索：%q", s)
	}
	for _, sc := range []int{413, 415, 422} {
		if isRetryableModelErr(errors.New(upstreamErrText(sc, nil, 1))) {
			t.Fatalf("%d 不该重试", sc)
		}
	}
	if !isRetryableModelErr(errors.New(upstreamErrText(500, nil, 1))) {
		t.Fatalf("5xx 仍应重试")
	}
}
