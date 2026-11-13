// Command benchface 是导入链路的阶段耗时基准：解码 / YuNet 检测 / arc 特征提取。
// 用来确认优化是否真的落在瓶颈上，而不是凭感觉改。
//
//	go run ./cmd/benchface -dir data/images/import -n 60
package main

import (
	"flag"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gocv.io/x/gocv"

	"vi/internal/importface"
)

func main() {
	dir := flag.String("dir", "data/images/import", "图片目录")
	models := flag.String("models", "models", "模型目录")
	n := flag.Int("n", 60, "采样图片数量")
	workers := flag.Int("workers", 1, "并行 worker 数（每个 worker 独立 net 实例）")
	flag.Parse()

	yunet, err := importface.NewYuNetDetector(filepath.Join(*models, "face_detection_yunet_2023mar.onnx"))
	if err != nil {
		fmt.Println("YuNet 加载失败:", err)
		os.Exit(1)
	}
	nets := make([]*gocv.Net, *workers)
	for i := range nets {
		net := gocv.ReadNetFromONNX(filepath.Join(*models, "arc.onnx"))
		if net.Empty() {
			fmt.Println("arc.onnx 加载失败")
			os.Exit(1)
		}
		nets[i] = &net
	}

	var paths []string
	exts := map[string]bool{".jpg": true, ".jpeg": true, ".png": true}
	filepath.WalkDir(*dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if exts[strings.ToLower(filepath.Ext(p))] {
			paths = append(paths, p)
		}
		return nil
	})
	if len(paths) > *n {
		paths = paths[:*n]
	}
	if len(paths) == 0 {
		fmt.Println("没有图片")
		os.Exit(1)
	}

	// 全部人脸（解码+检测串行，特征提取按 workers 并行）压进任务队列，
	// 测的是"多 net 实例能否线性扩展"这一关键假设。
	type job struct {
		img  gocv.Mat
		rect image.Rectangle
	}
	jobs := make(chan job, 64)
	var wg sync.WaitGroup
	var mu sync.Mutex
	faces := 0
	var tEmbed time.Duration
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(net *gocv.Net) {
			defer wg.Done()
			for j := range jobs {
				t2 := time.Now()
				importface.EmbedWithNet(net, j.img, j.rect)
				d := time.Since(t2)
				mu.Lock()
				tEmbed += d
				faces++
				mu.Unlock()
			}
		}(nets[w])
	}

	tStart := time.Now()
	var tDecode, tDetect time.Duration
	for _, p := range paths {
		t0 := time.Now()
		img := importface.ReadImageMat(p)
		tDecode += time.Since(t0)
		if img.Empty() {
			continue
		}
		t1 := time.Now()
		det := yunet.Detect(img)
		tDetect += time.Since(t1)
		for _, f := range det {
			r := f.Rect.Intersect(image.Rect(0, 0, img.Cols(), img.Rows()))
			if r.Dx() < 40 || r.Dy() < 40 {
				continue
			}
			jobs <- job{img: img, rect: r}
		}
	}
	close(jobs)
	wg.Wait()
	wall := time.Since(tStart)

	per := func(d time.Duration, k int) string {
		if k == 0 {
			return "0ns"
		}
		return (d / time.Duration(k)).Round(time.Microsecond).String()
	}
	durs := []time.Duration{tDecode, tDetect, tEmbed}
	sort.Slice(durs, func(i, j int) bool { return durs[i] > durs[j] })
	fmt.Printf("图片 %d 张 / 人脸 %d 张 / worker %d（墙钟 %s，吞吐 %.2f 人/秒）\n",
		len(paths), faces, *workers, wall.Round(time.Millisecond), float64(faces)/wall.Seconds())
	fmt.Printf("  解码    合计 %-10s 均值 %s\n", tDecode.Round(time.Millisecond), per(tDecode, len(paths)))
	fmt.Printf("  YuNet   合计 %-10s 均值 %s\n", tDetect.Round(time.Millisecond), per(tDetect, len(paths)))
	fmt.Printf("  arc特征 合计 %-10s 均值 %s\n", tEmbed.Round(time.Millisecond), per(tEmbed, faces))
	fmt.Printf("  主要开销: %s（串行累计 %s）\n", durs[0].Round(time.Millisecond), (tDecode + tDetect + tEmbed).Round(time.Millisecond))
}
