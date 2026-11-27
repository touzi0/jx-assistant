package main

// 附件库：read_image 等工具产出的图片按内容寻址（SHA-256）落盘到
// data/attachments/，工具结果里只保留一段文本信封（引用附件相对路径）。
// 前端经 GET /attachments/<路径> 直接查看；发给模型时按当前模型的视觉能力
// 处理——支持图片的模型在请求组装时按预算内联为 base64 内容块，纯文本模型
// 保留信封文本（模型仍知道有一张图、存放在哪）。
//
// 入库前统一「体检」（fitForInline）：原图再大，内联给模型的也只是一份缩到
// 预算内的副本——单张 base64 不超过 attachmentMaxInlineB64、最大边不超过
// attachmentMaxEdge，必要时降质、再降分辨率。设计动机（2026-11-27 真 bug）：
// 一张 5MB+ 的高分辨率图原样 base64 后请求体近 10MB，视觉模型的网关直接拒收
// （400/413），而这张图只要留在上下文里，**之后每一步**都带着它、步步失败；
// 长期助理读那段历史时信封又被原样内联，连别的会话也跟着崩。预算与缩放是
// 这条链上唯一的闸门，放在入库处一次做对，内联处再做一道兜底（老版本入库的
// 大图现场缩一次并缓存）。历史查询工具读回的文本不含信封（historytools.go），
// 读历史永远不会把原图带进上下文。
// 结构化输出与预算压缩的思路借鉴 ZCode 开源版（Apache-2.0）的 read-image
// 实现：最大边长 / 字节预算 / token≈base64 字符数×0.125 三重约束。

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	// 注册 PNG/GIF 解码器（宽高解析与转码用）；JPEG 同时用于超预算转码
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

const (
	// attachmentMaxInputBytes 原始图片文件的字节上限：再大的图解码要吃几百 MB
	// 内存，明确报错让用户先在本地缩小。
	attachmentMaxInputBytes = 20 << 20
	// attachmentMaxPixels 可解码的像素上限（8000×10000）：解码内存 ≈ 像素×4 字节。
	attachmentMaxPixels = 80 << 20
	// attachmentMaxInlineB64 单张图内联给模型的 base64 字符预算（≈300KB 原始字节）：
	// 主流视觉网关单图上限 5MB 左右，8 张满额也不到 4MB 请求体。
	attachmentMaxInlineB64 = 400 << 10
	// attachmentMaxEdge 内联给模型的最大边长（像素）：各家属视觉模型内部都按
	// 1~2K 像素取样，更大的原图只多付 token 不多得分辨率，还有厂商按像素拒绝。
	attachmentMaxEdge = 2048
	// attachmentJPEGQuality 超预算转 JPEG 的起始质量档。
	attachmentJPEGQuality = 85
	// attachmentMaxPerRequest 单次模型请求最多内联的图片张数，
	// 超出的只保留信封占位（DSH image-offload 同思路：卸载最老的）。
	attachmentMaxPerRequest = 8
)

// 缩图阶梯：同一分辨率先降质量（85→70），仍超预算再降一级分辨率；纯噪声图
// 到最后一级才动用 55 质量兜底。第一份进预算的即采用。
var (
	attachmentEdgeSteps    = []int{attachmentMaxEdge, 1536, 1024, 768, 512}
	attachmentQualitySteps = []int{attachmentJPEGQuality, 70}
)

// Attachment 是一张已入库图片的描述。
type Attachment struct {
	File      string // 相对 data/attachments/ 的存放路径（含扩展名）
	MediaType string
	Width     int
	Height    int
	Bytes     int
	SHA256    string
	// ShrunkFrom 非空表示入库时做了缩放/转码：记原图的尺寸与字节数，read_image
	// 据此向模型交代（它看到的是缩略版，看不清细节时知道原因）。
	ShrunkFrom string
}

// imageEnvelopeRe 从工具结果的文本里提取图片信封；捕获组是附件相对路径。
var imageEnvelopeRe = regexp.MustCompile(`<image attachment="([^"]+)"[^>]*/>`)

func (a *App) attachmentsDir() string {
	return filepath.Join(a.Cfg.DataDir, "attachments")
}

// sniffImageMediaType 按魔数识别图片类型；空串表示不是受支持的图片。
func sniffImageMediaType(b []byte) string {
	switch {
	case len(b) >= 8 && b[0] == 0x89 && b[1] == 'P' && b[2] == 'N' && b[3] == 'G':
		return "image/png"
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg"
	case len(b) >= 6 && b[0] == 'G' && b[1] == 'I' && b[2] == 'F':
		return "image/gif"
	case len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

func mediaTypeExt(mt string) string {
	switch mt {
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}

// imageDims 解出图片宽高；标准库不认识的格式（如 webp）返回 0,0。
func imageDims(b []byte) (int, int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

func base64Len(rawLen int) int { return (rawLen + 2) / 3 * 4 }

// ---------- 体检：缩到模型能吃的尺寸 ----------

// fitForInline 把图片处理到内联预算内：只在超字节预算或超最大边长时动手，
// 只缩不放。返回处理后的字节与媒体类型。解不了码（webp 没有标准库解码器、
// 截断文件）时原样返回——是否内联由调用方按体积决定（read_image 会向模型
// 说明）。像素超限直接报错：解码一张上亿像素的图要几百 MB 内存，不值得。
func fitForInline(raw []byte, mediaType string) ([]byte, string, error) {
	w, h := imageDims(raw)
	if w <= 0 || h <= 0 {
		return raw, mediaType, nil
	}
	if w*h > attachmentMaxPixels {
		return nil, "", fmt.Errorf("图片尺寸过大（%d×%d，上限 %d 像素），请先在本地缩小后再给模型看", w, h, attachmentMaxPixels)
	}
	overBytes := base64Len(len(raw)) > attachmentMaxInlineB64
	overEdge := max(w, h) > attachmentMaxEdge
	if !overBytes && !overEdge {
		return raw, mediaType, nil
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return raw, mediaType, nil
	}
	if overEdge {
		img = resizeDown(img, attachmentMaxEdge)
	}
	// 重编码前按 EXIF 转正：解码器不理 EXIF（浏览器显示时会按标签转正，解码出的
	// 像素不会），缩略副本不转正的话用户和模型看到的都是躺倒的照片。先缩后转
	// 与先转后缩等价（面积平均对 90° 旋转/镜像对称），大图省一次全幅遍历。
	if o := exifOrientation(raw); o > 1 {
		img = orientImage(img, o)
	}
	// 只因边长超限而缩的 PNG：仍存 PNG（文字截图无损清晰），进预算即可
	if mediaType == "image/png" {
		if buf := encodePNG(img); base64Len(len(buf)) <= attachmentMaxInlineB64 {
			return buf, "image/png", nil
		}
	}
	var flat image.Image = flattenOnWhite(img) // JPEG 没有透明通道，透明底压成白色而不是黑块
	for _, edge := range attachmentEdgeSteps {
		if max(flat.Bounds().Dx(), flat.Bounds().Dy()) > edge {
			flat = resizeDown(flat, edge)
		}
		for _, q := range attachmentQualitySteps {
			if buf := encodeJPEG(flat, q); base64Len(len(buf)) <= attachmentMaxInlineB64 {
				return buf, "image/jpeg", nil
			}
		}
	}
	// 病态图（纯噪声）缩到 512px 降两档质量仍压不进预算：55 质量兜底；还超的话
	// inlineSized 按预算判为不可内联（实际到不了这一步——512px 的 q55 很小）
	return encodeJPEG(flat, 55), "image/jpeg", nil
}

func encodeJPEG(img image.Image, quality int) []byte {
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality})
	return buf.Bytes()
}

func encodePNG(img image.Image) []byte {
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// flattenOnWhite 把图铺到白底上（透明像素压成白色），JPEG 转码前用。
func flattenOnWhite(src image.Image) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, src.Bounds().Dx(), src.Bounds().Dy()))
	draw.Draw(dst, dst.Rect, image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(dst, dst.Rect, src, src.Bounds().Min, draw.Over)
	return dst
}

// resizeDown 按面积平均把图等比缩到最大边不超过 maxEdge（只缩不放）。纯 Go
// 实现（项目不引 x/image）：视觉模型收到图后自己也按 1~2K 像素取样，面积
// 平均的抗锯齿对缩图足够，文字截图缩后仍清晰。源图每个像素恰好落进一个
// 目标格，整张只遍历一遍。
func resizeDown(src image.Image, maxEdge int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 || max(w, h) <= maxEdge {
		return src
	}
	scale := float64(maxEdge) / float64(max(w, h))
	dw := max(1, int(math.Round(float64(w)*scale)))
	dh := max(1, int(math.Round(float64(h)*scale)))
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		sy0 := b.Min.Y + y*h/dh
		sy1 := b.Min.Y + (y+1)*h/dh
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for x := 0; x < dw; x++ {
			sx0 := b.Min.X + x*w/dw
			sx1 := b.Min.X + (x+1)*w/dw
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var r, g, bl, a uint64
			n := uint64(0)
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					pr, pg, pb, pa := src.At(sx, sy).RGBA()
					r, g, bl, a, n = r+uint64(pr), g+uint64(pg), bl+uint64(pb), a+uint64(pa), n+1
				}
			}
			// At().RGBA() 给的是预乘 alpha 的 16 位值：预乘值取平均仍是合法的
			// 预乘色，color.RGBAModel 换算时统一除以 257 落到 8 位
			dst.Set(x, y, color.RGBAModel.Convert(color.RGBA64{R: uint16(r / n), G: uint16(g / n), B: uint16(bl / n), A: uint16(a / n)}))
		}
	}
	return dst
}

// exifOrientation 读 JPEG 的 EXIF 方向标签（APP1 段 → TIFF IFD0 的 0x0112）：
// 1 正常，2-8 为镜像/旋转；读不到按 1。只看第一层 IFD（手机照片都写在这）。
func exifOrientation(b []byte) int {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 1
	}
	for i := 2; i+4 <= len(b); {
		if b[i] != 0xFF {
			return 1
		}
		marker := b[i+1]
		if marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			i += 2 // 无长度字段的独立标记
			continue
		}
		size := int(b[i+2])<<8 | int(b[i+3])
		if size < 2 {
			return 1
		}
		end := i + 2 + size
		if end > len(b) {
			return 1
		}
		if marker == 0xE1 {
			if seg := b[i+4 : end]; len(seg) >= 8 && string(seg[:6]) == "Exif\x00\x00" {
				return tiffOrientation(seg[6:])
			}
		}
		if marker == 0xDA { // 到了扫描数据：后面没有元数据了
			return 1
		}
		i = end
	}
	return 1
}

func tiffOrientation(t []byte) int {
	var be bool
	switch {
	case len(t) >= 8 && string(t[:2]) == "MM":
		be = true
	case len(t) >= 8 && string(t[:2]) == "II":
	default:
		return 1
	}
	u16 := func(p []byte) int {
		if be {
			return int(p[0])<<8 | int(p[1])
		}
		return int(p[1])<<8 | int(p[0])
	}
	u32 := func(p []byte) int {
		v := 0
		for k := 0; k < 4; k++ {
			if be {
				v = v<<8 | int(p[k])
			} else {
				v = v<<8 | int(p[3-k])
			}
		}
		return v
	}
	if u16(t[2:4]) != 42 {
		return 1
	}
	off := u32(t[4:8])
	if off+2 > len(t) {
		return 1
	}
	n := u16(t[off : off+2])
	for k := 0; k < n; k++ {
		e := off + 2 + k*12
		if e+12 > len(t) {
			return 1
		}
		if u16(t[e:e+2]) == 0x0112 {
			return u16(t[e+8 : e+10])
		}
	}
	return 1
}

// orientImage 按 EXIF 方向把图转正（2 水平镜像、3 旋转 180、4 垂直镜像、5 转置、
// 6 顺时针 90、7 反对角转置、8 逆时针 90）。
func orientImage(src image.Image, o int) image.Image {
	if o < 2 || o > 8 {
		return src
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			var sx, sy int
			switch o {
			case 2:
				sx, sy = w-1-x, y
			case 3:
				sx, sy = w-1-x, h-1-y
			case 4:
				sx, sy = x, h-1-y
			case 5:
				sx, sy = y, x
			case 6:
				sx, sy = y, h-1-x
			case 7:
				sx, sy = w-1-y, h-1-x
			case 8:
				sx, sy = w-1-y, x
			}
			dst.Set(x, y, src.At(b.Min.X+sx, b.Min.Y+sy))
		}
	}
	return dst
}

// ---------- 入库 ----------

// storeAttachment 把图片字节入库：先体检（fitForInline：超预算/超边长的缩到
// 预算内），再内容寻址落盘，同内容天然去重。成功后返回附件描述。
func (a *App) storeAttachment(raw []byte, mediaType string) (*Attachment, error) {
	if mediaType == "" {
		return nil, fmt.Errorf("不是受支持的图片格式（支持 png / jpg / gif / webp）")
	}
	if len(raw) > attachmentMaxInputBytes {
		return nil, fmt.Errorf("图片过大（%s，上限 %s），请先在本地缩小后再给模型看", humanSize(int64(len(raw))), humanSize(attachmentMaxInputBytes))
	}
	data, mt, err := fitForInline(raw, mediaType)
	if err != nil {
		return nil, err
	}
	att := &Attachment{MediaType: mt, Bytes: len(data)}
	att.Width, att.Height = imageDims(data)
	if ow, oh := imageDims(raw); len(data) != len(raw) || att.Width != ow || att.Height != oh {
		att.ShrunkFrom = fmt.Sprintf("%d×%d %s", ow, oh, humanSize(int64(len(raw))))
	}
	sum := sha256.Sum256(data)
	att.SHA256 = hex.EncodeToString(sum[:])
	att.File = filepath.ToSlash(filepath.Join(att.SHA256[:2], att.SHA256+mediaTypeExt(att.MediaType)))

	full := filepath.Join(a.attachmentsDir(), filepath.FromSlash(att.File))
	if _, err := os.Stat(full); err == nil {
		return att, nil // 同内容已入库，直接复用
	}
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(full, data, 0644); err != nil {
		return nil, err
	}
	return att, nil
}

// loadAttachment 读回附件字节（带路径防穿越校验），用于内联与 HTTP 端点。
func (a *App) loadAttachment(rel string) ([]byte, string, error) {
	rel = filepath.ToSlash(filepath.Clean(rel))
	if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return nil, "", fmt.Errorf("非法附件路径")
	}
	full := filepath.Join(a.attachmentsDir(), filepath.FromSlash(rel))
	data, err := os.ReadFile(full)
	if err != nil {
		return nil, "", err
	}
	mt := sniffImageMediaType(data)
	if mt == "" {
		mt = "application/octet-stream"
	}
	return data, mt, nil
}

// inlineCache 老版本入库、没经过体检的大图：内联时现场缩一次并按附件路径
// 缓存（同一张图在上下文里会随每一步请求反复出现，不能每步都重缩）。App
// 级，不随会话；值是「缩后仍超预算」的也照样缓存（空字节），避免反复尝试。
var inlineCache sync.Map // rel → []byte（已缩好的字节；空=不可内联）

// inlineSized 取内联用字节：预算内的原样返回；超预算的走 fitForInline 缩一次。
// 返回 ok=false 表示这张图给不了模型（webp 这类解不了码又超预算的）。
func (a *App) inlineSized(rel string) (data []byte, mt string, ok bool) {
	raw, mt, err := a.loadAttachment(rel)
	if err != nil {
		return nil, "", false
	}
	if base64Len(len(raw)) <= attachmentMaxInlineB64 {
		return raw, mt, true
	}
	if v, hit := inlineCache.Load(rel); hit {
		if b, _ := v.([]byte); len(b) > 0 {
			return b, sniffImageMediaType(b), true
		}
		return nil, "", false
	}
	shrunk, smt, _ := fitForInline(raw, mt)
	if base64Len(len(shrunk)) > attachmentMaxInlineB64 {
		inlineCache.Store(rel, []byte{})
		return nil, "", false
	}
	inlineCache.Store(rel, shrunk)
	return shrunk, smt, true
}

// imageEnvelope 生成写进工具结果文本的图片信封；path 为源文件（供模型与用户对照）。
func imageEnvelope(att *Attachment, srcPath string) string {
	src := strings.ReplaceAll(filepath.ToSlash(srcPath), `"`, `'`)
	return fmt.Sprintf(`<image attachment="%s" mediaType="%s" width="%d" height="%d" bytes="%d" path="%s" />`,
		att.File, att.MediaType, att.Width, att.Height, att.Bytes, src)
}

// inlineNote read_image 结果里向模型交代图片的处理情况：缩过的说明原图规模
// （模型知道看到的是缩略版，看不清细节时会换 OCR 等手段而不是硬猜）；
// 解不了码又超预算的说明没内联（纯信封）——不能让模型以为它「看过」。
func inlineNote(att *Attachment, target LLMTarget) string {
	switch {
	case !target.SupportsImages:
		return "\n当前模型不支持图片，可使用OCR工具"
	case base64Len(att.Bytes) > attachmentMaxInlineB64:
		return fmt.Sprintf("\n（这张图 %s 超出模型单图预算且无法本地压缩（webp 等格式），本次没有内联给你；需要看内容请让用户转成 jpg/png，或改用 OCR 识别文字）", humanSize(int64(att.Bytes)))
	case att.ShrunkFrom != "":
		return fmt.Sprintf("\n（原图 %s，已缩到 %d×%d 后内联给你，细节看不清时可用 OCR 工具识别原文）", att.ShrunkFrom, att.Width, att.Height)
	}
	return ""
}

// mcpImagesEnvelope 把 MCP 工具结果携带的图片入库为附件并生成注入文本段
// （信封 + 说明行；无图片时返回空串）。MCP 服务器（如 Windows-MCP 的
// Screenshot/Snapshot）以 image content 回传截图，入库后与 read_image 走
// 同一条注入链路：inlineImagesForRequest 在请求组装时把信封内联为视觉块，
// 前端在工具卡片渲染原图。信封段应拼在工具文本结果**之前**——规则压缩只
// 保留工具结果的开头片段，放尾部会被长文本截掉。入库失败降级为一行说明，
// 不吞掉工具本身的结果。
func mcpImagesEnvelope(app *App, images []mcpImage) string {
	if len(images) == 0 {
		return ""
	}
	var b strings.Builder
	ok := 0
	for _, img := range images {
		att, err := app.storeAttachment(img.Data, img.MIMEType)
		if err != nil {
			continue
		}
		b.WriteString(imageEnvelope(att, "MCP 工具返回"))
		b.WriteString("\n")
		ok++
	}
	if ok == 0 {
		return fmt.Sprintf("（工具随结果返回了 %d 张图片，均入库失败：格式不受支持或超出大小上限）\n", len(images))
	}
	fmt.Fprintf(&b, "（以上 %d 张图片来自 MCP 工具返回：支持视觉的模型可直接看到内容，用户可在工具卡片里查看原图）", ok)
	return b.String()
}

// inlineImagesForRequest 是发给模型前的最后一步变换：当前模型支持视觉时，
// 把历史里工具结果携带的图片信封内联为 user 角色的 base64 内容块
// （紧跟该步全部工具结果之后；部分兼容端不接受 tool 角色直接携带图片）。
// 纯文本模型原样返回——信封文本本身就是对图片的描述。超过单次张数上限的
// 图片留在信封里不内联（最老的先被卸载）；超预算又缩不动的也不内联。
func inlineImagesForRequest(app *App, msgs []Message, target LLMTarget) []Message {
	if app == nil || !target.SupportsImages {
		return msgs
	}
	out := make([]Message, 0, len(msgs)+2)
	var pending []ContentPart
	flush := func() {
		if len(pending) > 0 {
			out = append(out, Message{Role: "user", Content: pending})
			pending = nil
		}
	}
	inlined := 0
	for _, m := range msgs {
		if m.Role != "tool" {
			flush()
			out = append(out, m)
			continue
		}
		out = append(out, m)
		s, ok := m.Content.(string)
		if !ok {
			continue
		}
		for _, match := range imageEnvelopeRe.FindAllStringSubmatch(s, -1) {
			if inlined >= attachmentMaxPerRequest {
				break // 张数上限已满，本条及后续信封保持占位
			}
			data, mt, ok := app.inlineSized(match[1])
			if !ok {
				continue
			}
			if len(pending) == 0 {
				pending = append(pending, ContentPart{Type: "text", Text: "（上面工具结果里引用的图片，供你查看）"})
			}
			pending = append(pending, ContentPart{
				Type:     "image_url",
				ImageURL: &ImageURL{URL: "data:" + mt + ";base64," + base64.StdEncoding.EncodeToString(data)},
			})
			inlined++
		}
	}
	flush()
	return out
}
