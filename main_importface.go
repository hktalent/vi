//go:build importface

package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"vi/internal/config"
	"vi/internal/geo"
	"vi/internal/importface"
	"vi/internal/qdrant"
	"vi/internal/store"
)

// runImportFaces 实现 vi importfaces 子命令。
// 扫描 -dir 指定目录递归，Haar cascade 检测人脸 → arc.onnx 提取 embedding → upsert 到 Qdrant。
//
//	vi importfaces -dir "/path/to/photos" [-label "婚礼"] [-dry-run] [-force] [-cascade path] [-arc path] [-qdrant url]
func runImportFaces(args []string) int {
	fs := flag.NewFlagSet("importfaces", flag.ExitOnError)
	dir := fs.String("dir", "", "图片目录（必填，递归扫描所有子目录）")
	label := fs.String("label", "", "可选标签，作为 PersonID 前缀（如 \"婚礼\"、\"团建\"）")
	dryRun := fs.Bool("dry-run", false, "只扫描+检测，不上传 Qdrant")
	force := fs.Bool("force", false, "强制重新导入（忽略已有记录，全部重新处理）")
	cascade := fs.String("cascade", "", "Haar cascade XML 路径（默认自动搜索 OpenCV 安装路径）")
	arc := fs.String("arc", "", "arc.onnx 路径（默认 ./models/arc.onnx）")
	yunet := fs.String("yunet", "", "YuNet 检测模型路径（默认 ./models/face_detection_yunet_2023mar.onnx，缺失回退 Haar）")
	qdURL := fs.String("qdrant", env("VI_QDRANT", "http://127.0.0.1:6333"), "Qdrant 地址")
	faceCol := fs.String("face-collection", env("VI_FACE_COLLECTION", "vi_faces"), "人脸集合名")
	dataDir := fs.String("data", env("VI_DATA", "data"), "数据目录（保存抠图）")
	modelsDir := fs.String("models", env("VI_MODELS", "models"), "模型目录")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "错误: 必须指定 -dir 参数")
		fmt.Fprintln(os.Stderr, "用法: vi importfaces -dir \"/path/to/photos\" [-label 婚礼]")
		return 2
	}

	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// 解析 YuNet 检测模型路径（可选，存在则优先于 Haar cascade）
	yunetPath := *yunet
	if yunetPath == "" {
		p := filepath.Join(*modelsDir, "face_detection_yunet_2023mar.onnx")
		if _, err := os.Stat(p); err == nil {
			yunetPath = p
		}
	}

	// 解析级联分类器路径（YuNet 可用时仅作回退，缺失不致命）
	cascadePath := *cascade
	if cascadePath == "" {
		cascadePath = findHaarCascade()
	}
	if _, err := os.Stat(cascadePath); err != nil && yunetPath == "" {
		log.Fatalf("Haar cascade 文件不存在: %s", cascadePath)
	}

	// 解析 arc.onnx 路径
	arcPath := *arc
	if arcPath == "" {
		arcPath = filepath.Join(*modelsDir, "arc.onnx")
	}
	if _, err := os.Stat(arcPath); err != nil {
		log.Fatalf("arc.onnx 模型不存在: %s", arcPath)
	}

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatalf("创建数据目录失败: %v", err)
	}

	cfg := config.Config{DataDir: *dataDir, ModelsDir: *modelsDir, Location: time.FixedZone("CST", 8*3600)}
	qd := qdrant.NewQdrant(*qdURL, *faceCol, env("VI_PLATE_COLLECTION", "vi_plates"))
	if err := qd.Ensure(); err != nil {
		log.Printf("[warn] Qdrant 不可用（dry-run 跳过或返回空）: %v", err)
		if !*dryRun {
			log.Fatalf("Qdrant 连接失败，请先启动 Qdrant 或使用 -dry-run")
		}
	}

	// 创建 Store 实例（写 events.jsonl + Qdrant），让导入的人脸在前端可见
	var st *store.Store
	if !*dryRun {
		g := geo.NewGeocoder("", "", filepath.Join(cfg.DataDir, "geo_cache.json"))
		var err error
		st, err = store.NewStore(cfg, qd, g)
		if err != nil {
			log.Printf("[warn] Store 初始化失败（导入的人脸不会出现在事件列表）: %v", err)
		}
	}

	imp, err := importface.NewImporter(cfg, qd, st, cascadePath, arcPath, yunetPath, *label, *dryRun, *force)
	if err != nil {
		log.Fatalf("初始化 Importer 失败: %v", err)
	}

	log.Printf("[import] 目录: %s", *dir)
	log.Printf("[import] Haar cascade: %s", cascadePath)
	log.Printf("[import] arc.onnx: %s", arcPath)
	log.Printf("[import] Qdrant: %s 集合: %s", *qdURL, *faceCol)
	if *label != "" {
		log.Printf("[import] 标签: %s", *label)
	}
	if *dryRun {
		log.Printf("[import] ⚠️ dry-run 模式：只扫描，不上传 Qdrant")
	}

	n, err := imp.Run(*dir)
	if err != nil {
		log.Printf("[import] 有错误（部分成功）: %v", err)
	}
	fmt.Printf("\n✅ 导入完成：%d 张新人脸\n", n)
	if strings.HasPrefix(*qdURL, "http://127.0.0.1") {
		fmt.Println("💡 启动 vi serve 后即可在首页看到导入的人脸作为同一性识别")
	}
	return 0
}

// findHaarCascade 在常见 OpenCV 安装路径中搜索 haarcascade_frontalface_default.xml。
func findHaarCascade() string {
	candidates := []string{
		// 发布包内置（release 把 cascade 打包到 models/haarcascades/）
		filepath.Join("models", "haarcascades", "haarcascade_frontalface_default.xml"),
		// Homebrew
		"/usr/local/share/opencv4/haarcascades/haarcascade_frontalface_default.xml",
		"/opt/homebrew/share/opencv4/haarcascades/haarcascade_frontalface_default.xml",
		"/usr/local/Cellar/opencv/4.11.0_1/share/opencv4/haarcascades/haarcascade_frontalface_default.xml",
		// 其他常见路径
		"/usr/share/opencv4/haarcascades/haarcascade_frontalface_default.xml",
		"/usr/local/opt/opencv/share/opencv4/haarcascades/haarcascade_frontalface_default.xml",
		// 相对路径 fallback
		filepath.Join("models", "haarcascade_frontalface_default.xml"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return candidates[0] // 找不到返回第一个，让后续 stat 报错
}
