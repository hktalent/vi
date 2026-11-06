package importface

import (
	"crypto/md5"
	"crypto/rand"
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
	force       bool              // 强制重新导入（忽略已有记录）
	faceThresh  float64           // 人脸相似度阈值（Qdrant 搜索用，保持 0.45 召回率优先）
	batchThresh float64           // 批次内同人匹配阈值（更高，避免不同人被错误合并）
	existing    map[string]bool   // 已导入的 person_id 集合（幂等去重）
	seen        []seenFace        // 本次 Run 内已见过的人脸向量+personID（批次内同人去重）
	skipped     int               // 跳过数量统计
	filtered    int               // 被 norm / 侧脸 / 五官不全过滤丢弃的数量统计
}

// seenFace 批次内已见过的一张脸，用于 Run 循环中的同人去重。
type seenFace struct {
	personID string
	vec      []float32
}

// cosine 算两个 512 维向量的余弦相似度。
func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// matchBatch 在本次导入的 seen 列表里找最相似的 personID，相似度 ≥ batchThresh 才返回。
// 批次内用更高阈值（0.58），避免不同人在光线/角度相近时 cosine 刚好 ≥ 0.45 被错误合并。
// ArcFace 余弦相似度典型分布：同人 0.65~0.92，不同人 0.25~0.48。
func (im *Importer) matchBatch(vec []float32) (string, float64) {
	var bestID string
	var bestScore float64
	for _, s := range im.seen {
		score := cosine(vec, s.vec)
		if score > bestScore {
			bestScore = score
			bestID = s.personID
		}
	}
	if bestScore >= im.batchThresh {
		return bestID, bestScore
	}
	return "", bestScore
}

// NewImporter 创建 Importer。
// yunetPath 是 YuNet 检测模型路径（face_detection_yunet_2023mar.onnx），存在时优先于 Haar cascade。
// cascadeXML 是 OpenCV Haar cascade 路径（YuNet 缺失时的回退检测器），arcOnnx 是 arc.onnx 路径（提取 embedding 用）。
// force=true 时忽略已有导入记录，全部重新导入。
func NewImporter(cfg config.Config, qd *qdrant.Qdrant, st *store.Store, cascadeXML, arcOnnx, yunetPath, label string, dryRun, force bool) (*Importer, error) {
	im := &Importer{
		cfg:         cfg,
		qd:          qd,
		st:          st,
		label:       label,
		dryRun:      dryRun,
		force:       force,
		faceThresh:  cfg.FaceThresh,
		batchThresh: 0.58, // 批次内更严格阈值（ArcFace 同人 ≥ 0.65，不同人 ≤ 0.48，0.58 是安全分界）
		seen:        []seenFace{},
	}

	// 检测器：优先 YuNet（DNN，准确率高且输出 5 点关键点）；加载失败回退 Haar cascade
	if yunetPath != "" {
		if yd, err := NewYuNetDetector(yunetPath); err == nil {
			im.yunet = yd
			log.Printf("[import] 使用 YuNet 人脸检测器: %s", yunetPath)
		} else {
			log.Printf("[import] ⚠️  %v，回退 Haar cascade", err)
		}
	}

	cascadeDir := filepath.Dir(cascadeXML)

	if im.yunet == nil {
		cc := gocv.NewCascadeClassifier()
		if !cc.Load(cascadeXML) {
			return nil, fmt.Errorf("加载 Haar cascade 失败: %s", cascadeXML)
		}
		// 加载第二、第三投票 cascade
		ccAlt := gocv.NewCascadeClassifier()
		if !ccAlt.Load(filepath.Join(cascadeDir, "haarcascade_frontalface_alt.xml")) {
			log.Printf("[import] ⚠️  alt cascade 加载失败，投票降级为单 cascade")
		}
		ccAlt2 := gocv.NewCascadeClassifier()
		if !ccAlt2.Load(filepath.Join(cascadeDir, "haarcascade_frontalface_alt2.xml")) {
			log.Printf("[import] ⚠️  alt2 cascade 加载失败，投票降级为单 cascade")
		}
		// mouth cascade 仅 Haar 路径需要
		mouthXML := filepath.Join(cascadeDir, "haarcascade_smile.xml")
		mc := gocv.NewCascadeClassifier()
		if !mc.Load(mouthXML) {
			return nil, fmt.Errorf("加载 mouth cascade 失败: %s", mouthXML)
		}
		im.cascade, im.cascadeAlt, im.cascadeAlt2, im.mouthCascade = &cc, &ccAlt, &ccAlt2, &mc
	}

	// ✅ eye cascade 两种模式都要加载：
	//   - YuNet 模式：用来验证 YuNet 给的眼睛 landmark 附近真的有睁开的眼睛（闭眼时眼皮缝 cascade 检不出）
	//   - Haar 模式：hasFacialFeatures 里用
	eyeXML := filepath.Join(cascadeDir, "haarcascade_eye_tree_eyeglasses.xml")
	if _, err := os.Stat(eyeXML); err != nil {
		eyeXML = filepath.Join(cascadeDir, "haarcascade_eye.xml")
	}
	ec := gocv.NewCascadeClassifier()
	if !ec.Load(eyeXML) {
		return nil, fmt.Errorf("加载 eye cascade 失败: %s", eyeXML)
	}
	im.eyeCascade = &ec

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
		faces, _, err := im.processImage(p)
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

// processImage 处理一张图片：检测所有人脸 → embedding 去重 → 保存。
func (im *Importer) processImage(path string) (int, int, error) {
	img := gocv.IMRead(path, gocv.IMReadColor)
	if img.Empty() {
		return 0, 0, fmt.Errorf("无法读取")
	}
	defer img.Close()

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

	if len(rects) == 0 {
		return 0, 0, nil
	}

	saved := 0
	filteredThisImg := 0
	for i, r := range rects {
		// YuNet 模式下用 score + 5 点关键点 + Haar eye cascade 三重验证五官完整性 + 睁眼
		if im.yunet != nil && i < len(yuFaces) {
			if !im.isQualityFace(yuFaces[i], img) {
				im.filtered++
				filteredThisImg++
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

		// Haar 级联启发式过滤（YuNet 是 DNN 检测器，跳过这三步检查）
		if im.yunet == nil {
			votes := 1
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
			if !hasSkinTone(crop) {
				im.filtered++
				crop.Close()
				continue
			}
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

		// ✅ personID 决策链（embedding 相似度优先，两处都用 batchThresh 严格阈值）：
		// 1. 批次内已见过（同目录前面图片已识别）→ 复用已有 personID（batchThresh 0.58）
		// 2. Qdrant 里已有（之前批次导入过）→ 复用已有 personID（同样用 batchThresh，避免跨批次误合并）
		// 3. 都没见过 → 新生成 import_{label}_{uuid8}
		var personID string
		if pid, score := im.matchBatch(embedding); pid != "" {
			personID = pid
			log.Printf("[import] 批次内同人复用 personID=%s (sim=%.3f)", personID, score)
		} else if im.qd != nil {
			// 也用 batchThresh（0.58），比 cfg.FaceThresh（0.52）严格，避免跨批次不同人被误合并
			if hit, err := im.qd.SearchFace(embedding, im.batchThresh); err == nil && hit.ID != "" {
				if pid, ok := hit.Payload["person_id"].(string); ok && pid != "" {
					personID = pid
					log.Printf("[import] Qdrant 命中已有人 personID=%s (sim=%.3f)", personID, hit.Score)
				}
			}
		}
		if personID == "" {
			personID = newPersonID(im.label)
		}

		// 记录到批次 seen 列表，后续同人匹配用（existing 检查之前就要加，否则同一张图里的多张同人脸会重复生成 personID）
		im.seen = append(im.seen, seenFace{personID: personID, vec: embedding})

		// ImportFace 的 existing 检查：personID 已存在（-force 模式）→ 跳过
		// 🔴 必须在 saveFaceCrop 之前检查，避免磁盘 I/O + 覆盖已存图片
		if !im.force && im.existing != nil && im.existing[personID] {
			crop.Close()
			im.skipped++
			continue
		}

		if im.dryRun {
			crop.Close()
			saved++
			continue
		}

		// ✅ 保存抠图（挪到 existing 检查之后）
		rel, err := im.saveFaceCrop(personID, crop)
		crop.Close()
		if err != nil {
			log.Printf("[import] 保存抠图失败: %v", err)
			continue // 抠图存不了就别进 event log 了
		}

		if im.st != nil {
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

	// ✅ 真正可信的日志：原始检测 / 通过五官 / 被过滤
	if len(rects) > 0 {
		p := filepath.Base(path)
		switch {
		case saved > 0 && filteredThisImg > 0:
			log.Printf("[import] %s → 检测 %d 张 · 通过五官 %d · 过滤 %d（闭眼/侧脸/五官不全）", p, len(rects), saved, filteredThisImg)
		case saved > 0:
			log.Printf("[import] %s → %d 张人脸（全部通过五官检查）", p, saved)
		default:
			log.Printf("[import] %s → 检测 %d 张 · 全部被五官检查过滤（闭眼/侧脸/五官不全）", p, len(rects))
		}
	}
	return saved, filteredThisImg, nil
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
// 文件名只看 personID → 同人多次落盘直接覆盖，磁盘上永远是最后一次采集的最新版本。
func (im *Importer) saveFaceCrop(personID string, crop gocv.Mat) (string, error) {
	rel := filepath.Join("import", personID+".jpg")
	full := filepath.Join(im.cfg.DataDir, "images", rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", err
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

func newPersonID(label string) string {
	base := "import"
	if label != "" {
		base = sanitize(label)
	}
	return fmt.Sprintf("import_%s_%s", base, uuidShort())
}

// uuidShort 生成短 UUID（前 8 位 hex）。
func uuidShort() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000"
	}
	return fmt.Sprintf("%x", b[:])
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

// hasFacialFeatures 用 Haar 级联矩形 + 几何约束 + 统计特征判断是否真脸。
// 强化版本（相对旧版的 3 个关键改动）：
//   1. MUST 有嘴巴：条件 1（成对眼睛）和条件 2（单眼）都要求 mouth cascade 命中。
//      旧版条件 1 只要求眼睛成对就 return true → 花的两个对称花瓣没嘴巴也能蒙混 → 现在必须有嘴巴。
//   2. 眼睛矩形宽高比 ≥ 1.5：真人眼睛是水平椭圆，花芯/纹理触发的矩形近正方形 w/h≈1.0 → reject。
//   3. 花放射状拒：上半脸 eye cascade 返回 ≥ 5 个小框 → 花瓣对称放射触发的误检 → 直接 reject。
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

	// 花放射状拒：eye cascade 返回 ≥ 5 个小矩形（花瓣对称放射状结构）→ 直接 reject
	if len(eyeRects) >= 5 {
		return false
	}

	type feat struct{ x, y, w, h float64 }
	eyes := make([]feat, 0, len(eyeRects))
	for _, r := range eyeRects {
		cx := (float64(r.Min.X) + float64(r.Dx())/2) / faceW
		cy := (float64(r.Min.Y) + float64(r.Dy())/2) / faceH
		w := float64(r.Dx()) / faceW
		h := float64(r.Dy()) / faceH
		// 尺寸 + 位置 + 宽高比（真人眼睛水平椭圆 w/h ≥ 1.5，花芯近正方形 ≈ 1.0）
		if w < 0.07 || w > 0.35 {
			continue
		}
		if h <= 0 || w/h < 1.5 {
			continue
		}
		if cy < 0.10 || cy > 0.60 || cx < 0.10 || cx > 0.90 {
			continue
		}
		eyes = append(eyes, feat{cx, cy, w, h})
	}

	// 2. 嘴巴检测（下 55% 区域，minNeighbors=8 更严格抑制布料/花纹理）
	lowerY := int(faceH * 0.45)
	lowerH := int(faceH) - lowerY
	if lowerH < 10 {
		lowerY = 0
		lowerH = int(faceH)
	}
	lower := gray.Region(image.Rect(0, lowerY, int(faceW), lowerY+lowerH))
	defer lower.Close()
	mouthRects := im.mouthCascade.DetectMultiScaleWithParams(lower, 1.1, 8, 0, image.Pt(12, 12), image.Pt(0, 0))

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
		mouths = append(mouths, feat{cx, cy, w, float64(r.Dy()) / faceH})
	}

	// ✅ 条件 1：水平成对的眼睛 + MUST 有嘴巴（旧版没要求嘴巴 → 花蒙混）
	if len(eyes) >= 2 && len(mouths) >= 1 {
		for i := 0; i < len(eyes); i++ {
			for j := i + 1; j < len(eyes); j++ {
				dy := math.Abs(eyes[i].y - eyes[j].y)
				dx := math.Abs(eyes[i].x - eyes[j].x)
				centerX := (eyes[i].x + eyes[j].x) / 2
				centerY := (eyes[i].y + eyes[j].y) / 2
				sizeRatio := eyes[i].w / eyes[j].w
				if sizeRatio < 1 {
					sizeRatio = 1 / sizeRatio
				}
				if dy < 0.15 && dx >= 0.15 && dx <= 0.50 && sizeRatio < 2.0 && centerX >= 0.28 && centerX <= 0.72 {
					// 成对眼睛 + 至少一个嘴巴在两眼中点正下方 ± 0.35 水平范围
					for _, m := range mouths {
						if m.y-centerY > 0.15 && math.Abs(m.x-centerX) < 0.35 {
							return true
						}
					}
				}
			}
		}
	}

	// ✅ 条件 2：1 只居中的眼睛 + MUST 有嘴巴（侧脸/单眼场景）
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

// isQualityFace 用 YuNet score + 5 点关键点 + Haar eye cascade 四重验证人脸质量。
// 5 个点顺序：右眼中心、左眼中心、鼻尖、右嘴角、左嘴角。
//
// 通过条件（全部满足才返回 true）：
//   A. YuNet Score ≥ 0.75（低质量脸 YuNet score 会降）
//   B. 双眼水平距离 / 脸宽 ∈ [12%, 55%]（侧脸双眼叠一起 → 过小；过宽 → YuNet 检测框太窄，耳朵被裁）
//   C. 双眼垂直倾斜 / 脸高 ≤ 30%（脸歪 → 过不了）
//   D. 鼻尖 x ∈ 双眼区间 ± 20%（侧脸鼻尖跑眼睛外 → 过不了）
//   E. MAR（嘴巴张合度）∈ [0.05, 0.45]（太小=闭嘴/花假 landmark，太大=假脸）
//   F. 嘴巴水平宽度 ≥ 12% 脸宽（花的假嘴角挤在一起 → 过不了）
//   G. 嘴巴中心 y ∈ [鼻尖+5%, 鼻尖+40%] faceH（严格在鼻子正下方）
//   H. 所有 5 个 landmark 在脸 bbox ±5% 内
//   I. 【闭眼拦截】双眼 landmark 附近 Haar eye cascade 必须能检测到
//   J. 【耳朵空间】YuNet bbox 宽度 ≥ 双眼距离 × 1.8（正脸双眼间距占脸宽 ~40%，YuNet bbox 覆盖到耳朵）
//      + 右眼角到 bbox 左边界 ≥ 15% faceW + 左眼角到 bbox 右边界 ≥ 15% faceW
//      侧脸时 YuNet bbox 紧贴眼角 → 耳朵被裁 → 过不了
func (im *Importer) isQualityFace(f YuFace, img gocv.Mat) bool {
	faceW := float32(f.Rect.Dx())
	faceH := float32(f.Rect.Dy())
	bboxLeft := float32(f.Rect.Min.X)
	bboxRight := float32(f.Rect.Max.X)

	// A. YuNet score 门槛
	if f.Score < 0.75 {
		return false
	}

	lm := f.Landmarks
	for k := 0; k < 5; k++ {
		x, y := lm[k][0], lm[k][1]
		if x != x || y != y {
			return false
		}
		if x < -5 || y < -5 || x > float32(img.Cols()+5) || y > float32(img.Rows()+5) {
			return false
		}
	}

	if faceW < 30 || faceH < 30 {
		return false
	}
	rightEye := lm[0]
	leftEye := lm[1]
	nose := lm[2]
	rightMouth := lm[3]
	leftMouth := lm[4]

	// B. 双眼水平距离（上下限双重检查）
	eyeDx := absF32(leftEye[0] - rightEye[0])
	eyeDxRatio := eyeDx / faceW
	if eyeDxRatio < 0.12 || eyeDxRatio > 0.55 {
		// 太小=侧脸；太大=YuNet bbox 太窄，耳朵被裁掉了
		return false
	}

	// C. 双眼垂直倾斜
	eyeDy := absF32(leftEye[1] - rightEye[1])
	if eyeDy/faceH > 0.30 {
		return false
	}

	// D. 鼻尖在双眼之间
	eyeMinX := minF32(leftEye[0], rightEye[0])
	eyeMaxX := maxF32(leftEye[0], rightEye[0])
	if nose[0] < eyeMinX-eyeDx*0.2 || nose[0] > eyeMaxX+eyeDx*0.2 {
		return false
	}

	// E. MAR（嘴巴张合度）
	mouthW := absF32(leftMouth[0] - rightMouth[0])
	mouthH := absF32(leftMouth[1] - rightMouth[1])
	if mouthW < 1 {
		return false
	}
	mar := mouthH / mouthW
	if mar < 0.05 || mar > 0.45 {
		return false
	}

	// F. 嘴巴水平宽度 ≥ 12% 脸宽
	if mouthW/faceW < 0.12 {
		return false
	}

	// G. 嘴巴中心严格在鼻尖下方
	mouthAvgY := (rightMouth[1] + leftMouth[1]) / 2
	offset := mouthAvgY - nose[1]
	if offset/faceH < 0.05 || offset/faceH > 0.40 {
		return false
	}

	// H. 所有 landmark 在 face bbox 内
	xb := bboxLeft - faceW*0.05
	x2b := bboxRight + faceW*0.05
	yb := float32(f.Rect.Min.Y) - faceH*0.05
	y2b := float32(f.Rect.Max.Y) + faceH*0.05
	for k := 0; k < 5; k++ {
		if lm[k][0] < xb || lm[k][0] > x2b || lm[k][1] < yb || lm[k][1] > y2b {
			return false
		}
	}

	// I. 闭眼拦截：双眼 landmark 附近 Haar eye cascade 必须能检测到
	if im.eyeCascade != nil {
		if !im.eyeOpenNear(img, rightEye[0], rightEye[1], faceW, faceH) ||
			!im.eyeOpenNear(img, leftEye[0], leftEye[1], faceW, faceH) {
			return false
		}
	}

	// J. ✅ 【耳朵空间】YuNet bbox 必须在左右眼角外各留 ≥ 15% faceW 的空间给耳朵
	// 正脸时 YuNet bbox 覆盖左右耳朵 → bbox 宽 ≥ eyeDx × 1.8
	// 侧脸 ≥ 30° 时远侧耳朵被裁 → bbox 紧贴眼角 → 过不了
	minEarSpace := faceW * 0.15
	// 右边：bbox 左边界 到 右眼角 landmark 的距离
	rightEarSpace := rightEye[0] - bboxLeft
	// 左边：bbox 右边界 到 左眼角 landmark 的距离
	leftEarSpace := bboxRight - leftEye[0]
	if rightEarSpace < minEarSpace || leftEarSpace < minEarSpace {
		return false // 至少一侧耳朵空间不够 → 侧脸 / 耳朵被裁
	}

	return true
}

// eyeOpenNear 检查 img 中 (cx, cy) 附近是否有一只睁开的眼睛。
// 以该点为中心，画 eyeW × eyeH 的小矩形（扩边到整像素），在里面跑 Haar eye cascade。
// 返回 true = 检测到至少一个眼睛矩形。闭眼时眼皮缝高度太小（< 5px）过不了 minSize。
func (im *Importer) eyeOpenNear(img gocv.Mat, cx, cy, faceW, faceH float32) bool {
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
	rects := im.eyeCascade.DetectMultiScaleWithParams(gray, 1.05, 5, 0, image.Pt(8, 5), image.Pt(0, 0))
	return len(rects) > 0
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
