package importface

import (
	"crypto/rand"
	"fmt"
	"image"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gocv.io/x/gocv"

	"vi/internal/config"
	"vi/internal/qdrant"
	"vi/internal/store"
)

// Importer 把本地文件夹里的图片扫描一遍，检测人脸 → 裁剪 → 提取 embedding → upsert 到 Qdrant。
type Importer struct {
	cfg    config.Config
	qd     *qdrant.Qdrant
	st     *store.Store
	cascade *gocv.CascadeClassifier
	net     *gocv.Net
	label   string // 可选标签，作为每张新脸的 PersonID 前缀
	dryRun  bool
}

// NewImporter 创建 Importer。
// cascadeXML 是 OpenCV Haar cascade 路径（检测人脸用），arcOnnx 是 arc.onnx 路径（提取 embedding 用）。
func NewImporter(cfg config.Config, qd *qdrant.Qdrant, st *store.Store, cascadeXML, arcOnnx, label string, dryRun bool) (*Importer, error) {
	cc := gocv.NewCascadeClassifier()
	if !cc.Load(cascadeXML) {
		return nil, fmt.Errorf("加载 Haar cascade 失败: %s", cascadeXML)
	}
	net := gocv.ReadNetFromONNX(arcOnnx)
	if net.Empty() {
		return nil, fmt.Errorf("加载 arc.onnx 失败: %s", arcOnnx)
	}
	log.Printf("[import] OpenCV DNN 已加载 arc.onnx")
	return &Importer{cfg: cfg, qd: qd, st: st, cascade: &cc, net: &net, label: label, dryRun: dryRun}, nil
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
	log.Printf("[import] 完成: %d 张图片 → %d 张人脸成功 / %d 张图片处理失败", len(imagePaths), success, failed)
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

	// 缩小图用于检测（加速 + 降低误检）
	detectScale := 1.0
	detectImg := img.Clone()
	const maxDetectDim = 1280
	if max(img.Rows(), img.Cols()) > maxDetectDim {
		detectScale = float64(maxDetectDim) / float64(max(img.Rows(), img.Cols()))
		newW := int(float64(img.Cols()) * detectScale)
		newH := int(float64(img.Rows()) * detectScale)
		gocv.Resize(img, &detectImg, image.Pt(newW, newH), 0, 0, gocv.InterpolationLinear)
	}

	// 人脸检测（Haar cascade）
	rects := im.cascade.DetectMultiScaleWithParams(detectImg, 1.1, 4, 0, image.Pt(30, 30), image.Pt(0, 0))
	im.writeFacesLog(path, len(rects))

	if len(rects) == 0 {
		return 0, nil
	}

	saved := 0
	for i, r := range rects {
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
		embedding, err := im.extractEmbedding(crop)
		if err != nil {
			log.Printf("[import] extractEmbedding 失败: %v", err)
			crop.Close()
			continue
		}

		// 保存抠图
		rel, err := im.saveFaceCrop(path, i, x, y, w, h, cropRect, crop)
		crop.Close()
		if err != nil {
			log.Printf("[import] 保存抠图失败: %v", err)
		}

		if im.dryRun {
			saved++
			continue
		}

		personID := newPersonID(im.label, path, i)
		if im.st != nil {
			// 通过 Store 统一写入：Event（前端可见） + Qdrant（向量搜索）
			if _, err := im.st.ImportFace(personID, embedding, rel, path, time.Now().In(im.cfg.Location)); err != nil {
				log.Printf("[import] ImportFace 失败: %v", err)
				continue
			}
		} else {
			// 无 Store（dry-run 或独立模式）：只写 Qdrant
			pointID := uuid4()
			payload := map[string]any{
				"kind":        "face",
				"person_id":   personID,
				"source_path": path,
				"source_file": filepath.Base(path),
				"imported_at": time.Now().In(im.cfg.Location).Format(time.RFC3339),
				"crop":        rel,
			}
			if err := im.qd.UpsertFace(pointID, embedding, payload); err != nil {
				log.Printf("[import] upsert Qdrant 失败: %v", err)
				continue
			}
		}
		saved++
	}

	detectImg.Close()
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
func (im *Importer) saveFaceCrop(srcPath string, idx, x, y, w, h int, rect image.Rectangle, crop gocv.Mat) (string, error) {
	ext := strings.ToLower(filepath.Ext(srcPath))
	switch ext {
	case ".jpeg":
		ext = ".jpg"
	case ".webp":
		ext = ".jpg" // OpenCV 不直接写 webp
	case ".heic":
		ext = ".jpg"
	case "":
		ext = ".jpg"
	}
	rel := filepath.Join("import", fmt.Sprintf("%s_f%d_%.3f_%.3f%s",
		filepath.Base(srcPath), idx, float64(x)/100, float64(y)/100, ext))
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

func newPersonID(label, sourcePath string, idx int) string {
	base := filepath.Base(filepath.Dir(sourcePath))
	if base == "." {
		base = filepath.Base(sourcePath)
	}
	base = sanitize(base)
	if label != "" {
		base = sanitize(label)
	}
	return fmt.Sprintf("import_%s_%s_%d", base, shortHash(sourcePath), idx)
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

// uuid4 生成一个 UUID v4 字符串（随机 122 bit + 固定 version/variant）。
func uuid4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 理论上不会失败，万一失败用 zero-padded fallback
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xx
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
