package importface

import (
	"fmt"
	"image"
	"math"
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
