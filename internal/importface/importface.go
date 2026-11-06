package importface

import (
	"crypto/md5"
	"fmt"
	"image"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gocv.io/x/gocv"

	"vi/internal/config"
	"vi/internal/exif"
	"vi/internal/qdrant"
	"vi/internal/store"
)

// Importer 把本地文件夹里的图片扫描一遍，检测人脸 → 裁剪 → 提取 embedding → upsert 到 Qdrant。
type Importer struct {
	cfg        config.Config
	qd         *qdrant.Qdrant
	st         *store.Store
	yunet      *YuNetDetector          // 优先使用的 DNN 检测器（nil 时回退 Haar）
	cascade    *gocv.CascadeClassifier
	cascadeAlt *gocv.CascadeClassifier // 第二投票 cascade
	cascadeAlt2 *gocv.CascadeClassifier // 第三投票 cascade
	eyeCascade  *gocv.CascadeClassifier // 眼睛（含戴眼镜）
	mouthCascade *gocv.CascadeClassifier // 嘴巴/微笑
	net         *gocv.Net
	label      string // 可选标签，作为每张新脸的 PersonID 前缀
	dryRun     bool
	force      bool              // 强制重新导入（忽略已有记录）
	existing   map[string]bool   // 已导入的 person_id 集合（幂等去重）
	skipped    int               // 跳过数量统计
	filtered   int               // 被 norm 过滤丢弃的数量统计
}

// NewImporter 创建 Importer。
// yunetPath 是 YuNet 检测模型路径（face_detection_yunet_2023mar.onnx），存在时优先于 Haar cascade。
// cascadeXML 是 OpenCV Haar cascade 路径（YuNet 缺失时的回退检测器），arcOnnx 是 arc.onnx 路径（提取 embedding 用）。
// force=true 时忽略已有导入记录，全部重新导入。
func NewImporter(cfg config.Config, qd *qdrant.Qdrant, st *store.Store, cascadeXML, arcOnnx, yunetPath, label string, dryRun, force bool) (*Importer, error) {
	im := &Importer{cfg: cfg, qd: qd, st: st, label: label, dryRun: dryRun, force: force}

	// 检测器：优先 YuNet（DNN，准确率高且输出 5 点关键点）；加载失败回退 Haar cascade
	if yunetPath != "" {
		if yd, err := NewYuNetDetector(yunetPath); err == nil {
			im.yunet = yd
			log.Printf("[import] 使用 YuNet 人脸检测器: %s", yunetPath)
		} else {
			log.Printf("[import] ⚠️  %v，回退 Haar cascade", err)
		}
	}

	if im.yunet == nil {
		cc := gocv.NewCascadeClassifier()
		if !cc.Load(cascadeXML) {
			return nil, fmt.Errorf("加载 Haar cascade 失败: %s", cascadeXML)
		}
		// 加载第二、第三投票 cascade（同目录）
		cascadeDir := filepath.Dir(cascadeXML)
		ccAlt := gocv.NewCascadeClassifier()
		if !ccAlt.Load(filepath.Join(cascadeDir, "haarcascade_frontalface_alt.xml")) {
			log.Printf("[import] ⚠️  alt cascade 加载失败，投票降级为单 cascade")
		}
		ccAlt2 := gocv.NewCascadeClassifier()
		if !ccAlt2.Load(filepath.Join(cascadeDir, "haarcascade_frontalface_alt2.xml")) {
			log.Printf("[import] ⚠️  alt2 cascade 加载失败，投票降级为单 cascade")
		}
		// 从 face cascade 路径推导 eye/mouth cascade 路径（同目录）
		eyeXML := filepath.Join(cascadeDir, "haarcascade_eye.xml")
		ec := gocv.NewCascadeClassifier()
		if !ec.Load(eyeXML) {
			return nil, fmt.Errorf("加载 eye cascade 失败: %s", eyeXML)
		}
		mouthXML := filepath.Join(cascadeDir, "haarcascade_smile.xml")
		mc := gocv.NewCascadeClassifier()
		if !mc.Load(mouthXML) {
			return nil, fmt.Errorf("加载 mouth cascade 失败: %s", mouthXML)
		}
		im.cascade, im.cascadeAlt, im.cascadeAlt2, im.eyeCascade, im.mouthCascade = &cc, &ccAlt, &ccAlt2, &ec, &mc
	}

	net := gocv.ReadNetFromONNX(arcOnnx)
	if net.Empty() {
		return nil, fmt.Errorf("加载 arc.onnx 失败: %s", arcOnnx)
	}
	log.Printf("[import] OpenCV DNN 已加载 arc.onnx")
	im.net = &net

	// 加载已有 person_id 集合（幂等去重）
	if st != nil && !force {
		im.existing = st.ExistingPersonIDs("import_")
		if len(im.existing) > 0 {
			log.Printf("[import] 发现 %d 条已导入记录，将跳过（-force 可强制重导）", len(im.existing))
		}
	}
	return im, nil
}

// Run 递归扫描 dir，导入所有图片。返回导入的人脸数量。
func (im *Importer) Run(dir string) (int, error) {
	if _, err := os.Stat(dir); err != nil {
		return 0, err
	}

	var imagePaths []string
	exts := map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".bmp": true, ".webp": true, ".heic": true}
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		if exts[ext] {
			imagePaths = append(imagePaths, p)
		}
		return nil
	})

	log.Printf("[import] 发现 %d 张图片，开始处理...", len(imagePaths))

	success, failed := 0, 0
	var firstErr error
	for i, p := range imagePaths {
		fmt.Fprintf(os.Stderr, "\r[%d/%d] %s", i+1, len(imagePaths), filepath.Base(p))
		faces, err := im.processImage(p)
		if err != nil {
			failed++
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", filepath.Base(p), err)
			}
			continue
		}
		success += faces
	}
	fmt.Fprintln(os.Stderr)
	log.Printf("[import] 完成: %d 张图片 → %d 张新人脸导入 / %d 张已存在跳过 / %d 张误检过滤 / %d 张图片处理失败",
		len(imagePaths), success, im.skipped, im.filtered, failed)
	if firstErr != nil {
		return success, firstErr
	}
	return success, nil
}

// processImage 处理一张图片：检测所有人脸，逐一提取 embedding + upsert。
func (im *Importer) processImage(path string) (int, error) {
	img := gocv.IMRead(path, gocv.IMReadColor)
	if img.Empty() {
		return 0, fmt.Errorf("无法读取")
	}
	defer img.Close()

	// 算文件内容 MD5 前缀（前 8 位 hex）——跨目录同内容图片（即使文件路径/文件名完全不同）也能命中去重
	contentHash := fileMD5Short(path)

	// 解析 EXIF：GPS 经纬度/海拔/相机方向/创建时间
	exifInfo := exif.Parse(path)

	// 人脸检测：YuNet（DNN，输出 5 点关键点）优先；否则 Haar cascade
	detectScale := 1.0
	var rects []image.Rectangle
	var yuFaces []YuFace // YuNet 模式下同时拿到含 landmarks 的完整结构
	if im.yunet != nil {
		yuFaces = im.yunet.Detect(img)
		for _, f := range yuFaces {
			rects = append(rects, f.Rect)
		}
	} else {
		// 缩小图用于检测（加速 + 降低误检）
		detectImg := img.Clone()
		const maxDetectDim = 1280
		if max(img.Rows(), img.Cols()) > maxDetectDim {
			detectScale = float64(maxDetectDim) / float64(max(img.Rows(), img.Cols()))
			newW := int(float64(img.Cols()) * detectScale)
			newH := int(float64(img.Rows()) * detectScale)
			gocv.Resize(img, &detectImg, image.Pt(newW, newH), 0, 0, gocv.InterpolationLinear)
		}
		// scaleFactor=1.2（更严格）, minNeighbors=6（减少误检）, minSize=50（过滤小区域）
		rects = im.cascade.DetectMultiScaleWithParams(detectImg, 1.2, 6, 0, image.Pt(50, 50), image.Pt(0, 0))
		detectImg.Close()
	}
	im.writeFacesLog(path, len(rects))

	if len(rects) == 0 {
		return 0, nil
	}

	saved := 0
	for i, r := range rects {
		// 幂等检查：personID 以文件内容 MD5 为核心，同一张图同一张脸不管在哪个目录都生成相同 personID
		personID := newPersonID(im.label, contentHash, i)
		if !im.force && im.existing != nil && im.existing[personID] {
			im.skipped++
			continue
		}

		// YuNet 模式下用 5 点关键点做侧脸 / 五官不全过滤
		if im.yunet != nil && i < len(yuFaces) {
			// YuNet 的 bbox 本身就是原图坐标，detectScale=1.0，直接用
			if !isFrontalFace(yuFaces[i], img.Cols(), img.Rows()) {
				im.filtered++
				continue
			}
		}

		// 坐标还原到原始图
		x := int(float64(r.Min.X) / detectScale)
		y := int(float64(r.Min.Y) / detectScale)
		w := int(float64(r.Dx()) / detectScale)
		h := int(float64(r.Dy()) / detectScale)
		x2 := int(float64(r.Max.X) / detectScale)
		y2 := int(float64(r.Max.Y) / detectScale)
		x, y = max(0, x), max(0, y)
		x2, y2 = min(img.Cols(), x2), min(img.Rows(), y2)
		w, h = x2-x, y2-y
		if w < 40 || h < 40 {
			continue
		}
		faceRect := image.Rect(x, y, x+w, y+h)
		face := img.Region(faceRect)
		if face.Empty() {
			continue
		}

		// 扩展脸部区域 20%（包含下巴/额头）
		face.Close()
		expandW, expandH := int(float64(w)*0.2), int(float64(h)*0.2)
		x, y = max(0, x-expandW), max(0, y-expandH)
		x2, y2 = min(img.Cols(), x2+expandW), min(img.Rows(), y2+expandH)
		w, h = x2-x, y2-y
		if w < 60 || h < 60 {
			continue
		}

		cropRect := image.Rect(x, y, x2, y2)
		crop := img.Region(cropRect)

		// 以下三处启发式过滤只为弥补 Haar 的高误检率；
		// YuNet 是 DNN 检测器且输出关键点，误检率极低，跳过这些检查（保留 embedding norm 过滤）。
		if im.yunet == nil {
			// 多 cascade 投票：alt/alt2 cascade 也检测一下，至少 2/3 通过才保留
			votes := 1 // 主 cascade 已投 1 票
			if im.cascadeAlt != nil {
				altRects := im.cascadeAlt.DetectMultiScaleWithParams(crop, 1.1, 4, 0, image.Pt(20, 20), image.Pt(0, 0))
				if len(altRects) > 0 {
					votes++
				}
			}
			if im.cascadeAlt2 != nil {
				alt2Rects := im.cascadeAlt2.DetectMultiScaleWithParams(crop, 1.1, 4, 0, image.Pt(20, 20), image.Pt(0, 0))
				if len(alt2Rects) > 0 {
					votes++
				}
			}
			if votes < 2 {
				im.filtered++
				crop.Close()
				continue
			}

			// 肤色检测：人脸包含一定比例的肤色像素，非人脸（纯白/纯黑/纹理）没有
			if !hasSkinTone(crop) {
				im.filtered++
				crop.Close()
				continue
			}

			// 面部特征几何验证：水平成对的眼睛，或眼睛+嘴巴的正确位置关系。
			// 领带/花朵/布料的纹理误检无法通过几何约束。
			if !im.hasFacialFeatures(crop) {
				im.filtered++
				crop.Close()
				continue
			}
		}

		embedding, err := im.extractEmbedding(crop)
		if err != nil {
			log.Printf("[import] extractEmbedding 失败: %v", err)
			crop.Close()
			continue
		}

		// embedding L2 范数过滤：非人脸区域（花纹/布料/手写）的范数通常异常
		norm := l2Norm(embedding)
		if norm > 8.0 || norm < 1.5 {
			im.filtered++
			crop.Close()
			continue
		}

		// 保存抠图
		rel, err := im.saveFaceCrop(path, personID, crop)
		crop.Close()
		if err != nil {
			log.Printf("[import] 保存抠图失败: %v", err)
		}

		if im.dryRun {
			saved++
			continue
		}

		if im.st != nil {
			// 通过 Store 统一写入：Event（前端可见） + Qdrant（向量搜索）
			capturedAt := exifInfo.CreatedAt
			if capturedAt.IsZero() {
				capturedAt = time.Now()
			}
			capturedAt = capturedAt.In(im.cfg.Location)
			if _, err := im.st.ImportFace(store.ImportFaceInput{
				PersonID:   personID,
				Vector:     embedding,
				ImagePath:  rel,
				SourcePath: path,
				CapturedAt: capturedAt,
				Lat:        exifInfo.Latitude,
				Lon:        exifInfo.Longitude,
				Altitude:   exifInfo.Altitude,
				Heading:    exifInfo.Heading,
			}); err != nil {
				log.Printf("[import] ImportFace 失败: %v", err)
				continue
			}
		} else {
			// 无 Store（dry-run 或独立模式）：只写 Qdrant
			// 用 personID 做 point ID：重复运行覆盖同 person 向量（幂等）
			payload := map[string]any{
				"kind":        "face",
				"person_id":   personID,
				"source_path": path,
				"source_file": filepath.Base(path),
				"imported_at": time.Now().In(im.cfg.Location).Format(time.RFC3339),
				"crop":        rel,
			}
			if err := im.qd.UpsertFace(personID, embedding, payload); err != nil {
				log.Printf("[import] upsert Qdrant 失败: %v", err)
				continue
			}
		}
		saved++
	}

	return saved, nil
}

// extractEmbedding 用 arc.onnx 提取 512 维特征向量。
//
// arc.onnx 是 TensorFlow 导出的，声明输入格式是 NHWC [batch, H, W, C]。
// gocv.BlobFromImage 总是产出 NCHW，所以这里手动构造 NHWC 4D blob 传给 SetInput。
func (im *Importer) extractEmbedding(crop gocv.Mat) ([]float32, error) {
	// BGR → RGB（和浏览器端 arc.onnx 输入一致）
	rgb := gocv.NewMat()
	gocv.CvtColor(crop, &rgb, gocv.ColorBGRToRGB)
	defer rgb.Close()

	// resize 到 112×112
	resized := gocv.NewMat()
	gocv.Resize(rgb, &resized, image.Pt(112, 112), 0, 0, gocv.InterpolationLinear)
	defer resized.Close()

	// normalize: (pixel - 127.5) / 127.5 → [-1, 1]
	norm := gocv.NewMat()
	resized.ConvertToWithParams(&norm, gocv.MatTypeCV32F, 1.0/127.5, -127.5/127.5)
	defer norm.Close()

	// 手动构造 NHWC 格式 4D blob：[1, 112, 112, 3]
	// 内存布局和 norm（H×W×3 HWC）完全一致，直接 memcpy 即可
	blob, err := makeNhwcBlob(norm)
	if err != nil {
		return nil, fmt.Errorf("构造 NHWC blob 失败: %w", err)
	}
	defer blob.Close()

	im.net.SetInput(blob, "input_1")
	out := im.net.Forward("embedding")
	defer out.Close()

	// out shape 通常是 [1, 512]
	if out.Empty() {
		return nil, fmt.Errorf("net.Forward 返回空")
	}
	return matToFloat32(out)
}

// makeNhwcBlob 从 HWC Mat 构造 NHWC 4D blob。
// src 是 H×W×C 的 CV_32FC3 Mat，返回 dims=[1, H, W, C] 的 CV_32FC1 Mat，数据与 src 共享内存布局。
func makeNhwcBlob(src gocv.Mat) (gocv.Mat, error) {
	h, w, c := src.Rows(), src.Cols(), src.Channels()
	if src.Type()&gocv.MatTypeCV32F != gocv.MatTypeCV32F {
		return gocv.NewMat(), fmt.Errorf("src 必须是 CV_32F，实际 type=%d", src.Type())
	}
	if !src.IsContinuous() {
		return gocv.NewMat(), fmt.Errorf("src 必须是连续 Mat")
	}
	dst := gocv.NewMatWithSizes([]int{1, h, w, c}, gocv.MatTypeCV32F)
	srcData, err := src.DataPtrFloat32()
	if err != nil {
		dst.Close()
		return gocv.NewMat(), err
	}
	dstData, err := dst.DataPtrFloat32()
	if err != nil {
		dst.Close()
		return gocv.NewMat(), err
	}
	copy(dstData, srcData)
	return dst, nil
}

func matToFloat32(m gocv.Mat) ([]float32, error) {
	rows := m.Rows()
	cols := m.Cols()
	if rows == 0 {
		return nil, fmt.Errorf("mat 为空")
	}
	// arc.onnx 输出通常是 [1, 512] 或 [1, 1, 512] → 展平
	total := rows * cols
	if total > 100000 {
		return nil, fmt.Errorf("embedding 维度异常: %d×%d", rows, cols)
	}
	vec := make([]float32, 0, total)
	for r := 0; r < rows; r++ {
		for c := 0; c < cols; c++ {
			vec = append(vec, float32(m.GetFloatAt(r, c)))
		}
	}
	return vec, nil
}

// saveFaceCrop 把人脸抠图落盘到 data/images/import/ 目录。
// 文件名基于 personID（确定性），同一张图同一张脸多次运行生成相同文件名，不会重复保存。
// 路径：images/import/personID.jpg（扁平结构，import 的 personID 已含文件内容 MD5）。
func (im *Importer) saveFaceCrop(srcPath string, personID string, crop gocv.Mat) (string, error) {
	ext := strings.ToLower(filepath.Ext(srcPath))
	switch ext {
	case ".jpeg", ".webp", ".heic", "":
		ext = ".jpg"
	}
	rel := filepath.Join("import", personID+ext)
	full := filepath.Join(im.cfg.DataDir, "images", rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", err
	}
	// 文件已存在 → 跳过（幂等）
	if _, err := os.Stat(full); err == nil {
		return rel, nil
	}
	target := gocv.NewMat()
	gocv.Resize(crop, &target, image.Pt(160, 160), 0, 0, gocv.InterpolationLinear)
	defer target.Close()
	if gocv.IMWrite(full, target) {
		return rel, nil
	}
	return "", fmt.Errorf("gocv.IMWrite 失败")
}

func (im *Importer) writeFacesLog(path string, n int) {
	if n == 0 {
		return
	}
	if n > 8 {
		n = 8 // Haarcascade 对群体照可能多检几次
	}
	log.Printf("[import] %s → %d 张人脸", filepath.Base(path), n)
}

func newPersonID(label, contentHash string, idx int) string {
	base := "import"
	if label != "" {
		base = sanitize(label)
	}
	return fmt.Sprintf("import_%s_%s_%d", base, contentHash, idx)
}

// fileMD5Short 读整个文件算 MD5，返回前 8 位 hex。
// 读错误时退化为文件路径的字符串 hash（保证 personID 至少可预测）。
func fileMD5Short(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Sprintf("err%x", shortHash(path))
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Sprintf("err%x", shortHash(path))
	}
	return fmt.Sprintf("%x", h.Sum(nil)[:4]) // 4 字节 = 8 hex 位，足够同文件唯一，同时 personID 不会过长
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) == 0 {
		out = "person"
	}
	return out
}

func shortHash(s string) string {
	h := uint32(2166136261)
	for _, b := range []byte(s) {
		h ^= uint32(b)
		h *= 16777619
	}
	return fmt.Sprintf("%x", h&0xffff)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// l2Norm 计算 float32 切片的 L2 范数。
func l2Norm(v []float32) float64 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	return math.Sqrt(sum)
}

// hasSkinTone 检查图像中是否含有足够比例的肤色像素。
// 人脸在 HSV 色彩空间中 H∈[0,50], S∈[0.23,0.68], V∈[0.35,1.0] 范围内有稳定的肤色分布。
// 非人脸区域（纯白婚纱、纯黑背景、手写线条、花纹布料）的肤色比例极低。
// 返回 true 表示肤色比例 >= 15%，可能是人脸。
func hasSkinTone(crop gocv.Mat) bool {
	hsv := gocv.NewMat()
	defer hsv.Close()
	gocv.CvtColor(crop, &hsv, gocv.ColorBGRToHSV)

	// 肤色范围（HSV）：H 0-50, S 58-173, V 90-255
	mask := gocv.NewMat()
	defer mask.Close()
	gocv.InRangeWithScalar(hsv, gocv.NewScalar(0, 58, 90, 0), gocv.NewScalar(50, 173, 255, 0), &mask)

	totalPixels := mask.Rows() * mask.Cols()
	if totalPixels == 0 {
		return false
	}
	skinPixels := gocv.CountNonZero(mask)
	ratio := float64(skinPixels) / float64(totalPixels)
	return ratio >= 0.15
}

// hasFacialFeatures 用几何约束验证面部特征，过滤领带/花朵/布料等误检。
//
// 通过条件（任一）：
//  1. 眼睛成对：两只眼睛水平对齐、间距合理（0.15-0.50 脸宽）、大小相近、整体居中。
//     领带纹理的"眼睛"是竖直排列或尺寸过小（<0.07 脸宽）的噪声；
//     玫瑰花辨的"眼睛"分布在画面边缘、间距过宽，均无法通过。
//  2. 眼睛 + 嘴巴位置关系正确：1 只居中的眼睛 + 嘴巴在其下方。
//     覆盖侧脸（只能看到 1 只眼睛）场景。
//
// 关键约束是尺寸归一化：真人眼睛宽约 0.07-0.35 脸宽，嘴巴宽约 0.15-0.60 脸宽；
// 高分辨率图片上花纹/布料触发的误检尺寸远小于此。
func (im *Importer) hasFacialFeatures(crop gocv.Mat) bool {
	faceW := float64(crop.Cols())
	faceH := float64(crop.Rows())

	gray := gocv.NewMat()
	defer gray.Close()
	gocv.CvtColor(crop, &gray, gocv.ColorBGRToGray)

	// 1. 眼睛检测（上 65% 区域）
	upperH := int(faceH * 0.65)
	if upperH < 10 {
		upperH = int(faceH)
	}
	upper := gray.Region(image.Rect(0, 0, int(faceW), upperH))
	defer upper.Close()
	eyeRects := im.eyeCascade.DetectMultiScaleWithParams(upper, 1.1, 4, 0, image.Pt(10, 10), image.Pt(0, 0))

	type feat struct{ x, y, w float64 }
	eyes := make([]feat, 0, len(eyeRects))
	for _, r := range eyeRects {
		cx := (float64(r.Min.X) + float64(r.Dx())/2) / faceW
		cy := (float64(r.Min.Y) + float64(r.Dy())/2) / faceH // region 从 y=0 起，无需偏移
		w := float64(r.Dx()) / faceW
		// 尺寸 + 位置约束：过滤花纹噪声
		if w < 0.07 || w > 0.35 {
			continue
		}
		if cy < 0.10 || cy > 0.60 || cx < 0.10 || cx > 0.90 {
			continue
		}
		eyes = append(eyes, feat{cx, cy, w})
	}

	// 条件 1：水平成对的眼睛（间距、对齐、大小、居中）
	for i := 0; i < len(eyes); i++ {
		for j := i + 1; j < len(eyes); j++ {
			dy := math.Abs(eyes[i].y - eyes[j].y)
			dx := math.Abs(eyes[i].x - eyes[j].x)
			centerX := (eyes[i].x + eyes[j].x) / 2
			sizeRatio := eyes[i].w / eyes[j].w
			if sizeRatio < 1 {
				sizeRatio = 1 / sizeRatio
			}
			if dy < 0.15 && dx >= 0.15 && dx <= 0.50 && sizeRatio < 2.0 && centerX >= 0.28 && centerX <= 0.72 {
				return true
			}
		}
	}

	// 2. 嘴巴检测（下 55% 区域，minNeighbors=6 抑制纹理误检）
	lowerY := int(faceH * 0.45)
	lowerH := int(faceH) - lowerY
	if lowerH < 10 {
		lowerY = 0
		lowerH = int(faceH)
	}
	lower := gray.Region(image.Rect(0, lowerY, int(faceW), lowerY+lowerH))
	defer lower.Close()
	mouthRects := im.mouthCascade.DetectMultiScaleWithParams(lower, 1.1, 6, 0, image.Pt(12, 12), image.Pt(0, 0))

	mouths := make([]feat, 0, len(mouthRects))
	for _, r := range mouthRects {
		cx := (float64(r.Min.X) + float64(r.Dx())/2) / faceW
		cy := (float64(lowerY+r.Min.Y) + float64(r.Dy())/2) / faceH // 还原到全图坐标
		w := float64(r.Dx()) / faceW
		if w < 0.15 || w > 0.60 {
			continue
		}
		if cy < 0.55 || cy > 0.95 || cx < 0.28 || cx > 0.72 {
			continue
		}
		mouths = append(mouths, feat{cx, cy, w})
	}

	// 条件 2：1 只居中的眼睛 + 嘴巴在其下方（侧脸/单眼场景）
	for _, e := range eyes {
		if e.x < 0.30 || e.x > 0.70 {
			continue
		}
		for _, m := range mouths {
			if m.y-e.y > 0.15 && math.Abs(m.x-e.x) < 0.35 {
				return true
			}
		}
	}

	return false
}

// isFrontalFace 用 YuNet 5 点关键点判断人脸是否足够正 / 五官是否完整。
// 5 个点顺序：右眼、左眼、鼻尖、右嘴角、左嘴角。
//
// 通过条件（全部满足才返回 true）：
//  1. 双眼水平距离占脸宽 ≥ 12%：侧脸时双眼几乎叠在一起，比例会 < 10%
//  2. 双眼垂直倾斜占脸高 ≤ 30%：正常脸双眼水平对齐，倾斜过多说明偏/晃
//  3. 鼻尖在双眼之间（x 在 eye_left.x ~ eye_right.x 范围内）：侧脸时鼻尖会跑到眼睛一侧外面
//  4. 嘴角 y > 鼻尖 y（嘴巴在鼻子下面）：反向说明脸可能歪到不可识别
//  5. 所有 landmark 都在脸 bbox 内（无越界）：五官不全（比如只看到半张脸）时会有 landmark 跑出 bbox
func isFrontalFace(f YuFace, imgW, imgH int) bool {
	lm := f.Landmarks
	// 先确认 5 个点坐标都合理（不是 NaN、不是 0、不在图片外 5px）
	for k := 0; k < 5; k++ {
		x, y := lm[k][0], lm[k][1]
		if x != x || y != y {
			return false // NaN
		}
		if x < -5 || y < -5 || x > float32(imgW+5) || y > float32(imgH+5) {
			return false // 完全越界
		}
	}

	faceW := float32(f.Rect.Dx())
	faceH := float32(f.Rect.Dy())
	if faceW < 20 || faceH < 20 {
		return false
	}
	rightEye := lm[0]
	leftEye := lm[1]
	nose := lm[2]
	rightMouth := lm[3]
	leftMouth := lm[4]

	// 1. 双眼水平距离
	eyeDx := absF32(leftEye[0] - rightEye[0])
	if eyeDx/faceW < 0.12 {
		return false // 双眼太近 → 侧脸严重
	}

	// 2. 双眼垂直倾斜
	eyeDy := absF32(leftEye[1] - rightEye[1])
	if eyeDy/faceH > 0.30 {
		return false // 倾斜过大
	}

	// 3. 鼻尖在双眼之间（允许少量偏移）
	eyeMinX := minF32(leftEye[0], rightEye[0])
	eyeMaxX := maxF32(leftEye[0], rightEye[0])
	if nose[0] < eyeMinX-eyeDx*0.2 || nose[0] > eyeMaxX+eyeDx*0.2 {
		return false // 鼻尖不在双眼中间 → 脸转太多
	}

	// 4. 嘴角 y 在鼻尖 y 下方（嘴巴在鼻子下，允许少量）
	mouthAvgY := (rightMouth[1] + leftMouth[1]) / 2
	if mouthAvgY < nose[1]-faceH*0.05 {
		return false
	}

	// 5. 所有 landmark 在 face bbox 内（含 5% 宽松）
	xb := float32(f.Rect.Min.X) - faceW*0.05
	x2b := float32(f.Rect.Max.X) + faceW*0.05
	yb := float32(f.Rect.Min.Y) - faceH*0.05
	y2b := float32(f.Rect.Max.Y) + faceH*0.05
	for k := 0; k < 5; k++ {
		if lm[k][0] < xb || lm[k][0] > x2b || lm[k][1] < yb || lm[k][1] > y2b {
			return false // landmark 跑出脸框 → 五官不全（侧脸被裁掉）
		}
	}

	return true
}

func absF32(x float32) float32 {
	if x < 0 {
		return -x
	}
	return x
}
func minF32(a, b float32) float32 {
	if a < b {
		return a
	}
	return b
}
func maxF32(a, b float32) float32 {
	if a > b {
		return a
	}
	return b
}
