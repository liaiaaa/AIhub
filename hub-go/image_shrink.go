package main

// 入站图片降采样 + 请求体字节闸（2026-11-02）
//
// 起因：Codex 一个会话里堆了 51 张 base64 截图（31MB，占请求体 91.8%），
// 每轮全量重发给 qoder 上游，请求体涨到 30.8MB 被上游 413 拒收：
//
//	{"code":"413","message":"Request body size exceeds maximum allowed 52428800 bytes", ...}
//
// 上游文案写 50MB，但实测 28.9MB 能过、30.9MB 被拒（阈值约 30MiB）。
// 关键教训：**token 预算合法不代表字节预算合法** —— 图片几乎不占 token，
// 却极占字节（该会话 154 字节/token，正常约 4）。上下文压缩按 token 算，
// 永远算不到这些图片头上，于是 Codex 认为「还有 33 万 token 空间」，
// 实际早已越界，连 /compact（本身也要重发整段历史）都会被同一个 413 挡死。
//
// 所以在这里按**字节**兜底：入站先把大图降采样，仍然超限就把最老的图换成占位图。
// 全部只用标准库（image/png + image/jpeg + 自写双线性缩放），不引第三方依赖。

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"strconv"
	"strings"
)

const (
	// 长边上限：截图给模型看，1024 足够；再大基本是浪费字节。
	imgShrinkDefaultMaxSide = 1024
	// JPEG 质量：q80 在截图上肉眼几乎无损，体积约为原图 1/10。
	imgShrinkDefaultQuality = 80
	// 只处理 base64 长度超过这个值（≈24KB 原始字节）的图，小图不动。
	imgShrinkDefaultMinKB = 96
	// 请求体字节上限：上游实测阈值 29–31MB，留安全边际取 24MB。
	payloadLimitDefaultMB = 24
	// PNG 无损压缩后仍超过这个体积，说明更像照片/复杂界面而不是纯文字截图，
	// 退回 JPEG（实测同一批 Codex 截图：纯 PNG 只能压到 54%，退回 JPEG 后约 8%）。
	imgShrinkPNGFallbackKB = 256
)

// 环境变量（都可选）：
//
//	AIHUB_IMG_SHRINK=0        关掉整条降采样逻辑
//	AIHUB_IMG_MAX_SIDE=1024   长边上限
//	AIHUB_IMG_QUALITY=80      JPEG 质量
//	AIHUB_IMG_MIN_KB=96       低于这个 base64 长度的图不动
//	AIHUB_PAYLOAD_LIMIT_MB=24 请求体字节上限（超过就裁旧图）
func imgShrinkEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("AIHUB_IMG_SHRINK")))
	return !(v == "0" || v == "off" || v == "false")
}

func imgShrinkMaxSide() int {
	if n := envInt("AIHUB_IMG_MAX_SIDE", imgShrinkDefaultMaxSide); n >= 64 {
		return n
	}
	return imgShrinkDefaultMaxSide
}

func imgShrinkQuality() int {
	n := envInt("AIHUB_IMG_QUALITY", imgShrinkDefaultQuality)
	if n < 10 || n > 100 {
		return imgShrinkDefaultQuality
	}
	return n
}

func imgShrinkMinBytes() int {
	n := envInt("AIHUB_IMG_MIN_KB", imgShrinkDefaultMinKB)
	if n < 4 {
		n = imgShrinkDefaultMinKB
	}
	return n << 10
}

func payloadLimitBytes() int {
	n := envInt("AIHUB_PAYLOAD_LIMIT_MB", payloadLimitDefaultMB)
	if n < 1 {
		n = payloadLimitDefaultMB
	}
	return n << 20
}

// imgShrinkTriggerBytes 图片总量低于这个值就整体跳过（避免每个请求都白跑一遍
// 解码/重编码：实测 48 张 4MB 截图重编码要 2.6s，普通小请求不该付这个成本）。
func imgShrinkTriggerBytes() int {
	n := envInt("AIHUB_IMG_SHRINK_MIN_MB", 12)
	if n < 0 {
		n = 12
	}
	return n << 20
}

func imgShrinkPNGFallbackBytes() int {
	n := envInt("AIHUB_IMG_PNG_MAX_KB", imgShrinkPNGFallbackKB)
	if n < 16 {
		n = imgShrinkPNGFallbackKB
	}
	return n << 10
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// imagePlaceholderURL 1x1 透明 PNG：老图被裁掉时用它顶上，保证请求体结构仍然合法
// （上游对 image_url 的空值/非 data URL 会挑刺，塞一张合法的最小图最稳）。
const imagePlaceholderURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

// splitDataURL 拆 "data:image/png;base64,xxxx"。
func splitDataURL(s string) (mime, b64 string, ok bool) {
	if !strings.HasPrefix(s, "data:image/") {
		return "", "", false
	}
	i := strings.Index(s, ";base64,")
	if i < 0 {
		return "", "", false
	}
	return s[len("data:"):i], s[i+len(";base64,"):], true
}

// shrinkOneDataURL 把单张图降采样；返回新字符串与「改没改」。
func shrinkOneDataURL(s string, maxSide, quality, minBytes int) (string, bool) {
	mime, b64, ok := splitDataURL(s)
	if !ok || len(b64) < minBytes {
		return s, false
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return s, false
	}
	src, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return s, false
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return s, false
	}
	// 缩到长边 <= maxSide（本来就更小的话，仍可能因为 PNG 无损而很大，走重编码）
	nw, nh := w, h
	longest := w
	if h > longest {
		longest = h
	}
	if longest > maxSide {
		scale := float64(maxSide) / float64(longest)
		nw = int(math.Round(float64(w) * scale))
		nh = int(math.Round(float64(h) * scale))
		if nw < 1 {
			nw = 1
		}
		if nh < 1 {
			nh = 1
		}
	}
	var out image.Image = src
	if nw != w || nh != h {
		out = scaleBilinear(src, nw, nh)
		format = "scaled" // 缩放后一律重新编码
	}

	var buf bytes.Buffer
	switch {
	case strings.Contains(mime, "jpeg") || strings.Contains(mime, "jpg"):
		if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: quality}); err != nil {
			return s, false
		}
	case format == "scaled" || strings.Contains(mime, "png"):
		// PNG 源（多为 UI 截图、含文字）：先保持无损，避免文字糊掉。
		enc := png.Encoder{CompressionLevel: png.BestSpeed}
		if err := enc.Encode(&buf, out); err != nil {
			return s, false
		}
		// 两种情况退回 JPEG：
		//   ① 无损重编码没收益（照片类本来就不是 PNG 擅长）；
		//   ② 压到长边上限后 PNG 仍然很大（复杂界面截图）——这时 JPEG 的
		//      文字损失可接受，体积收益是数量级的（实测 16MB → 2.4MB）。
		if buf.Len() >= len(raw) || buf.Len() > imgShrinkPNGFallbackBytes() {
			var jb bytes.Buffer
			if err := jpeg.Encode(&jb, out, &jpeg.Options{Quality: quality}); err != nil {
				return s, false
			}
			if jb.Len() < buf.Len() {
				buf = jb
			} else if buf.Len() >= len(raw) {
				return s, false
			}
		}
	default:
		return s, false
	}
	if buf.Len() >= len(raw) {
		return s, false // 没有收益就别动
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), true
}

// scaleBilinear 双线性缩放（标准库没有缩放，自己实现，避免引 golang.org/x/image）。
func scaleBilinear(src image.Image, dw, dh int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if sw <= 0 || sh <= 0 || dw <= 0 || dh <= 0 {
		return dst
	}
	srcRGBA, ok := src.(*image.RGBA)
	if !ok {
		srcRGBA = image.NewRGBA(image.Rect(0, 0, sw, sh))
		draw.Draw(srcRGBA, srcRGBA.Bounds(), src, sb.Min, draw.Src)
	}
	xRatio := float64(sw) / float64(dw)
	yRatio := float64(sh) / float64(dh)
	for y := 0; y < dh; y++ {
		sy := (float64(y)+0.5)*yRatio - 0.5
		y0 := int(math.Floor(sy))
		fy := sy - float64(y0)
		if y0 < 0 {
			y0, fy = 0, 0
		}
		y1 := y0 + 1
		if y1 >= sh {
			y1 = sh - 1
		}
		for x := 0; x < dw; x++ {
			sx := (float64(x)+0.5)*xRatio - 0.5
			x0 := int(math.Floor(sx))
			fx := sx - float64(x0)
			if x0 < 0 {
				x0, fx = 0, 0
			}
			x1 := x0 + 1
			if x1 >= sw {
				x1 = sw - 1
			}
			c00 := srcRGBA.RGBAAt(x0, y0)
			c10 := srcRGBA.RGBAAt(x1, y0)
			c01 := srcRGBA.RGBAAt(x0, y1)
			c11 := srcRGBA.RGBAAt(x1, y1)
			mix := func(a, b, c, d uint8) uint8 {
				top := float64(a)*(1-fx) + float64(b)*fx
				bot := float64(c)*(1-fx) + float64(d)*fx
				v := top*(1-fy) + bot*fy
				if v < 0 {
					v = 0
				}
				if v > 255 {
					v = 255
				}
				return uint8(v + 0.5)
			}
			dst.SetRGBA(x, y, color.RGBA{
				R: mix(c00.R, c10.R, c01.R, c11.R),
				G: mix(c00.G, c10.G, c01.G, c11.G),
				B: mix(c00.B, c10.B, c01.B, c11.B),
				A: mix(c00.A, c10.A, c01.A, c11.A),
			})
		}
	}
	return dst
}

// shrinkDataURLImagesIn 递归遍历 JSON 结构，把其中的大图降采样。
// 返回：处理的图片张数、处理前后这些图的总字节数。
func shrinkDataURLImagesIn(v any) (count int, before, after int) {
	if !imgShrinkEnabled() {
		return 0, 0, 0
	}
	// 先量一下图片总量：不够大就不用动（普通请求零成本）
	var slots []imgSlot
	collectImageSlots(v, &slots)
	total := 0
	for _, s := range slots {
		total += len(s.cur)
	}
	if trigger := imgShrinkTriggerBytes(); trigger > 0 && total < trigger {
		return 0, 0, 0
	}
	maxSide, quality, minBytes := imgShrinkMaxSide(), imgShrinkQuality(), imgShrinkMinBytes()
	return shrinkWalk(v, func(s string) (string, bool) {
		ns, changed := shrinkOneDataURL(s, maxSide, quality, minBytes)
		return ns, changed
	})
}

func shrinkWalk(v any, fn func(string) (string, bool)) (count int, before, after int) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if s, ok := val.(string); ok {
				if ns, changed := fn(s); changed {
					t[k] = ns
					count++
					before += len(s)
					after += len(ns)
				}
				continue
			}
			c, b, a := shrinkWalk(val, fn)
			count += c
			before += b
			after += a
		}
	case []any:
		for i, val := range t {
			if s, ok := val.(string); ok {
				if ns, changed := fn(s); changed {
					t[i] = ns
					count++
					before += len(s)
					after += len(ns)
				}
				continue
			}
			c, b, a := shrinkWalk(val, fn)
			count += c
			before += b
			after += a
		}
	}
	return
}

// imgSlot 记录一个可回写的图片字符串位置（按出现顺序收集 = 最老的在前，
// 因为 messages 是 []any，遍历天然有序）。
type imgSlot struct {
	set func(string)
	cur string
}

func collectImageSlots(v any, out *[]imgSlot) {
	switch t := v.(type) {
	case map[string]any:
		for _, k := range sortedKeysOf(t) {
			val := t[k]
			key := k
			parent := t
			if s, ok := val.(string); ok {
				if _, _, ok := splitDataURL(s); ok {
					*out = append(*out, imgSlot{set: func(nv string) { parent[key] = nv }, cur: s})
				}
				continue
			}
			collectImageSlots(val, out)
		}
	case []any:
		for i, val := range t {
			idx := i
			parent := t
			if s, ok := val.(string); ok {
				if _, _, ok := splitDataURL(s); ok {
					*out = append(*out, imgSlot{set: func(nv string) { parent[idx] = nv }, cur: s})
				}
				continue
			}
			collectImageSlots(val, out)
		}
	}
}

func sortedKeysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// 小 map，插入排序足够；避免为一个排序引 sort 以外的依赖
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// trimOldDataURLImagesIn 请求体仍超字节上限时的最后一道闸：**最老优先**把图片
// 换成 1x1 占位图，直到总量落回上限以下。返回被裁掉的张数。
func trimOldDataURLImagesIn(v any, limit int) int {
	if limit <= 0 {
		return 0
	}
	var slots []imgSlot
	collectImageSlots(v, &slots)
	if len(slots) == 0 {
		return 0
	}
	total := 0
	for _, s := range slots {
		total += len(s.cur)
	}
	trimmed := 0
	for _, s := range slots {
		if total <= limit {
			break
		}
		if len(s.cur) <= len(imagePlaceholderURL) {
			continue
		}
		total -= len(s.cur) - len(imagePlaceholderURL)
		s.set(imagePlaceholderURL)
		trimmed++
	}
	return trimmed
}
