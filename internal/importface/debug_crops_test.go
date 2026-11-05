//go:build importface

package importface

import (
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gocv.io/x/gocv"
)

// TestDebugCrops 在原始分辨率下重放检测管线，输出每个候选人脸的过滤细节。
// 目标 hash 通过环境变量 DEBUG_HASHES 指定（逗号分隔，如 "ad45,b746"），
// 源目录通过 DEBUG_DIR 指定。
// 运行: DEBUG_HASHES=ad45,b746 DEBUG_DIR=/path/to/dir go test -tags importface -run TestDebugCrops -v ./internal/importface/
func TestDebugCrops(t *testing.T) {
	dir := os.Getenv("DEBUG_DIR")
	hashesEnv := os.Getenv("DEBUG_HASHES")
	if dir == "" || hashesEnv == "" {
		t.Skip("需要 DEBUG_DIR 和 DEBUG_HASHES")
	}
	targets := map[string]bool{}
	for _, h := range strings.Split(hashesEnv, ",") {
		targets[strings.TrimSpace(h)] = true
	}

	cascadeDir := os.Getenv("CASCADE_DIR")
	if cascadeDir == "" {
		for _, d := range []string{"/usr/local/share/opencv4/haarcascades", "/opt/homebrew/share/opencv4/haarcascades", "/usr/share/opencv4/haarcascades"} {
			if _, err := os.Stat(filepath.Join(d, "haarcascade_eye.xml")); err == nil {
				cascadeDir = d
				break
			}
		}
	}
	load := func(name string) *gocv.CascadeClassifier {
		c := gocv.NewCascadeClassifier()
		if !c.Load(filepath.Join(cascadeDir, name)) {
			t.Fatalf("加载失败: %s", name)
		}
		return &c
	}
	im := &Importer{
		cascade:      load("haarcascade_frontalface_default.xml"),
		cascadeAlt:   load("haarcascade_frontalface_alt.xml"),
		cascadeAlt2:  load("haarcascade_frontalface_alt2.xml"),
		eyeCascade:   load("haarcascade_eye.xml"),
		mouthCascade: load("haarcascade_smile.xml"),
	}

	// 找目标 hash 对应的源图片
	var srcPaths []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if targets[shortHash(p)] {
			srcPaths = append(srcPaths, p)
		}
		return nil
	})
	if len(srcPaths) == 0 {
		t.Fatalf("没有找到 hash 匹配的源图片: %v", targets)
	}

	for _, path := range srcPaths {
		fmt.Printf("\n##### %s (hash=%s) #####\n", filepath.Base(path), shortHash(path))
		img := gocv.IMRead(path, gocv.IMReadColor)
		if img.Empty() {
			fmt.Println("无法读取")
			continue
		}

		detectScale := 1.0
		detectImg := img.Clone()
		const maxDetectDim = 1280
		if max(img.Rows(), img.Cols()) > maxDetectDim {
			detectScale = float64(maxDetectDim) / float64(max(img.Rows(), img.Cols()))
			gocv.Resize(img, &detectImg, image.Pt(int(float64(img.Cols())*detectScale), int(float64(img.Rows())*detectScale)), 0, 0, gocv.InterpolationLinear)
		}
		rects := im.cascade.DetectMultiScaleWithParams(detectImg, 1.2, 6, 0, image.Pt(50, 50), image.Pt(0, 0))
		fmt.Printf("检测到 %d 个候选\n", len(rects))

		for i, r := range rects {
			x := int(float64(r.Min.X) / detectScale)
			y := int(float64(r.Min.Y) / detectScale)
			x2 := int(float64(r.Max.X) / detectScale)
			y2 := int(float64(r.Max.Y) / detectScale)
			x, y = max(0, x), max(0, y)
			x2, y2 = min(img.Cols(), x2), min(img.Rows(), y2)
			w, h := x2-x, y2-y
			if w < 40 || h < 40 {
				continue
			}
			expandW, expandH := int(float64(w)*0.2), int(float64(h)*0.2)
			x, y = max(0, x-expandW), max(0, y-expandH)
			x2, y2 = min(img.Cols(), x2+expandW), min(img.Rows(), y2+expandH)
			w, h = x2-x, y2-y
			if w < 60 || h < 60 {
				continue
			}
			crop := img.Region(image.Rect(x, y, x2, y2))
			W, H := float64(crop.Cols()), float64(crop.Rows())
			fmt.Printf("\n  [idx=%d] crop %dx%d\n", i, int(W), int(H))

			// 投票
			votes := 1
			if r2 := im.cascadeAlt.DetectMultiScaleWithParams(crop, 1.1, 4, 0, image.Pt(20, 20), image.Pt(0, 0)); len(r2) > 0 {
				votes++
			}
			if r2 := im.cascadeAlt2.DetectMultiScaleWithParams(crop, 1.1, 4, 0, image.Pt(20, 20), image.Pt(0, 0)); len(r2) > 0 {
				votes++
			}
			fmt.Printf("    votes: %d %s\n", votes, map[bool]string{true: "PASS", false: "FAIL"}[votes >= 2])

			// 肤色
			skinOK := hasSkinTone(crop)
			hsv := gocv.NewMat()
			gocv.CvtColor(crop, &hsv, gocv.ColorBGRToHSV)
			mask := gocv.NewMat()
			gocv.InRangeWithScalar(hsv, gocv.NewScalar(0, 58, 90, 0), gocv.NewScalar(50, 173, 255, 0), &mask)
			ratio := float64(gocv.CountNonZero(mask)) / float64(mask.Rows()*mask.Cols())
			fmt.Printf("    skin: %.3f %s\n", ratio, map[bool]string{true: "PASS", false: "FAIL"}[skinOK])
			hsv.Close()
			mask.Close()

			// 眼睛/嘴巴细节
			gray := gocv.NewMat()
			gocv.CvtColor(crop, &gray, gocv.ColorBGRToGray)
			upperH := int(H * 0.65)
			upper := gray.Region(image.Rect(0, 0, int(W), upperH))
			for _, mn := range []int{3, 4, 5} {
				er := im.eyeCascade.DetectMultiScaleWithParams(upper, 1.1, mn, 0, image.Pt(10, 10), image.Pt(0, 0))
				fmt.Printf("    eyes mn=%d: %d", mn, len(er))
				for _, r := range er {
					fmt.Printf(" [%.2f,%.2f w%.2f]", (float64(r.Min.X)+float64(r.Dx())/2)/W, (float64(r.Min.Y)+float64(r.Dy())/2)/H, float64(r.Dx())/W)
				}
				fmt.Println()
			}
			lowerY := int(H * 0.45)
			lower := gray.Region(image.Rect(0, lowerY, int(W), int(H)))
			for _, mn := range []int{3, 5, 6, 8} {
				mr := im.mouthCascade.DetectMultiScaleWithParams(lower, 1.1, mn, 0, image.Pt(12, 12), image.Pt(0, 0))
				fmt.Printf("    mouths mn=%d: %d", mn, len(mr))
				for _, r := range mr {
					fmt.Printf(" [%.2f,%.2f w%.2f]", (float64(r.Min.X)+float64(r.Dx())/2)/W, (float64(lowerY+r.Min.Y)+float64(r.Dy())/2)/H, float64(r.Dx())/W)
				}
				fmt.Println()
			}
			fmt.Printf("    hasFacialFeatures: %v\n", im.hasFacialFeatures(crop))

			gray.Close()
			upper.Close()
			lower.Close()
			crop.Close()
		}
		detectImg.Close()
		img.Close()
	}
}
