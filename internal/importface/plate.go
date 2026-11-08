package importface

import (
	"encoding/json"
	"fmt"
	"image"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"

	"gocv.io/x/gocv"
)

// 车牌链路与 web/js/plate.js 完全一致：
//
//	plate_det.onnx：YOLO 检测 640×640 letterbox，输出 [1, 25200, 15]（cx,cy,w,h,conf + 5 个关键点）
//	plate_rec.onnx：CRNN + CTC 识别 48×160，输出 [1,T,C]（C = len(chars)）
const (
	plateDetSize  = 640
	plateRecH     = 48
	plateRecW     = 160
	plateConfMin  = 0.30
	plateNMSIou   = 0.55
	plateMaxBox   = 5
	plateMinChars = 5
)

// PlateHit 是一次车牌识别结果。
type PlateHit struct {
	Text string
	Conf float64
	Rect image.Rectangle // 在原图上的检测框
}

// PlateEngine 封装车牌检测 + 识别两个 ONNX 模型（OpenCV DNN，CPU）。
type PlateEngine struct {
	det   *gocv.Net
	rec   *gocv.Net
	chars []string
}

// NewPlateEngine 从模型目录加载 plate_det.onnx / plate_rec.onnx / chars.json。
func NewPlateEngine(modelsDir string) (*PlateEngine, error) {
	detPath := filepath.Join(modelsDir, "plate_det.onnx")
	recPath := filepath.Join(modelsDir, "plate_rec.onnx")
	charsPath := filepath.Join(modelsDir, "chars.json")

	for _, p := range []string{detPath, recPath, charsPath} {
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("车牌模型文件缺失: %s", p)
		}
	}

	det := gocv.ReadNetFromONNX(detPath)
	if det.Empty() {
		return nil, fmt.Errorf("加载失败: %s", detPath)
	}
	rec := gocv.ReadNetFromONNX(recPath)
	if rec.Empty() {
		det.Close()
		return nil, fmt.Errorf("加载失败: %s", recPath)
	}

	var chars []string
	b, err := os.ReadFile(charsPath)
	if err != nil {
		det.Close()
		rec.Close()
		return nil, err
	}
	if err := json.Unmarshal(b, &chars); err != nil {
		det.Close()
		rec.Close()
		return nil, fmt.Errorf("解析 chars.json 失败: %w", err)
	}
	if len(chars) == 0 {
		det.Close()
		rec.Close()
		return nil, fmt.Errorf("chars.json 为空")
	}

	log.Printf("[plate] 车牌模型就绪（%d 个字符）", len(chars))
	return &PlateEngine{det: &det, rec: &rec, chars: chars}, nil
}

// Close 释放两个推理网络。
func (e *PlateEngine) Close() {
	if e == nil {
		return
	}
	if e.det != nil {
		e.det.Close()
	}
	if e.rec != nil {
		e.rec.Close()
	}
}

// DetectAndRecognize 在一帧画面里检测所有车牌并逐块识别。
func (e *PlateEngine) DetectAndRecognize(img gocv.Mat) []PlateHit {
	if e == nil || e.det == nil || e.rec == nil {
		return nil
	}
	boxes := e.detect(img)
	hits := make([]PlateHit, 0, len(boxes))
	for _, b := range boxes {
		if b.Dx() < 24 || b.Dy() < 8 {
			continue
		}
		text, conf := e.recognize(img, b)
		if len([]rune(text)) < plateMinChars {
			continue
		}
		// 识别置信度过低直接丢弃，避免把"安全出口"等绿色标牌当车牌
		if conf < 0.35 {
			continue
		}
		hits = append(hits, PlateHit{Text: text, Conf: conf, Rect: b})
	}
	return hits
}

// detect 640×640 letterbox 检测，返回原图坐标下的车牌框（已 NMS）。
func (e *PlateEngine) detect(img gocv.Mat) []image.Rectangle {
	vw, vh := img.Cols(), img.Rows()
	r := math.Min(float64(plateDetSize)/float64(vw), float64(plateDetSize)/float64(vh))
	nw, nh := int(math.Round(float64(vw)*r)), int(math.Round(float64(vh)*r))
	dw, dh := (plateDetSize-nw)/2, (plateDetSize-nh)/2

	canvas := gocv.NewMatWithSize(plateDetSize, plateDetSize, gocv.MatTypeCV8UC3) // 全黑
	defer canvas.Close()
	resized := gocv.NewMat()
	defer resized.Close()
	gocv.Resize(img, &resized, image.Pt(nw, nh), 0, 0, gocv.InterpolationLinear)
	roi := canvas.Region(image.Rect(dw, dh, dw+nw, dh+nh))
	resized.CopyTo(&roi)
	roi.Close()

	blob := gocv.BlobFromImage(canvas, 1.0/255, image.Pt(plateDetSize, plateDetSize),
		gocv.NewScalar(0, 0, 0, 0), true, false)
	defer blob.Close()
	e.det.SetInput(blob, "")
	out := e.det.Forward("")
	defer out.Close()
	if out.Empty() {
		return nil
	}
	data, err := out.DataPtrFloat32()
	if err != nil {
		return nil
	}

	type cand struct {
		rect  image.Rectangle
		score float64
	}
	var cands []cand
	for o := 0; o+15 <= len(data); o += 15 {
		conf := float64(data[o+4])
		if conf < plateConfMin {
			continue
		}
		cx := (float64(data[o]) - float64(dw)) / r
		cy := (float64(data[o+1]) - float64(dh)) / r
		w := float64(data[o+2]) / r
		h := float64(data[o+3]) / r
		if w < 12 || h < 4 || w > float64(vw)*1.1 {
			continue
		}

		minX, minY := float64(vw), float64(vh)
		maxX, maxY := 0.0, 0.0
		validPts := 0
		for k := 0; k < 5; k++ {
			// 模型对不存在的关键点会输出 (≈0, ≈0)，必须忽略，否则会把检测框撑到整张图
			if data[o+5+k*2] <= 1 && data[o+6+k*2] <= 1 {
				continue
			}
			px := (float64(data[o+5+k*2]) - float64(dw)) / r
			py := (float64(data[o+6+k*2]) - float64(dh)) / r
			if px != px || py != py {
				continue
			}
			validPts++
			minX, maxX = math.Min(minX, px), math.Max(maxX, px)
			minY, maxY = math.Min(minY, py), math.Max(maxY, py)
		}
		x1, y1, x2, y2 := cx-w/2, cy-h/2, cx+w/2, cy+h/2
		// 至少 4 个有效角点才用关键点框（贴合倾斜车牌），否则退回检测框
		if validPts >= 4 && maxX-minX > 4 && maxY-minY > 2 {
			x1, y1, x2, y2 = minX, minY, maxX, maxY
		}
		x1, y1 = math.Max(0, x1), math.Max(0, y1)
		x2, y2 = math.Min(float64(vw), x2), math.Min(float64(vh), y2)
		if x2-x1 < 8 || y2-y1 < 4 {
			continue
		}
		cands = append(cands, cand{
			rect:  image.Rect(int(x1), int(y1), int(x2), int(y2)),
			score: conf,
		})
	}

	// NMS（按 score 降序）
	sort.Slice(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	var keep []image.Rectangle
	for _, c := range cands {
		dup := false
		for _, k := range keep {
			if rectIou(c.rect, k) > plateNMSIou {
				dup = true
				break
			}
		}
		if !dup {
			keep = append(keep, c.rect)
		}
		if len(keep) >= plateMaxBox {
			break
		}
	}
	return keep
}

// recognize 把车牌框裁剪后送 CRNN，贪心 CTC 解码。
func (e *PlateEngine) recognize(img gocv.Mat, box image.Rectangle) (string, float64) {
	vw, vh := img.Cols(), img.Rows()
	pad := math.Max(2, float64(box.Dy())*0.08)
	sx := math.Max(0, float64(box.Min.X)-pad)
	sy := math.Max(0, float64(box.Min.Y)-pad)
	sw := math.Min(float64(vw)-sx, float64(box.Dx())+pad*2)
	sh := math.Min(float64(vh)-sy, float64(box.Dy())+pad*2)
	if sw <= 0 || sh <= 0 {
		return "", 0
	}

	ratio := sw / sh
	resizedW := int(math.Round(plateRecH * ratio))
	resizedW = max(20, min(plateRecW, resizedW))

	small := gocv.NewMat()
	defer small.Close()
	crop := img.Region(image.Rect(int(sx), int(sy), int(sx+sw), int(sy+sh)))
	defer crop.Close()
	gocv.Resize(crop, &small, image.Pt(resizedW, plateRecH), 0, 0, gocv.InterpolationLinear)

	// 160 宽黑底画布，右侧补黑（与 JS 一致：补零 → 归一化后为 -1）
	canvas := gocv.NewMatWithSize(plateRecH, plateRecW, gocv.MatTypeCV8UC3)
	defer canvas.Close()
	dst := canvas.Region(image.Rect(0, 0, resizedW, plateRecH))
	small.CopyTo(&dst)
	dst.Close()

	blob := gocv.BlobFromImage(canvas, 1.0/127.5, image.Pt(plateRecW, plateRecH),
		gocv.NewScalar(127.5, 127.5, 127.5, 0), true, false)
	defer blob.Close()
	e.rec.SetInput(blob, "")
	out := e.rec.Forward("")
	defer out.Close()
	if out.Empty() {
		return "", 0
	}
	data, err := out.DataPtrFloat32()
	if err != nil {
		return "", 0
	}

	C := len(e.chars)
	T := len(data) / C
	// 模型输出形状为 [1,T,C]；C 必须以模型维度为准（chars.json 只有 77 项，
	// 而模型通常是 78 类 = blank + 77 字符），否则按 77 取步长会整体错位。
	if dims := out.Size(); len(dims) >= 3 {
		T, C = dims[1], dims[2]
	}
	var text []rune
	var confSum float64
	var confN int
	prev := -1
	for t := 0; t < T; t++ {
		base := t * C
		best, bestIdx := 0.0, 0
		sum := 0.0
		for c := 0; c < C; c++ {
			v := float64(data[base+c])
			sum += v
			if v > best {
				best, bestIdx = v, c
			}
		}
		if bestIdx == 0 { // blank
			prev = 0
			continue
		}
		if bestIdx == prev {
			continue
		}
		prev = bestIdx
		if bestIdx >= len(e.chars) {
			continue
		}
		ch := e.chars[bestIdx]
		if ch == "" {
			continue
		}
		text = append(text, []rune(ch)...)
		if sum > 0 {
			confSum += best / sum
		}
		confN++
	}
	conf := 0.0
	if confN > 0 {
		conf = confSum / float64(confN)
	}
	return string(text), conf
}

// rectIou 计算两个矩形的交并比。
func rectIou(a, b image.Rectangle) float64 {
	x1 := max(a.Min.X, b.Min.X)
	y1 := max(a.Min.Y, b.Min.Y)
	x2 := min(a.Max.X, b.Max.X)
	y2 := min(a.Max.Y, b.Max.Y)
	inter := float64(max(0, x2-x1) * max(0, y2-y1))
	union := float64(a.Dx()*a.Dy() + b.Dx()*b.Dy())
	union -= inter
	if union <= 0 {
		return 0
	}
	return inter / union
}
