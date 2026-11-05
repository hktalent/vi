package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Geocoder 调用逆地理编码服务，把经纬度转换为中文地址，并做本地缓存。
type Geocoder struct {
	base  string
	cli   *http.Client
	file  string
	mu    sync.Mutex
	cache map[string]string
	dirty bool
}

func NewGeocoder(base, file string) *Geocoder {
	g := &Geocoder{
		base:  base,
		file:  file,
		cli:   &http.Client{Timeout: 6 * time.Second},
		cache: map[string]string{},
	}
	if b, err := os.ReadFile(file); err == nil {
		json.Unmarshal(b, &g.cache)
	}
	return g
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
	g.mu.Lock()
	if v, ok := g.cache[key]; ok {
		g.mu.Unlock()
		return v
	}
	g.mu.Unlock()

	addr := g.fetch(*lat, *lon)
	if addr == "" {
		addr = fmt.Sprintf("%.5f,%.5f", *lat, *lon)
	}
	g.mu.Lock()
	g.cache[key] = addr
	g.dirty = true
	g.mu.Unlock()
	g.flush()
	return addr
}

func (g *Geocoder) fetch(lat, lon float64) string {
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
		log.Printf("[geo] 逆地理编码失败: %v", err)
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
