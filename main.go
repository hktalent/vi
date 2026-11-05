package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Version 由 -ldflags "-X main.Version=xxx" 在构建时注入；空串显示 "dev"。
var Version = "dev"

// Config 是所有运行参数的集合，可用命令行参数或环境变量覆盖。
type Config struct {
	Addr       string
	DataDir    string
	ModelsDir  string
	WebDir     string
	QdrantURL  string
	FaceCol    string
	PlateCol   string
	FaceThresh float64
	Cooldown   time.Duration
	GeoURL     string
	SummaryAt  string
	Location   *time.Location
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func loadConfig() Config {
	c := Config{}
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
	flag.StringVar(&c.GeoURL, "geocode-url", env("VI_GEOCODE_URL", "https://api.bigdatacloud.net/data/reverse-geocode-client"), "逆地理编码接口")
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

func main() {
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

	qd := NewQdrant(cfg.QdrantURL, cfg.FaceCol, cfg.PlateCol)
	if err := qd.Ensure(); err != nil {
		log.Printf("[warn] Qdrant 初始化失败（人脸同一性匹配将退化为新建身份）: %v", err)
	}

	geo := NewGeocoder(cfg.GeoURL, filepath.Join(cfg.DataDir, "geo_cache.json"))
	st, err := NewStore(cfg, qd, geo)
	if err != nil {
		log.Fatalf("加载数据失败: %v", err)
	}

	go runSummaryScheduler(cfg, st)

	if err := loadUsers(cfg.DataDir); err != nil {
		log.Fatalf("加载用户失败: %v", err)
	}

	srv := NewServer(cfg, st)
	log.Printf("vi 已启动: http://%s  (人脸阈值 %.2f, 冷却 %s)", cfg.Addr, cfg.FaceThresh, cfg.Cooldown)
	if err := http.ListenAndServe(cfg.Addr, srv); err != nil {
		log.Fatal(err)
	}
}
