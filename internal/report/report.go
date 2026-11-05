package report

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"vi/internal/config"
	"vi/internal/store"
)

// WriteSummary 生成某一天的 Markdown 总结文档，返回文件绝对路径。
func WriteSummary(cfg config.Config, st *store.Store, day time.Time) (string, error) {
	loc := cfg.Location
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	end := start.AddDate(0, 0, 1)

	var events []*store.Event
	for _, e := range st.Events() {
		ts := e.CapturedAt.In(loc)
		if !ts.Before(start) && ts.Before(end) {
			events = append(events, e)
		}
	}

	f := newSummaryFacts(events, st, cfg.Location)
	f.day = start
	f.events = events

	var b strings.Builder
	fmt.Fprintf(&b, "# vi 每日总结 %s\n\n", start.Format("2006-01-02"))
	fmt.Fprintf(&b, "> 生成时间：%s\n\n", time.Now().In(loc).Format("2006-01-02 15:04:05"))

	b.WriteString("## 概览\n\n")
	fmt.Fprintf(&b, "- 见到的人脸次数：**%d**\n", f.faces)
	fmt.Fprintf(&b, "- 不同人员数量：**%d**\n", f.uniquePersons)
	fmt.Fprintf(&b, "- 见到的车牌次数：**%d**\n", f.plates)
	fmt.Fprintf(&b, "- 不同车牌数量：**%d**\n", f.uniquePlates)
	fmt.Fprintf(&b, "- 标记过的人/车记录：**%d**\n", f.labeledEvents)
	if len(f.labelHits) > 0 {
		fmt.Fprintf(&b, "- 出现过的标签：%s\n", strings.Join(f.labelNames(), "、"))
	}
	b.WriteString("\n")

	b.WriteString("## 标签命中\n\n")
	if len(f.labelHits) == 0 {
		b.WriteString("当天没有标记过的人或车出现。\n\n")
	} else {
		b.WriteString("| 标签 | 类型 | 出现次数 | 首次 | 最近 |\n|---|---|---|---|---|\n")
		for _, h := range f.sortedLabelHits() {
			fmt.Fprintf(&b, "| %s | %s | %d | %s | %s |\n",
				h.Name, kindCN(h.Kind), h.Count,
				h.First.Format("15:04:05"), h.Last.Format("15:04:05"))
		}
		b.WriteString("\n")
	}

	b.WriteString("## 时段分布\n\n")
	b.WriteString("| 小时 | 人脸 | 车牌 |\n|---|---|---|\n")
	for h := 0; h < 24; h++ {
		if f.hourFaces[h] == 0 && f.hourPlates[h] == 0 {
			continue
		}
		fmt.Fprintf(&b, "| %02d:00 | %d | %d |\n", h, f.hourFaces[h], f.hourPlates[h])
	}
	b.WriteString("\n")

	b.WriteString("## 高频地点\n\n")
	if len(f.placeCounts) == 0 {
		b.WriteString("当天没有可用的地址信息（可能未授权定位或逆地理编码不可用）。\n\n")
	} else {
		b.WriteString("| 地址 | 次数 |\n|---|---|\n")
		for _, p := range f.topPlaces(6) {
			fmt.Fprintf(&b, "| %s | %d |\n", p.name, p.count)
		}
		b.WriteString("\n")
	}

	b.WriteString("## 最近记录\n\n")
	limit := 20
	if len(events) < limit {
		limit = len(events)
	}
	for _, e := range events[:limit] {
		kind := "人脸"
		what := ""
		if e.Kind == "plate" {
			kind = "车牌"
			what = e.PlateText
		} else {
			for _, l := range st.LabelsFor(e) {
				what = l.Name
				break
			}
			if what == "" {
				what = shortID(e.PersonID)
			}
			if attrs := faceAttrs(e); attrs != "" {
				what += "（" + attrs + "）"
			}
		}
		fmt.Fprintf(&b, "- %s %s %s | %s | %s\n",
			e.CapturedAt.In(loc).Format("15:04:05"), kind, what, e.Address, imageLink(e.Image))
	}
	b.WriteString("\n")

	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "reports"), 0o755); err != nil {
		return "", err
	}
	name := "summary-" + start.Format("2006-01-02") + ".md"
	p := filepath.Join(cfg.DataDir, "reports", name)
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return p, nil
}

func imageLink(rel string) string {
	if rel == "" {
		return ""
	}
	return "[图片](../images/" + strings.ReplaceAll(rel, string(filepath.Separator), "/") + ")"
}

func shortID(id string) string {
	if len(id) > 10 {
		return id[:10]
	}
	return id
}

func kindCN(k string) string {
	if k == "plate" {
		return "车牌"
	}
	return "人脸"
}

func faceAttrs(e *store.Event) string {
	if e.Kind != "face" {
		return ""
	}
	var parts []string
	switch e.Gender {
	case "male":
		parts = append(parts, "男")
	case "female":
		parts = append(parts, "女")
	}
	if e.AgeRange != "" {
		parts = append(parts, e.AgeRange+"岁")
	}
	switch e.Glasses {
	case "yes":
		parts = append(parts, "戴眼镜")
	case "no":
		parts = append(parts, "无眼镜")
	}
	return strings.Join(parts, "·")
}

type labelHit struct {
	Name  string
	Kind  string
	Count int
	First time.Time
	Last  time.Time
}

type placeCount struct {
	name  string
	count int
}

type summaryFacts struct {
	day           time.Time
	events        []*store.Event
	faces         int
	plates        int
	uniquePersons int
	uniquePlates  int
	labeledEvents int
	hourFaces     [24]int
	hourPlates    [24]int
	labelHits     map[string]*labelHit
	placeCounts   map[string]int
}

func newSummaryFacts(events []*store.Event, st *store.Store, loc *time.Location) *summaryFacts {
	f := &summaryFacts{
		labelHits:   map[string]*labelHit{},
		placeCounts: map[string]int{},
	}
	persons := map[string]bool{}
	plates := map[string]bool{}
	for _, e := range events {
		h := e.CapturedAt.In(loc).Hour()
		if e.Kind == "face" {
			f.faces++
			f.hourFaces[h]++
			persons[e.PersonID] = true
		} else {
			f.plates++
			f.hourPlates[h]++
			plates[e.PlateText] = true
		}
		if e.Address != "" {
			f.placeCounts[e.Address]++
		}
		ls := st.LabelsFor(e)
		if len(ls) > 0 {
			f.labeledEvents++
		}
		for _, l := range ls {
			h := f.labelHits[l.ID]
			if h == nil {
				h = &labelHit{Name: l.Name, Kind: l.Kind, First: e.CapturedAt}
				f.labelHits[l.ID] = h
			}
			h.Count++
			if e.CapturedAt.Before(h.First) {
				h.First = e.CapturedAt
			}
			if e.CapturedAt.After(h.Last) {
				h.Last = e.CapturedAt
			}
		}
	}
	f.uniquePersons = len(persons)
	f.uniquePlates = len(plates)
	return f
}

func (f *summaryFacts) labelNames() []string {
	var out []string
	for _, h := range f.labelHits {
		out = append(out, h.Name)
	}
	sort.Strings(out)
	return out
}

func (f *summaryFacts) sortedLabelHits() []*labelHit {
	var out []*labelHit
	for _, h := range f.labelHits {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

func (f *summaryFacts) topPlaces(n int) []placeCount {
	var out []placeCount
	for k, v := range f.placeCounts {
		out = append(out, placeCount{name: k, count: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].count > out[j].count })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// RunSummaryScheduler 每天在配置的时间生成当天总结。
func RunSummaryScheduler(cfg config.Config, st *store.Store) {
	for {
		next := nextSummaryTime(time.Now().In(cfg.Location), cfg.SummaryAt, cfg.Location)
		log.Printf("[summary] 下次生成时间: %s", next.Format("2006-01-02 15:04:05"))
		time.Sleep(time.Until(next))
		now := time.Now().In(cfg.Location)
		p, err := WriteSummary(cfg, st, now)
		if err != nil {
			log.Printf("[summary] 生成失败: %v", err)
			continue
		}
		log.Printf("[summary] 已生成: %s", p)
	}
}

func nextSummaryTime(now time.Time, at string, loc *time.Location) time.Time {
	var hh, mm int
	fmt.Sscanf(at, "%d:%d", &hh, &mm)
	target := time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, loc)
	if !target.After(now) {
		target = target.AddDate(0, 0, 1)
	}
	return target
}
