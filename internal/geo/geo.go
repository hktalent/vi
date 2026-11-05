package geo

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// WGS84ToGCJ02 把 WGS-84（GPS 原始坐标）转为 GCJ-02（火星坐标系）。
// 中国境外坐标不做偏移，直接返回原值。
func WGS84ToGCJ02(lng, lat float64) (float64, float64) {
	const (
		a  = 6378245.0              // WGS-84 长半轴
		ee = 0.00669342162296594323 // 偏心率平方
	)
	outOfChina := func(l, b float64) bool {
		return !(l > 73.66 && l < 135.05 && b > 3.86 && b < 53.55)
	}
	if outOfChina(lng, lat) {
		return lng, lat
	}

	transformLat := func(x, y float64) float64 {
		ret := -100.0 + 2.0*x + 3.0*y + 0.2*y*y + 0.1*x*y + 0.2*math.Sqrt(math.Abs(x))
		ret += (20.0*math.Sin(6.0*x*math.Pi) + 20.0*math.Sin(2.0*x*math.Pi)) * 2.0 / 3.0
		ret += (20.0*math.Sin(y*math.Pi) + 40.0*math.Sin(y/3.0*math.Pi)) * 2.0 / 3.0
		ret += (160.0*math.Sin(y/12.0*math.Pi) + 320.0*math.Sin(y*math.Pi/30.0)) * 2.0 / 3.0
		return ret
	}
	transformLng := func(x, y float64) float64 {
		ret := 300.0 + x + 2.0*y + 0.1*x*x + 0.1*x*y + 0.1*math.Sqrt(math.Abs(x))
		ret += (20.0*math.Sin(6.0*x*math.Pi) + 20.0*math.Sin(2.0*x*math.Pi)) * 2.0 / 3.0
		ret += (20.0*math.Sin(x*math.Pi) + 40.0*math.Sin(x/3.0*math.Pi)) * 2.0 / 3.0
		ret += (150.0*math.Sin(x/12.0*math.Pi) + 300.0*math.Sin(x/30.0*math.Pi)) * 2.0 / 3.0
		return ret
	}

	dLat := transformLat(lng-105.0, lat-35.0)
	dLng := transformLng(lng-105.0, lat-35.0)
	radLat := lat / 180.0 * math.Pi
	magic := math.Sin(radLat)
	magic = 1 - ee*magic*magic
	sqrtMagic := math.Sqrt(magic)
	dLat = (dLat * 180.0) / ((a * (1 - ee)) / (magic*sqrtMagic) * math.Pi)
	dLng = (dLng * 180.0) / (a / sqrtMagic * math.Cos(radLat) * math.Pi)
	return lng + dLng, lat + dLat
}

// cachedAddr 是一条缓存记录：地址 + 写入时间 + 是否为"不好"的 fallback（纯经纬度字符串）
type cachedAddr struct {
	Addr string `json:"addr"`
	At   int64  `json:"at"`   // Unix 秒
	Bad  bool   `json:"bad"`  // true = 是 fallback 经纬度/空地址，有高德 key 时应强制刷新
	Src  string `json:"src"`  // amap | openmeteo | fallback
}

// Geocoder 调用逆地理编码服务，把经纬度转换为中文地址，并做本地缓存。
// 优先走高德（AMAP），国内坐标更准；没配 AMAP key 时回退 Open-Meteo。
type Geocoder struct {
	base    string
	amapKey string
	cli     *http.Client
	file    string
	mu      sync.Mutex
	cache   map[string]cachedAddr
	dirty   bool
}

func NewGeocoder(base, amapKey, file string) *Geocoder {
	g := &Geocoder{
		base:    base,
		amapKey: amapKey,
		file:    file,
		cli:     &http.Client{Timeout: 6 * time.Second},
		cache:   map[string]cachedAddr{},
	}
	if b, err := os.ReadFile(file); err == nil {
		// 兼容旧格式：map[string]string
		var old map[string]string
		if json.Unmarshal(b, &old) == nil && len(old) > 0 {
			for k, v := range old {
				g.cache[k] = cachedAddr{Addr: v, At: time.Now().Unix(), Bad: isFallbackAddr(v), Src: "legacy"}
			}
		} else {
			json.Unmarshal(b, &g.cache)
		}
	}
	if amapKey != "" {
		log.Printf("[geo] 已配置高德 Key（%s…），共 %d 条缓存", amapKey[:min(8, len(amapKey))], len(g.cache))
	} else {
		log.Printf("[geo] 未配置高德 Key，使用 Open-Meteo 备用服务，共 %d 条缓存", len(g.cache))
	}
	return g
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// isFallbackAddr 判断地址是否是"不好"的 fallback（纯经纬度字符串如 "31.23456,121.45678"）
func isFallbackAddr(s string) bool {
	// 纯数字+逗号+小数点 → 是 fallback 经纬度
	var hasDigit, hasComma, hasLetter bool
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9' || r == '.' || r == '-':
			hasDigit = true
		case r == ',':
			hasComma = true
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r > 0x80:
			hasLetter = true
		}
	}
	return hasDigit && hasComma && !hasLetter
}

func geoKey(lat, lon float64) string {
	return fmt.Sprintf("%.3f,%.3f", lat, lon)
}

// Reverse 返回地址；服务不可用时退化为经纬度字符串，保证采集不中断。
func (g *Geocoder) Reverse(lat, lon *float64) string {
	if lat == nil || lon == nil {
		return ""
	}
	key := geoKey(*lat, *lon)

	// 缓存命中且不是 bad（经纬度 fallback）→ 直接返回
	// 如果有高德 key 且缓存标记为 bad → 强制刷新（旧 Open-Meteo 结果换高德）
	g.mu.Lock()
	if v, ok := g.cache[key]; ok {
		forceRefresh := v.Bad && g.amapKey != ""
		if !forceRefresh {
			g.mu.Unlock()
			return v.Addr
		}
		log.Printf("[geo] 缓存是 fallback 经纬度，有高德 key，强制刷新: %s", key)
	}
	g.mu.Unlock()

	addr, src := g.fetch(*lat, *lon)
	bad := addr == "" || isFallbackAddr(addr)
	if bad && addr == "" {
		addr = fmt.Sprintf("%.5f,%.5f", *lat, *lon)
	}

	g.mu.Lock()
	g.cache[key] = cachedAddr{Addr: addr, At: time.Now().Unix(), Bad: bad, Src: src}
	g.dirty = true
	g.mu.Unlock()
	g.flush()

	latStr, lonStr := fmt.Sprintf("%.5f", *lat), fmt.Sprintf("%.5f", *lon)
	if bad {
		log.Printf("[geo] [%s] 逆地理编码返回 fallback: %s (%s, %s)", src, addr, latStr, lonStr)
	} else {
		log.Printf("[geo] [%s] %s ← (%s, %s)", src, addr, latStr, lonStr)
	}
	return addr
}

// fetch 返回 (地址, 来源标识)
func (g *Geocoder) fetch(lat, lon float64) (string, string) {
	if g.amapKey != "" {
		if addr := g.fetchAmap(lat, lon); addr != "" && !isFallbackAddr(addr) {
			return addr, "amap"
		}
	}
	if addr := g.fetchOpenMeteo(lat, lon); addr != "" {
		return addr, "openmeteo"
	}
	return "", "fallback"
}

// fetchAmap 调用高德 Web Service 逆地理编码。
// 高德只认 GCJ-02（火星坐标系），入参 WGS-84 先转换。
func (g *Geocoder) fetchAmap(wgsLat, wgsLon float64) string {
	gcjLon, gcjLat := WGS84ToGCJ02(wgsLon, wgsLat)
	url := fmt.Sprintf(
		"https://restapi.amap.com/v3/geocode/regeo?key=%s&location=%.6f,%.6f&extensions=base",
		g.amapKey, gcjLon, gcjLat,
	)
	resp, err := g.cli.Get(url)
	if err != nil {
		log.Printf("[geo] 高德逆地理编码网络失败: %v", err)
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Printf("[geo] 高德 HTTP %d", resp.StatusCode)
		return ""
	}
	var d struct {
		Status    string `json:"status"`
		Info      string `json:"info"`
		Regeocode struct {
			FormattedAddress string `json:"formatted_address"`
		} `json:"regeocode"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		log.Printf("[geo] 高德响应解析失败: %v", err)
		return ""
	}
	if d.Status != "1" {
		log.Printf("[geo] 高德返回非成功: status=%s info=%s", d.Status, d.Info)
		return ""
	}
	return strings.TrimSpace(d.Regeocode.FormattedAddress)
}

// fetchOpenMeteo 走 Open-Meteo（bigdatacloud）逆地理编码。
// 国际服务直接吃 WGS-84，不需要转换坐标系。
func (g *Geocoder) fetchOpenMeteo(lat, lon float64) string {
	if g.base == "" {
		return ""
	}
	u, err := url.Parse(g.base)
	if err != nil {
		return ""
	}
	q := u.Query()
	q.Set("latitude", fmt.Sprintf("%f", lat))
	q.Set("longitude", fmt.Sprintf("%f", lon))
	q.Set("localityLanguage", "zh")
	u.RawQuery = q.Encode()
	resp, err := g.cli.Get(u.String())
	if err != nil {
		log.Printf("[geo] Open-Meteo 逆地理编码失败: %v", err)
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	var d struct {
		CountryName          string `json:"countryName"`
		PrincipalSubdivision string `json:"principalSubdivision"`
		City                 string `json:"city"`
		Locality             string `json:"locality"`
		LocalityInfo         struct {
			Administrative []struct {
				Name  string `json:"name"`
				Order int    `json:"order"`
			} `json:"administrative"`
		} `json:"localityInfo"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return ""
	}
	var parts []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		for _, p := range parts {
			if p == s {
				return
			}
		}
		parts = append(parts, s)
	}
	for _, a := range d.LocalityInfo.Administrative {
		add(a.Name)
	}
	add(d.PrincipalSubdivision)
	add(d.City)
	add(d.Locality)
	if len(parts) == 0 {
		add(d.CountryName)
	}
	return strings.Join(parts, " ")
}

func (g *Geocoder) flush() {
	g.mu.Lock()
	if !g.dirty {
		g.mu.Unlock()
		return
	}
	b, _ := json.Marshal(g.cache)
	g.dirty = false
	g.mu.Unlock()
	if err := os.WriteFile(g.file, b, 0o644); err != nil {
		log.Printf("[geo] 写缓存失败: %v", err)
	}
}
