package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"vi/internal/api"
	"vi/internal/auth"
	"vi/internal/config"
	"vi/internal/geo"
	"vi/internal/importface"
	"vi/internal/qdrant"
	"vi/internal/report"
	"vi/internal/store"
)

// Version 由 -ldflags "-X main.Version=xxx" 在构建时注入；空串显示 "dev"。
var Version = "dev"

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func mustFloat(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		log.Fatalf("配置值不是合法数字: %s", s)
	}
	return v
}

func mustInt(s string) int {
	v, err := strconv.Atoi(s)
	if err != nil {
		log.Fatalf("配置值不是合法整数: %s", s)
	}
	return v
}

func loadConfig() config.Config {
	c := config.Config{}
	flag.StringVar(&c.Addr, "addr", env("VI_ADDR", "127.0.0.1:8688"), "HTTP 监听地址")
	flag.StringVar(&c.DataDir, "data", env("VI_DATA", "data"), "数据目录")
	flag.StringVar(&c.ModelsDir, "models", env("VI_MODELS", "models"), "模型目录")
	flag.StringVar(&c.WebDir, "web", env("VI_WEB", "web"), "前端目录")
	flag.StringVar(&c.QdrantURL, "qdrant", env("VI_QDRANT", "http://127.0.0.1:6333"), "Qdrant 地址")
	flag.StringVar(&c.FaceCol, "face-collection", env("VI_FACE_COLLECTION", "vi_faces"), "人脸向量集合")
	flag.StringVar(&c.PlateCol, "plate-collection", env("VI_PLATE_COLLECTION", "vi_plates"), "车牌向量集合")
	th := env("VI_FACE_THRESHOLD", "0.45")
	flag.Float64Var(&c.FaceThresh, "face-threshold", mustFloat(th), "同一人脸判定阈值(余弦相似度)")
	cd := env("VI_COOLDOWN_SECONDS", "60")
	flag.DurationVar(&c.Cooldown, "cooldown", time.Duration(mustInt(cd))*time.Second, "同一目标重复入库冷却时间")
	flag.StringVar(&c.GeoURL, "geocode-url", env("VI_GEOCODE_URL", "https://api.bigdatacloud.net/data/reverse-geocode-client"), "逆地理编码接口（Open-Meteo 备用）")
	flag.StringVar(&c.AmapKey, "amap-key", env("QuietShield", ""), "高德 Web Service Key（国内坐标更准，留空则走 Open-Meteo）")
	flag.StringVar(&c.SummaryAt, "summary-at", env("VI_SUMMARY_AT", "23:50"), "每日总结生成时间 HH:MM")
	flag.Parse()

	loc, err := time.LoadLocation(env("VI_TZ", "Asia/Shanghai"))
	if err != nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	c.Location = loc
	if abs, err := filepath.Abs(c.DataDir); err == nil {
		c.DataDir = abs
	}
	return c
}

/*
go build -trimpath -o ./bin/vi -ldflags "-s -w" main.go
*/
func main() {
	// 子命令分支
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve":
			os.Exit(runServe())
		case "importfaces", "import":
			os.Exit(runImportFaces(os.Args[2:]))
		case "reset":
			os.Exit(runReset(os.Args[2:]))
		case "version", "--version", "-v":
			fmt.Println("vi", Version)
			return
		case "help", "--help", "-h":
			printUsage()
			return
		}
	}
	// 默认 serve
	os.Exit(runServe())
}

func printUsage() {
	fmt.Println("vi — 视频监控系统")
	fmt.Println()
	fmt.Println("用法:")
	fmt.Println("  vi serve [flags]              启动 HTTP 服务")
	fmt.Println("  vi importfaces [flags] -dir   导入本地图片（检测人脸+提取特征+入库）")
	fmt.Println("  vi reset [-data dir]          清空所有数据（事件、抠图、向量）")
	fmt.Println("  vi version                    打印版本")
	fmt.Println("  vi help                       打印本帮助")
	fmt.Println()
	fmt.Println("环境变量:")
	fmt.Println("  VI_ADDR, VI_DATA, VI_MODELS, VI_WEB, VI_QDRANT, VI_FACE_THRESHOLD, VI_COOLDOWN_SECONDS")
	fmt.Println("  QuietShield                   高德 Web Service Key")
	fmt.Println()
	fmt.Println("全局 flags (serve):")
	flag.CommandLine.PrintDefaults()
}

// runServe 启动 HTTP 服务（原有逻辑）。
func runServe() int {
	cfg := loadConfig()
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	for _, d := range []string{
		filepath.Join(cfg.DataDir, "images"),
		filepath.Join(cfg.DataDir, "reports"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			log.Fatalf("创建目录失败 %s: %v", d, err)
		}
	}

	qd := qdrant.NewQdrant(cfg.QdrantURL, cfg.FaceCol, cfg.PlateCol)
	if err := qd.Ensure(); err != nil {
		log.Printf("[warn] Qdrant 初始化失败（人脸同一性匹配将退化为新建身份）: %v", err)
	}

	g := geo.NewGeocoder(cfg.GeoURL, cfg.AmapKey, filepath.Join(cfg.DataDir, "geo_cache.json"))
	st, err := store.NewStore(cfg, qd, g)
	if err != nil {
		log.Fatalf("加载数据失败: %v", err)
	}

	go report.RunSummaryScheduler(cfg, st)

	if err := auth.LoadUsers(cfg.DataDir); err != nil {
		log.Fatalf("加载用户失败: %v", err)
	}
	if err := auth.LoadMediaSecret(cfg.DataDir); err != nil {
		log.Fatalf("加载媒体签名密钥失败: %v", err)
	}

	srv := api.NewServer(cfg, st)
	log.Printf("vi 已启动: http://%s  (人脸阈值 %.2f, 冷却 %s)", cfg.Addr, cfg.FaceThresh, cfg.Cooldown)
	if err := http.ListenAndServe(cfg.Addr, srv); err != nil {
		log.Fatal(err)
	}
	return 0
}

// runReset 清空所有数据：events.jsonl、抠图文件、Qdrant 向量。
// 用于重新导入前的清理。
//
//	vi reset [-data dir] [-qdrant url] [-face-collection name] [-plate-collection name] [-yes]
func runReset(args []string) int {
	fs := flag.NewFlagSet("reset", flag.ExitOnError)
	dataDir := fs.String("data", env("VI_DATA", "data"), "数据目录")
	qdURL := fs.String("qdrant", env("VI_QDRANT", "http://127.0.0.1:6333"), "Qdrant 地址")
	faceCol := fs.String("face-collection", env("VI_FACE_COLLECTION", "vi_faces"), "人脸集合名")
	plateCol := fs.String("plate-collection", env("VI_PLATE_COLLECTION", "vi_plates"), "车牌集合名")
	yes := fs.Bool("yes", false, "跳过确认直接执行")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	if !*yes {
		fmt.Printf("⚠️  将清空以下数据:\n")
		fmt.Printf("   事件文件: %s/events.jsonl\n", *dataDir)
		fmt.Printf("   抠图目录: %s/images/\n", *dataDir)
		fmt.Printf("   Qdrant:   %s 集合 %s / %s\n", *qdURL, *faceCol, *plateCol)
		fmt.Printf("\n确认执行? 输入 yes 继续: ")
		var confirm string
		fmt.Scanln(&confirm)
		if confirm != "yes" {
			fmt.Println("已取消")
			return 0
		}
	}

	// 1. 清空事件文件
	eventsFile := filepath.Join(*dataDir, "events.jsonl")
	if err := os.Truncate(eventsFile, 0); err != nil && !os.IsNotExist(err) {
		log.Printf("[reset] 清空 events.jsonl 失败: %v", err)
	} else {
		log.Printf("[reset] ✅ events.jsonl 已清空")
	}

	// 2. 清空抠图目录
	imagesDir := filepath.Join(*dataDir, "images")
	if err := os.RemoveAll(imagesDir); err != nil {
		log.Printf("[reset] 删除 images 目录失败: %v", err)
	} else {
		log.Printf("[reset] ✅ images/ 已删除")
	}

	// 3. 清空标签与告警文件
	for _, f := range []string{"labels.json", "alerts.jsonl"} {
		p := filepath.Join(*dataDir, f)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			log.Printf("[reset] 删除 %s 失败: %v", f, err)
		} else {
			log.Printf("[reset] ✅ %s 已删除", f)
		}
	}

	// 4. 清空 Qdrant 集合
	for _, col := range []string{*faceCol, *plateCol} {
		url := fmt.Sprintf("%s/collections/%s", *qdURL, col)
		req, _ := http.NewRequest("DELETE", url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Printf("[reset] 删除 Qdrant 集合 %s 失败: %v", col, err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == 200 {
			log.Printf("[reset] ✅ Qdrant 集合 %s 已删除", col)
		} else {
			log.Printf("[reset] Qdrant 集合 %s 删除返回 %d", col, resp.StatusCode)
		}
	}

	fmt.Println("\n✅ 所有数据已清空。启动 vi serve 后会自动重建空集合。")
	return 0
}

// runImportFaces 实现 vi importfaces 子命令。
// 扫描 -dir 指定目录递归，Haar cascade / YuNet 检测人脸 → arc.onnx 提取 embedding → upsert 到 Qdrant。
// 同时读取图片 EXIF：GPS 经纬度/海拔 → 逆地理编码得地址；图片创建时间作为 captured_at。
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
		g := geo.NewGeocoder(env("VI_GEOCODE_URL", ""), env("QuietShield", ""), filepath.Join(cfg.DataDir, "geo_cache.json"))
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
