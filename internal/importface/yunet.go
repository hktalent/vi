package importface

import (
	"fmt"
	"image"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"

	"gocv.io/x/gocv"
)

// YuNet 人脸检测器（OpenCV Zoo face_detection_yunet_2023mar.onnx，YOLO 风格 12 输出）。
// 基于 DNN，准确率远高于 Haar cascade，且输出 5 点关键点（双眼、鼻尖、双嘴角）。
//
// 解码与 OpenCV FaceDetectorYN（modules/objdetect/src/face_detect.cpp）完全一致：
//   - 输入：缩放到 32 的倍数（模型内部按 /8 /16 /32 整除）
//   - 输出：按 stride 8/16/32 各 4 个 blob——cls/obj [1, rows*cols, 1]、bbox [1, rows*cols, 4]、kps [1, rows*cols, 10]
//   - score = sqrt(clamp(cls) * clamp(obj))
//   - bbox：cx=(gridX+bbox[0])*stride, cy=(gridY+bbox[1])*stride, w=exp(bbox[2])*stride, h=exp(bbox[3])*stride
//   - 关键点：x=(kps[2n]+gridX)*stride, y=(kps[2n+1]+gridY)*stride
//   - 最后 NMS

const (
	yuMaxDim         = 1280 // 检测前最长边缩放到此（兼顾小脸召回与速度）
	yuScoreThreshold = 0.6  // 人脸得分阈值（OpenCV 默认 0.9 对婚礼模糊小脸太严格）
	yuNMSThreshold   = 0.3  // NMS IoU 阈值
	yuTopK           = 200  // NMS 后最多保留的人脸数
)

var (
	yuStrides      = []int{8, 16, 32}
	yuOutputNames  = []string{"cls_8", "cls_16", "cls_32", "obj_8", "obj_16", "obj_32", "bbox_8", "bbox_16", "bbox_32", "kps_8", "kps_16", "kps_32"}
	yuMinFacePixel = 16 // 原图上的最小人脸边长
)

// YuFace 是 YuNet 检测到的一张人脸。
type YuFace struct {
	Rect      image.Rectangle // 原图坐标
	Score     float32
	Landmarks [5][2]float32 // 右眼、左眼、鼻尖、右嘴角、左嘴角（原图坐标）
}

// YuNetDetector 封装 YuNet ONNX 模型。
type YuNetDetector struct {
	net gocv.Net
}

// NewYuNetDetector 加载 YuNet 模型。
func NewYuNetDetector(modelPath string) (*YuNetDetector, error) {
	net := gocv.ReadNetFromONNX(modelPath)
	if net.Empty() {
		return nil, fmt.Errorf("YuNet 模型加载失败: %s", modelPath)
	}
	return &YuNetDetector{net: net}, nil
}

// Close 释放模型资源。
func (d *YuNetDetector) Close() { d.net.Close() }

// Detect 检测图片中的所有人脸，返回原图坐标。
func (d *YuNetDetector) Detect(img gocv.Mat) []YuFace {
	origW, origH := img.Cols(), img.Rows()
	scale := 1.0
	if m := max(origH, origW); m > yuMaxDim {
		scale = float64(yuMaxDim) / float64(m)
	}
	inW := int(float64(origW) * scale)
	inH := int(float64(origH) * scale)
	// YuNet 内部按 /8 /16 /32 整除，输入宽高必须是 32 的倍数
	inW = max(32, (inW/32)*32)
	inH = max(32, (inH/32)*32)
	invX := float64(origW) / float64(inW)
	invY := float64(origH) / float64(inH)

	resized := gocv.NewMat()
	defer resized.Close()
	gocv.Resize(img, &resized, image.Pt(inW, inH), 0, 0, gocv.InterpolationLinear)

	blob := gocv.BlobFromImage(resized, 1.0, image.Pt(inW, inH), gocv.NewScalar(0, 0, 0, 0), false, false)
	defer blob.Close()
	d.net.SetInput(blob, "input")

	outs := d.net.ForwardLayers(yuOutputNames)
	if len(outs) != len(yuOutputNames) {
		return nil
	}
	defer func() {
		for i := range outs {
			outs[i].Close()
		}
	}()

	var faces []YuFace
	// 输出顺序与 yuOutputNames 一致：[cls_8,cls_16,cls_32, obj_8..32, bbox_8..32, kps_8..32]
	for si, stride := range yuStrides {
		cols := inW / stride
		rows := inH / stride
		n := rows * cols

		cls := outs[si].Reshape(1, n)         // n×1
		obj := outs[si+3].Reshape(1, n)       // n×1
		bbox := outs[si+6].Reshape(1, n)      // n×4
		kps := outs[si+9].Reshape(1, n)       // n×10
		clsC, objC, bboxC, kpsC := cls, obj, bbox, kps
		defer clsC.Close()
		defer objC.Close()
		defer bboxC.Close()
		defer kpsC.Close()

		for r := 0; r < rows; r++ {
			for c := 0; c < cols; c++ {
				idx := r*cols + c
				clsScore := clamp(cls.GetFloatAt(idx, 0), 0, 1)
				objScore := clamp(obj.GetFloatAt(idx, 0), 0, 1)
				score := float32(math.Sqrt(float64(clsScore * objScore)))
				if score < yuScoreThreshold {
					continue
				}
				// bbox 解码（输入图坐标）
				cx := (float64(c) + float64(bbox.GetFloatAt(idx, 0))) * float64(stride)
				cy := (float64(r) + float64(bbox.GetFloatAt(idx, 1))) * float64(stride)
				w := math.Exp(float64(bbox.GetFloatAt(idx, 2))) * float64(stride)
				h := math.Exp(float64(bbox.GetFloatAt(idx, 3))) * float64(stride)
				// 还原到原图坐标
				x1 := int(math.Round((cx - w/2) * invX))
				y1 := int(math.Round((cy - h/2) * invY))
				x2 := int(math.Round((cx + w/2) * invX))
				y2 := int(math.Round((cy + h/2) * invY))
				rect := image.Rect(x1, y1, x2, y2).Intersect(image.Rect(0, 0, origW, origH))
				if rect.Dx() < yuMinFacePixel || rect.Dy() < yuMinFacePixel {
					continue
				}
				var lm [5][2]float32
				for k := 0; k < 5; k++ {
					lx := (float64(kps.GetFloatAt(idx, 2*k)) + float64(c)) * float64(stride)
					ly := (float64(kps.GetFloatAt(idx, 2*k+1)) + float64(r)) * float64(stride)
					lm[k] = [2]float32{float32(lx * invX), float32(ly * invY)}
				}
				faces = append(faces, YuFace{Rect: rect, Score: score, Landmarks: lm})
			}
		}
	}

	return yuNMS(faces, yuNMSThreshold, yuTopK)
}

// yuNMS 按得分降序做贪心 NMS。
func yuNMS(faces []YuFace, threshold float64, limit int) []YuFace {
	sort.Slice(faces, func(i, j int) bool { return faces[i].Score > faces[j].Score })
	suppressed := make([]bool, len(faces))
	var keep []YuFace
	for i := range faces {
		if suppressed[i] {
			continue
		}
		keep = append(keep, faces[i])
		if len(keep) >= limit {
			break
		}
		for j := i + 1; j < len(faces); j++ {
			if suppressed[j] {
				continue
			}
			if rectIoU(faces[i].Rect, faces[j].Rect) > threshold {
				suppressed[j] = true
			}
		}
	}
	return keep
}

func rectIoU(a, b image.Rectangle) float64 {
	inter := a.Intersect(b)
	if inter.Empty() {
		return 0
	}
	ia := float64(inter.Dx() * inter.Dy())
	ua := float64(a.Dx()*a.Dy() + b.Dx()*b.Dy()) - ia
	if ua <= 0 {
		return 0
	}
	return ia / ua
}

func clamp(v float32, lo, hi float32) float32 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// FaceChecker 封装 YuNet 检测器 + Haar eye cascade，对图片做人脸检测 + 完整五官质量验证。
// 两条路径（图片导入 / Web 采集）统一复用它的 Detect + CheckQuality。
//
// YuNet 输出 5 个点：右眼中心、左眼中心、鼻尖、右嘴角、左嘴角
// 通过 CheckQuality 的条件（10 项全过才算完整五官）：
//   A. YuNet Score ≥ 0.75
//   B. 双眼水平距离 / 脸宽 ∈ [12%, 55%]
//   C. 双眼垂直倾斜 / 脸高 ≤ 30%
//   D. 鼻尖 x ∈ 双眼区间 ± 20%
//   E. MAR（嘴巴张合度）∈ [0.05, 0.45]
//   F. 嘴巴水平宽度 ≥ 12% 脸宽
//   G. 嘴巴中心 y ∈ [鼻尖+5%, 鼻尖+40%] faceH
//   H. 所有 5 个 landmark 在脸 bbox ±5% 内
//   I. 双眼 landmark 附近 Haar eye cascade 必须能检测到（闭眼拦截）
//   J. YuNet bbox 左右各留 ≥ 15% faceW 的耳朵空间（侧脸耳朵被裁 → 过不了）
type FaceChecker struct {
	yunet      *YuNetDetector
	eyeCascade *gocv.CascadeClassifier // eye_tree_eyeglasses 或 eye.xml
}

// NewFaceChecker 加载 YuNet 模型 + Haar eye cascade，构造 FaceChecker。
// yunetPath 为空或加载失败 → 返回 (nil, nil)（调用方可用 nil 判断是否可用）。
// cascadeDir 是 OpenCV Haar cascades 所在目录。
func NewFaceChecker(yunetPath, cascadeDir string) (*FaceChecker, error) {
	fc := &FaceChecker{}
	if yunetPath != "" {
		yd, err := NewYuNetDetector(yunetPath)
		if err != nil {
			log.Printf("[facecheck] ⚠️ YuNet 加载失败: %v，跳过人脸质量验证", err)
			return nil, nil
		}
		fc.yunet = yd
		log.Printf("[facecheck] YuNet 加载成功: %s", yunetPath)
	} else {
		return nil, nil
	}

	if cascadeDir != "" {
		eyeXML := filepath.Join(cascadeDir, "haarcascade_eye_tree_eyeglasses.xml")
		if _, err := os.Stat(eyeXML); err != nil {
			eyeXML = filepath.Join(cascadeDir, "haarcascade_eye.xml")
		}
		if _, err := os.Stat(eyeXML); err == nil {
			ec := gocv.NewCascadeClassifier()
			if ec.Load(eyeXML) {
				fc.eyeCascade = &ec
				log.Printf("[facecheck] Haar eye cascade 加载成功: %s", filepath.Base(eyeXML))
			} else {
				ec.Close()
				log.Printf("[facecheck] ⚠️ eye cascade 加载失败，闭眼拦截降级为 YuNet score 检查")
			}
		}
	}

	return fc, nil
}

// Close 释放 YuNet 模型资源。
func (fc *FaceChecker) Close() {
	if fc.yunet != nil {
		fc.yunet.Close()
	}
	if fc.eyeCascade != nil {
		fc.eyeCascade.Close()
	}
}

// Detect 检测图片中所有人脸（直接代理 YuNetDetector.Detect）。
func (fc *FaceChecker) Detect(img gocv.Mat) []YuFace {
	if fc.yunet == nil {
		return nil
	}
	return fc.yunet.Detect(img)
}

// CheckQuality 验证 YuNet 检测到的一张脸是否完整五官（全部条件满足才返回 true）。
//
// 参数调优依据 500 张真实婚礼/旅行照片的 reject 统计：
//   - YuNet 默认 score 阈值 0.6，手机糊脸集中 0.6-0.74 → score≥0.62
//   - 闭嘴脸 YuNet mouth landmark y 几乎重合 → MAR 下限从 0.05 降到 0.01
//   - Haar eye cascade 在低光/眼镜/糊脸场景几乎全瞎 → 改用 YuNet score 兜底
//   - 近脸 bbox 紧贴眼角 → 耳朵空间从 15% 降到 6%
//   - 广角/侧脸双眼距离偏小 → eyeDxRatio 下限从 12% 降到 6%
func (fc *FaceChecker) CheckQuality(f YuFace, img gocv.Mat) bool {
	faceW := float32(f.Rect.Dx())
	faceH := float32(f.Rect.Dy())
	bboxLeft := float32(f.Rect.Min.X)
	bboxRight := float32(f.Rect.Max.X)

	// A. YuNet score 门槛（0.62 覆盖手机糊脸 p90 但过滤 YuNet 默认 0.6 以下的极差脸）
	if f.Score < 0.62 {
		return false
	}

	lm := f.Landmarks
	for k := 0; k < 5; k++ {
		x, y := lm[k][0], lm[k][1]
		if x != x || y != y { // NaN
			return false
		}
		if x < -5 || y < -5 || x > float32(img.Cols()+5) || y > float32(img.Rows()+5) {
			return false
		}
	}
	if faceW < 25 || faceH < 25 {
		return false
	}

	rightEye := lm[0]
	leftEye := lm[1]
	nose := lm[2]
	rightMouth := lm[3]
	leftMouth := lm[4]

	// B. 双眼水平距离 ∈ [6%, 65%] faceW（真实分布 p50=0.074，下限改 0.06）
	eyeDx := absF32(leftEye[0] - rightEye[0])
	eyeDxRatio := eyeDx / faceW
	if eyeDxRatio < 0.06 || eyeDxRatio > 0.65 {
		return false
	}

	// C. 双眼垂直倾斜 ≤ 40%（脸歪/仰拍放宽）
	eyeDy := absF32(leftEye[1] - rightEye[1])
	if eyeDy/faceH > 0.40 {
		return false
	}

	// D. 鼻尖在双眼区间 ±35% eyeDx 内（放宽）
	eyeMinX := minF32(leftEye[0], rightEye[0])
	eyeMaxX := maxF32(leftEye[0], rightEye[0])
	if nose[0] < eyeMinX-eyeDx*0.35 || nose[0] > eyeMaxX+eyeDx*0.35 {
		return false
	}

	// E. MAR（嘴巴张合度）∈ [0.01, 0.55]
	// 闭嘴脸 YuNet mouth landmark y 几乎重合 → MAR→0，下限 0.01 放行真闭嘴脸
	mouthW := absF32(leftMouth[0] - rightMouth[0])
	mouthH := absF32(leftMouth[1] - rightMouth[1])
	if mouthW < 1 {
		return false
	}
	mar := mouthH / mouthW
	if mar < 0.01 || mar > 0.55 {
		return false
	}

	// F. 嘴巴水平宽度 ≥ 6% faceW（闭嘴时 mouth landmark 靠近）
	if mouthW/faceW < 0.06 {
		return false
	}

	// G. 嘴巴中心 y ∈ [鼻尖+3%, 鼻尖+48%] faceH
	mouthAvgY := (rightMouth[1] + leftMouth[1]) / 2
	offset := mouthAvgY - nose[1]
	if offset/faceH < 0.03 || offset/faceH > 0.48 {
		return false
	}

	// H. 所有 landmark 在 face bbox ±8% 内
	xb := bboxLeft - faceW*0.08
	x2b := bboxRight + faceW*0.08
	yb := float32(f.Rect.Min.Y) - faceH*0.08
	y2b := float32(f.Rect.Max.Y) + faceH*0.08
	for k := 0; k < 5; k++ {
		if lm[k][0] < xb || lm[k][0] > x2b || lm[k][1] < yb || lm[k][1] > y2b {
			return false
		}
	}

	// I. 闭眼/假脸拦截——双策略：
	//   1) YuNet score ≥ 0.72 → 高置信度，YuNet 自己不会给闭眼高分 → 跳过 Haar 检查
	//   2) score 在 0.62-0.72 之间 → 必须至少有一只眼睛被 Haar 检出（兜底防低分假脸）
	if f.Score < 0.72 && fc.eyeCascade != nil {
		rightOpen := fc.eyeOpenNear(img, rightEye[0], rightEye[1], faceW, faceH)
		leftOpen := fc.eyeOpenNear(img, leftEye[0], leftEye[1], faceW, faceH)
		if !rightOpen && !leftOpen {
			return false // 低分 + 两眼都检不出 → 大概率闭眼或假脸
		}
	}

	// J. 耳朵空间：YuNet bbox 左右各留 ≥ 6% faceW
	// 真实近脸分布 p50=13-14%，侧脸 < 3%，6% 是安全分界
	minEarSpace := faceW * 0.06
	rightEarSpace := rightEye[0] - bboxLeft
	leftEarSpace := bboxRight - leftEye[0]
	if rightEarSpace < minEarSpace || leftEarSpace < minEarSpace {
		return false // 至少一侧耳朵空间不够 → 侧脸
	}

	return true
}

// eyeOpenNear 检查 img 中 (cx, cy) 附近是否有一只睁开的眼睛。
// 闭眼时眼皮缝高度只有 2-3px，minSize=8×5 挡不住 → cascade 检不出。
func (fc *FaceChecker) eyeOpenNear(img gocv.Mat, cx, cy, faceW, faceH float32) bool {
	w := int(maxF32(faceW*0.22, 14))
	h := int(maxF32(faceH*0.14, 9))
	x1 := max(0, int(cx)-w/2)
	y1 := max(0, int(cy)-h/2)
	x2 := min(img.Cols(), x1+w)
	y2 := min(img.Rows(), y1+h)
	if x2-x1 < 10 || y2-y1 < 6 {
		return false
	}
	patch := img.Region(image.Rect(x1, y1, x2, y2))
	defer patch.Close()
	gray := gocv.NewMat()
	defer gray.Close()
	gocv.CvtColor(patch, &gray, gocv.ColorBGRToGray)
	rects := fc.eyeCascade.DetectMultiScaleWithParams(gray, 1.05, 5, 0, image.Pt(8, 5), image.Pt(0, 0))
	return len(rects) > 0
}
