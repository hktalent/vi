package config

import "time"

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
	GeoURL     string // Open-Meteo 备用 URL（没配高德 key 时使用）
	AmapKey    string // 高德 Web Service Key（配了就优先走高德逆地理编码）
	SummaryAt  string
	Location   *time.Location
}
