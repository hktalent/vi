package importface

import (
	"fmt"
	"image"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gocv.io/x/gocv"

	"vi/internal/exif"
	"vi/internal/store"
)

// processVideo 导入一个视频里的人脸与车牌。
// 用 ffmpeg 按采样间隔抽帧到临时目录，再让每帧走图片管线（人脸复用 importFacesFromMat）。
// 返回 (新人脸数, 新车牌数)。
func (im *Importer) processVideo(path string) (int, int, error) {
	ffmpeg, err := findFFmpeg()
	if err != nil {
		return 0, 0, err
	}

	tmpDir, err := os.MkdirTemp("", "vi-video-*")
	if err != nil {
		return 0, 0, fmt.Errorf("创建临时目录失败: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// fps=1/step → 每 step 秒取 1 帧；-frames:v 限制总量
	vf := fmt.Sprintf("fps=1/%.4g", im.video.StepSec)
	cmd := exec.Command(ffmpeg,
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-i", path,
		"-vf", vf,
		"-frames:v", fmt.Sprintf("%d", im.video.MaxFrames),
		"-q:v", "2",
		filepath.Join(tmpDir, "f_%05d.jpg"),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return 0, 0, fmt.Errorf("ffmpeg 抽帧失败: %s", msg)
	}

	frames, _ := filepath.Glob(filepath.Join(tmpDir, "f_*.jpg"))
	sort.Strings(frames)
	if len(frames) == 0 {
		return 0, 0, nil
	}

	// 时间基准：视频 EXIF 创建时间 → 文件修改时间
	base := exif.Parse(path).CreatedAt
	if base.IsZero() {
		if st, err := os.Stat(path); err == nil {
			base = st.ModTime()
		}
	}
	base = base.In(im.cfg.Location)

	faceDedupe := map[string]float64{} // personID → 当前最佳帧分数
	plateSeen := map[string]bool{}     // 本视频内已导入的车牌（去重）
	faces, plates := 0, 0

	for i, fp := range frames {
		img := gocv.IMRead(fp, gocv.IMReadColor)
		if img.Empty() {
			continue
		}
		ts := base.Add(time.Duration(float64(i) * im.video.StepSec * float64(time.Second)))

		if im.video.Faces {
			saved, _, _ := im.importFacesFromMat(img, faceSrcMeta{
				srcPath:    path,
				capturedAt: ts,
				dedupe:     faceDedupe,
			})
			faces += saved
		}

		if im.plate != nil {
			for _, hit := range im.plate.DetectAndRecognize(img) {
				text := store.NormalizePlate(hit.Text)
				if text == "" || plateSeen[text] {
					continue
				}
				plateSeen[text] = true

				rel, err := im.savePlateCrop(text, img, hit.Rect)
				if err != nil {
					log.Printf("[import] 保存车牌图失败 %s: %v", text, err)
					continue
				}

				if im.dryRun {
					plates++
					continue
				}
				if im.st != nil {
					res, err := im.st.Capture(store.CaptureInput{
						Kind:       "plate",
						CapturedAt: ts,
						PlateText:  text,
						PlateConf:  hit.Conf,
						ImagePath:  rel,
						PersonID:   store.ResolvePlatePersonID(text),
					})
					if err != nil {
						log.Printf("[import] 车牌事件写入失败 %s: %v", text, err)
						continue
					}
					if res.Skipped {
						continue
					}
				}
				plates++
			}
		}
		img.Close()
	}
	return faces, plates, nil
}

// savePlateCrop 把车牌区域裁剪后写入 data/images/import/plate-{text}.jpg（同车牌确定性覆盖）。
func (im *Importer) savePlateCrop(text string, img gocv.Mat, box image.Rectangle) (string, error) {
	vw, vh := img.Cols(), img.Rows()
	pad := max(2, box.Dy()/8)
	r := image.Rect(box.Min.X-pad, box.Min.Y-pad, box.Max.X+pad, box.Max.Y+pad)
	r = r.Intersect(image.Rect(0, 0, vw, vh))
	if r.Dx() < 8 || r.Dy() < 4 {
		return "", fmt.Errorf("车牌区域过小")
	}
	// 限制最大宽度，避免超高分辨率视频写入巨大文件
	crop := img.Region(r)
	defer crop.Close()
	if r.Dx() > 480 {
		resized := gocv.NewMat()
		defer resized.Close()
		nh := max(16, int(float64(r.Dy())*(480.0/float64(r.Dx()))))
		gocv.Resize(crop, &resized, image.Pt(480, nh), 0, 0, gocv.InterpolationArea)
		return im.writePlateFile(text, resized)
	}
	return im.writePlateFile(text, crop)
}

func (im *Importer) writePlateFile(text string, mat gocv.Mat) (string, error) {
	rel := filepath.ToSlash(filepath.Join("import", "plate-"+text+".jpg"))
	full := filepath.Join(im.cfg.DataDir, "images", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", err
	}
	if !gocv.IMWriteWithParams(full, mat, []int{gocv.IMWriteJpegQuality, 92}) {
		return "", fmt.Errorf("imwrite 失败")
	}
	return rel, nil
}

// findFFmpeg 依次尝试 PATH、Homebrew 常见位置。
func findFFmpeg() (string, error) {
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p, nil
	}
	for _, p := range []string{
		"/opt/homebrew/bin/ffmpeg",
		"/usr/local/bin/ffmpeg",
		"/usr/bin/ffmpeg",
	} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("未找到 ffmpeg，请先安装：brew install ffmpeg")
}
