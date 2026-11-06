// Package imgenh 提供人脸抠图的异步高清增强：
// 三次插值 ×3 放大 → unsharp 锐化 → Lanczos4 还原到原始尺寸 → 原子写回原文件。
// 超分→缩回的往返比直接一次缩放保留更多真实细节，消除压缩与低分辨率带来的模糊感。
// 纯 OpenCV imgproc 实现（不用 DNN 模型）：毫秒级、零崩溃风险、无外部依赖。
// 目的：刚采集时看到的是原图，毫秒级增强完成后，下次再看到同一张脸就是清晰版。
package imgenh

import (
	"image"
	"log"
	"os"
	"path/filepath"
	"sync"

	"gocv.io/x/gocv"
)

const upscaleFactor = 3

// Enhancer 异步图像增强器。1 个 worker 串行处理（单张毫秒级，足够跟上采集速率）。
type Enhancer struct {
	dataDir string

	mu        sync.Mutex
	cond      *sync.Cond // 队列有新任务
	emptyCond *sync.Cond // 队列排空（Wait 用）
	queue     []string
	seen      map[string]int64 // rel → 处理完成后的文件大小（同内容不重复处理）
	busy      bool
	done      bool
}

// New 创建增强器并启动后台 worker。
func New(dataDir string) *Enhancer {
	e := &Enhancer{
		dataDir: dataDir,
		seen:    map[string]int64{},
	}
	e.cond = sync.NewCond(&e.mu)
	e.emptyCond = sync.NewCond(&e.mu)
	go e.worker()
	return e
}

// Enqueue 把相对 dataDir/images 的路径（如 "import/xxx.jpg"、"2026/11/06/face-xxx.jpg"）
// 加入异步增强队列。非阻塞；同内容（路径+文件大小相同）自动去重。
func (e *Enhancer) Enqueue(rel string) {
	if e == nil || rel == "" {
		return
	}
	full := filepath.Join(e.dataDir, "images", filepath.FromSlash(rel))
	st, err := os.Stat(full)
	if err != nil || st.Size() < 512 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if sz, ok := e.seen[rel]; ok && sz == st.Size() {
		return // 同一内容已处理过
	}
	e.queue = append(e.queue, rel)
	e.cond.Signal()
}

// Wait 阻塞直到队列排空（importfaces 等一次性进程退出前调用，防止任务丢失）。
func (e *Enhancer) Wait() {
	e.mu.Lock()
	for (len(e.queue) > 0 || e.busy) && !e.done {
		e.emptyCond.Wait()
	}
	e.mu.Unlock()
}

func (e *Enhancer) worker() {
	for {
		e.mu.Lock()
		for len(e.queue) == 0 && !e.done {
			e.cond.Wait()
		}
		if e.done && len(e.queue) == 0 {
			e.mu.Unlock()
			return
		}
		rel := e.queue[0]
		e.queue = e.queue[1:]
		e.busy = true
		e.mu.Unlock()

		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[enh] panic 已忽略: %v", r)
				}
			}()
			e.process(rel)
		}()

		e.mu.Lock()
		e.busy = false
		if len(e.queue) == 0 {
			e.emptyCond.Broadcast()
		}
		e.mu.Unlock()
	}
}

// process 单张增强：读文件 → ×3 放大 → 锐化 → 还原原尺寸 → 原子写回。
func (e *Enhancer) process(rel string) {
	full := filepath.Join(e.dataDir, "images", filepath.FromSlash(rel))
	img := gocv.IMRead(full, gocv.IMReadColor)
	if img.Empty() {
		return
	}
	defer img.Close()
	w, h := img.Cols(), img.Rows()
	if w < 24 || h < 24 || w > 800 || h > 800 {
		return // 太小没有增强意义 / 已经够大不浪费 CPU
	}

	// 1) ×3 三次插值放大
	up := gocv.NewMat()
	defer up.Close()
	gocv.Resize(img, &up, image.Pt(w*upscaleFactor, h*upscaleFactor), 0, 0, gocv.InterpolationCubic)

	// 2) 轻锐化（unsharp mask：1.4×原图 − 0.4×高斯模糊）
	blur := gocv.NewMat()
	defer blur.Close()
	gocv.GaussianBlur(up, &blur, image.Pt(0, 0), 2, 2, gocv.BorderDefault)
	sharp := gocv.NewMat()
	defer sharp.Close()
	gocv.AddWeighted(up, 1.4, blur, -0.4, 0, &sharp)

	// 3) Lanczos4 还原到原始尺寸：比 Area 保留更多微对比度，消除模糊感
	out := gocv.NewMat()
	defer out.Close()
	gocv.Resize(sharp, &out, image.Pt(w, h), 0, 0, gocv.InterpolationLanczos4)

	// 4) 原子写回原路径：媒体接口下次读取即是清晰版
	// 注意：imwrite 按扩展名选编码器，临时文件必须以 .jpg 结尾
	tmp := full + ".tmp.jpg"
	if !gocv.IMWriteWithParams(tmp, out, []int{gocv.IMWriteJpegQuality, 92}) {
		os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, full); err != nil {
		os.Remove(tmp)
		return
	}
	if st, err := os.Stat(full); err == nil {
		e.mu.Lock()
		e.seen[rel] = st.Size()
		e.mu.Unlock()
	}
}
