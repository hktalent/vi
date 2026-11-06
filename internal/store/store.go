package store

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"vi/internal/config"
	"vi/internal/geo"
	"vi/internal/qdrant"
)

// Event 是一条采集记录：人脸或车牌的一次"遇见"。
type Event struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"` // face | plate
	CapturedAt time.Time `json:"captured_at"`
	Lat        *float64  `json:"lat,omitempty"`
	Lon        *float64  `json:"lon,omitempty"`
	Address    string    `json:"address"`
	Altitude   *float64  `json:"altitude,omitempty"` // 海拔（米）
	Heading    *float64  `json:"heading,omitempty"`  // 相机方向角度（0=北, 90=东）
	Image      string    `json:"image"`              // 相对 data/images 的路径
	PlateText  string    `json:"plate_text,omitempty"`
	PlateConf  float64   `json:"plate_conf,omitempty"`
	PersonID   string    `json:"person_id"`
	Sim        float64   `json:"sim,omitempty"`
	Gender     string    `json:"gender,omitempty"`    // male | female
	AgeRange   string    `json:"age_range,omitempty"` // 0-2 ... 70+
	Glasses    string    `json:"glasses,omitempty"`   // yes | no
	CreatedAt  time.Time `json:"created_at"`
}

// Label 把某个身份（人脸 person_id 或车牌号）绑定到一个人类可读的名字。
type Label struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`   // face | plate
	Target    string    `json:"target"` // person_id 或 车牌号
	Beep      bool      `json:"beep"`
	CreatedAt time.Time `json:"created_at"`
}

// Alert 是命中"需要鸣叫"的标签时产生的事件。
type Alert struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Label      string    `json:"label"`
	PersonID   string    `json:"person_id"`
	PlateText  string    `json:"plate_text,omitempty"`
	Image      string    `json:"image"`
	Address    string    `json:"address"`
	CapturedAt time.Time `json:"captured_at"`
	CreatedAt  time.Time `json:"created_at"`
}

type Store struct {
	cfg config.Config
	qd  *qdrant.Qdrant
	geo *geo.Geocoder

	mu       sync.RWMutex
	events   []*Event
	labels   []*Label
	alerts   []Alert
	cooldown map[string]time.Time

	eventsF *os.File
	alertsF *os.File
	labelsP string

	// 增量 reload：记录 events.jsonl 上次加载到的字节数 + mtime，
	// 跨进程 append 新行时，Web 进程的内存会自动追加。
	eventsPath   string
	eventsSize   int64
	labelsSize   int64
	labelsMtime  time.Time
}

func NewStore(cfg config.Config, qd *qdrant.Qdrant, geo *geo.Geocoder) (*Store, error) {
	evPath := filepath.Join(cfg.DataDir, "events.jsonl")
	s := &Store{
		cfg:        cfg,
		qd:         qd,
		geo:        geo,
		labelsP:    filepath.Join(cfg.DataDir, "labels.json"),
		cooldown:   map[string]time.Time{},
		eventsPath: evPath,
	}
	f, err := os.OpenFile(evPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	s.eventsF = f

	if b, err := os.ReadFile(evPath); err == nil {
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var e Event
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				log.Printf("[store] 跳过损坏记录: %v", err)
				continue
			}
			s.events = append(s.events, &e)
		}
		s.eventsSize = int64(len(b))
	}
	if st, err := os.Stat(s.labelsP); err == nil {
		s.labelsSize = st.Size()
		s.labelsMtime = st.ModTime()
		if b, err := os.ReadFile(s.labelsP); err == nil {
			json.Unmarshal(b, &s.labels)
		}
	}
	alertsPath := filepath.Join(cfg.DataDir, "alerts.jsonl")
	if b, err := os.ReadFile(alertsPath); err == nil {
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			var a Alert
			if json.Unmarshal([]byte(sc.Text()), &a) == nil && a.ID != "" {
				s.alerts = append(s.alerts, a)
			}
		}
	}
	af, err := os.OpenFile(alertsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	s.alertsF = af

	// 从历史事件恢复冷却表，避免重启后重复记录
	for _, e := range s.events {
		k := identityKey(e)
		if t, ok := s.cooldown[k]; !ok || e.CapturedAt.After(t) {
			s.cooldown[k] = e.CapturedAt
		}
	}
	log.Printf("[store] 载入 %d 条事件, %d 个标签", len(s.events), len(s.labels))
	return s, nil
}

func identityKey(e *Event) string {
	if e.Kind == "plate" {
		return "plate:" + e.PlateText
	}
	return "face:" + e.PersonID
}

// newID 生成 UUID v4 字符串。
func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// SaveImage 把前端裁剪好的人脸/车牌图片落盘，返回相对路径。
// personID 用于生成确定性文件名（同 personID + 同 day → 同文件名）。
// personID 不为空时，文件总被覆盖写入 → 保证"最后一次采集"替换前面所有版本。
// 不传 personID（空串）时回退到随机 UUID（仅兼容旧调用）。
func (s *Store) SaveImage(kind string, personID string, data []byte, contentType string, ts time.Time) (string, error) {
	ext := ".jpg"
	switch contentType {
	case "image/png":
		ext = ".png"
	case "image/webp":
		ext = ".webp"
	}
	dir := filepath.Join(fmt.Sprintf("%04d", ts.Year()), fmt.Sprintf("%02d", int(ts.Month())), fmt.Sprintf("%02d", ts.Day()))
	if err := os.MkdirAll(filepath.Join(s.cfg.DataDir, "images", dir), 0o755); err != nil {
		return "", err
	}
	var name string
	if personID != "" {
		// 确定性命名：同 personID 同天 → 同文件。后一次覆盖前一次 = 只保留最后一次采集。
		name = fmt.Sprintf("%s-%s-%04d%02d%02d%s", kind, personID, ts.Year(), ts.Month(), ts.Day(), ext)
	} else {
		name = fmt.Sprintf("%s-%s%s", kind, newID(), ext)
	}
	rel := filepath.Join(dir, name)
	full := filepath.Join(s.cfg.DataDir, "images", rel)
	// 无论 personID 模式还是 UUID 模式，总 WriteFile → 新图覆盖旧图。
	// personID 模式下的幂等 / 去重在更上层解决：API 层 cooldown、importfaces 的 existing personID 跳过。
	if err := os.WriteFile(full, data, 0o644); err != nil {
		return "", err
	}
	return rel, nil
}

// ResolveFacePersonID 给 512 维人脸向量解析 personID：
// Qdrant 里有匹配 → 返回已有的 personID；没有匹配 → 生成新 personID。
// API 层需要在 SaveImage 之前拿到 personID 才能用确定性文件名。
func (s *Store) ResolveFacePersonID(vec []float32) string {
	if hit, err := s.qd.SearchFace(vec, s.cfg.FaceThresh); err == nil && hit.ID != "" {
		if pid, ok := hit.Payload["person_id"].(string); ok && pid != "" {
			return pid
		}
	}
	return "face-" + newID()
}

// ResolvePlatePersonID 给车牌号解析 personID（车牌正则已在 normalizePlate 里校验）。
func ResolvePlatePersonID(plateText string) string {
	plateText = normalizePlate(plateText)
	if plateText == "" {
		return ""
	}
	return "plate:" + plateText
}

// Capture 处理一次采集：逆地理编码 -> 身份匹配 -> 冷却判断 -> 落盘/入库 -> 命中标签。
type CaptureInput struct {
	Kind       string
	CapturedAt time.Time
	Lat, Lon   *float64
	Altitude   *float64 // 海拔（米）
	Heading    *float64 // 相机方向角度（0=北, 90=东）
	Vector     []float32
	PlateText  string
	PlateConf  float64
	ImagePath  string
	PersonID   string // 可选：API 层预解析的 personID（确定性文件名依赖它）。空则 Capture 内部解析。
	Gender     string
	AgeRange   string
	Glasses    string
}

type CaptureResult struct {
	Event   *Event
	Skipped bool
	Labels  []*Label
}

func (s *Store) Capture(in CaptureInput) (*CaptureResult, error) {
	// Reverse 用原始 WGS-84 拿地址（国际服务要 WGS-84）
	addr := s.geo.Reverse(in.Lat, in.Lon)
	// 存进 Qdrant 的坐标转 GCJ-02（火星坐标系），国内地图显示无偏移
	var gcjLat, gcjLon *float64
	if in.Lat != nil && in.Lon != nil {
		g, l := geo.WGS84ToGCJ02(*in.Lon, *in.Lat)
		gcjLat, gcjLon = &g, &l
	}

	var personID string
	var sim float64

	switch in.Kind {
	case "face":
		if len(in.Vector) == 0 {
			return nil, errors.New("人脸特征向量为空")
		}
		if in.PersonID != "" {
			// API 层预解析了 personID（用于确定性文件名），跳过重复搜索
			personID = in.PersonID
		} else {
			if hit, err := s.qd.SearchFace(in.Vector, s.cfg.FaceThresh); err != nil {
				log.Printf("[qdrant] 人脸检索失败: %v", err)
			} else if hit.ID != "" {
				personID, _ = hit.Payload["person_id"].(string)
				sim = hit.Score
			}
			if personID == "" {
				personID = "face-" + newID()
			}
		}
	case "plate":
		in.PlateText = normalizePlate(in.PlateText)
		if in.PlateText == "" {
			return nil, errors.New("车牌号为空")
		}
		personID = "plate:" + in.PlateText
	default:
		return nil, errors.New("未知采集类型")
	}

	key := identityKey(&Event{Kind: in.Kind, PersonID: personID, PlateText: in.PlateText})
	s.mu.RLock()
	last, ok := s.cooldown[key]
	s.mu.RUnlock()
	if ok && in.CapturedAt.Sub(last) < s.cfg.Cooldown {
		return &CaptureResult{Skipped: true}, nil
	}

	e := &Event{
		ID:         newID(),
		Kind:       in.Kind,
		CapturedAt: in.CapturedAt,
		Lat:        gcjLat,
		Lon:        gcjLon,
		Address:    addr,
		Altitude:   in.Altitude,
		Heading:    in.Heading,
		Image:      in.ImagePath,
		PlateText:  in.PlateText,
		PlateConf:  in.PlateConf,
		PersonID:   personID,
		Sim:        sim,
		Gender:     in.Gender,
		AgeRange:   in.AgeRange,
		Glasses:    in.Glasses,
		CreatedAt:  time.Now(),
	}

	s.mu.Lock()
	s.events = append(s.events, e)
	s.cooldown[key] = in.CapturedAt
	s.mu.Unlock()

	if err := s.appendEvents(e); err != nil {
		log.Printf("[store] 写事件日志失败: %v", err)
	}

	payload := map[string]any{
		"kind":        e.Kind,
		"person_id":   e.PersonID,
		"captured_at": e.CapturedAt.Format(time.RFC3339),
		"address":     e.Address,
		"image":       e.Image,
		"lat":         e.Lat,
		"lon":         e.Lon,
	}
	if e.Altitude != nil {
		payload["altitude"] = *e.Altitude
	}
	if e.Heading != nil {
		payload["heading"] = *e.Heading
	}
	if e.Kind == "plate" {
		payload["plate"] = e.PlateText
		idx, vals := qdrant.PlateSparse(e.PlateText)
		if err := s.qd.UpsertPlate(e.ID, idx, vals, payload); err != nil {
			log.Printf("[qdrant] 车牌写入失败: %v", err)
		}
	} else {
		if e.Gender != "" {
			payload["gender"] = e.Gender
		}
		if e.AgeRange != "" {
			payload["age_range"] = e.AgeRange
		}
		if e.Glasses != "" {
			payload["glasses"] = e.Glasses
		}
		payload["updated_at"] = time.Now().Format(time.RFC3339)
		// 用 personID 做 point ID：同人重复采集 → 覆盖向量（避免一份人存多份 embedding 浪费 Qdrant 空间）
		// event log 里每次仍生成新的 e.ID（保留每次事件的时间/坐标），但 Qdrant 向量只保留最新的
		if err := s.qd.UpsertFace(e.PersonID, in.Vector, payload); err != nil {
			log.Printf("[qdrant] 人脸写入失败: %v", err)
		}
	}

	labels := s.LabelsFor(e)
	if len(labels) > 0 {
		var anyBeep bool
		var names []string
		for _, l := range labels {
			names = append(names, l.Name)
			anyBeep = anyBeep || l.Beep
		}
		if anyBeep {
			a := Alert{
				ID:         newID(),
				Kind:       e.Kind,
				Label:      strings.Join(names, "、"),
				PersonID:   e.PersonID,
				PlateText:  e.PlateText,
				Image:      e.Image,
				Address:    e.Address,
				CapturedAt: e.CapturedAt,
				CreatedAt:  time.Now(),
			}
			s.mu.Lock()
			s.alerts = append(s.alerts, a)
			s.mu.Unlock()
			if b, err := json.Marshal(a); err == nil {
				s.alertsF.Write(append(b, '\n'))
			}
		}
	}
	return &CaptureResult{Event: e, Labels: labels}, nil
}

func (s *Store) appendEvents(e *Event) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = s.eventsF.Write(append(b, '\n'))
	return err
}

// ImportFaceInput 批量导入单张人脸所需的字段。
type ImportFaceInput struct {
	PersonID   string
	Vector     []float32
	ImagePath  string    // 抠图相对路径
	SourcePath string    // 原始图片路径（EXIF 解析时用）
	CapturedAt time.Time // 事件时间（EXIF 创建时间优先，回退 time.Now）
	Lat        *float64  // WGS-84 纬度（来自 EXIF GPS）
	Lon        *float64  // WGS-84 经度
	Altitude   *float64  // 海拔（米）
	Heading    *float64  // 相机方向角度（0=北, 90=东）
}

// ImportFace 直接写入一条人脸事件 + upsert Qdrant，跳过同一性搜索和 cooldown。
// 用于批量导入场景（importfaces），导入时不需要搜索匹配，直接插入即可。
// 如果 in.Lat/in.Lon 非空，会做逆地理编码得到地址。
func (s *Store) ImportFace(in ImportFaceInput) (*Event, error) {
	var gcjLat, gcjLon *float64
	addr := ""
	if in.Lat != nil && in.Lon != nil {
		addr = s.geo.Reverse(in.Lat, in.Lon)
		g, l := geo.WGS84ToGCJ02(*in.Lon, *in.Lat)
		gcjLat, gcjLon = &g, &l
	}

	if in.CapturedAt.IsZero() {
		in.CapturedAt = time.Now()
	}

	e := &Event{
		ID:         newID(),
		Kind:       "face",
		CapturedAt: in.CapturedAt,
		Lat:        gcjLat,
		Lon:        gcjLon,
		Address:    addr,
		Altitude:   in.Altitude,
		Heading:    in.Heading,
		Image:      in.ImagePath,
		PersonID:   in.PersonID,
		CreatedAt:  time.Now(),
	}
	// ✅ 先 upsert Qdrant 向量：失败 → 事件不落盘，调用方也不会存抠图，无脏数据
	// 🔴 旧顺序（先事件后向量）在 Qdrant 短暂不可用时会产生"有事件无图片"的坏数据
	payload := map[string]any{
		"kind":        e.Kind,
		"person_id":   e.PersonID,
		"captured_at": e.CapturedAt.Format(time.RFC3339),
		"image":       e.Image,
		"source_path": in.SourcePath,
	}
	if addr != "" {
		payload["address"] = addr
	}
	if e.Lat != nil {
		payload["lat"] = e.Lat
		payload["lon"] = e.Lon
	}
	if e.Altitude != nil {
		payload["altitude"] = *e.Altitude
	}
	if e.Heading != nil {
		payload["heading"] = *e.Heading
	}
	if err := s.qd.UpsertFace(e.PersonID, in.Vector, payload); err != nil {
		return nil, fmt.Errorf("upsert face: %w", err)
	}

	// ✅ 向量已入 Qdrant，事件才落盘（事件是 identities 展示依据，必须保证有图可指）
	s.mu.Lock()
	s.events = append(s.events, e)
	s.mu.Unlock()
	if err := s.appendEvents(e); err != nil {
		return nil, fmt.Errorf("写事件日志: %w", err)
	}
	return e, nil
}

// ExistingPersonIDs 返回所有 person_id 以 prefix 开头的事件集合。
// 用于 importfaces 幂等判断：已导入过的人脸跳过不重复导入。
func (s *Store) ExistingPersonIDs(prefix string) map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]bool{}
	for _, e := range s.events {
		if strings.HasPrefix(e.PersonID, prefix) {
			out[e.PersonID] = true
		}
	}
	return out
}

// DeleteEvents 批量删除事件：从内存、events.jsonl、本地图片、Qdrant 中移除。
// 返回删除的事件数量和错误（如果有）。
func (s *Store) DeleteEvents(ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	idSet := map[string]bool{}
	for _, id := range ids {
		idSet[id] = true
	}

	s.mu.Lock()
	// 找出要删除的事件（收集图片路径用于后续删除文件）
	var toDelete []*Event
	var kept []*Event
	for _, e := range s.events {
		if idSet[e.ID] {
			toDelete = append(toDelete, e)
		} else {
			kept = append(kept, e)
		}
	}
	s.events = kept
	s.mu.Unlock()

	if len(toDelete) == 0 {
		return 0, nil
	}

	// 重写 events.jsonl
	if err := s.rewriteEventsFile(); err != nil {
		log.Printf("[store] 重写 events.jsonl 失败: %v", err)
	}

	// 删除本地图片文件
	for _, e := range toDelete {
		if e.Image != "" {
			imgPath := filepath.Join(s.cfg.DataDir, "images", e.Image)
			if err := os.Remove(imgPath); err != nil && !os.IsNotExist(err) {
				log.Printf("[store] 删除图片失败 %s: %v", imgPath, err)
			}
		}
	}

	// 从 Qdrant 删除 point（按 kind 分 collection）
	faceIDs, plateIDs := []string{}, []string{}
	for _, e := range toDelete {
		if e.Kind == "plate" {
			plateIDs = append(plateIDs, e.ID)
		} else {
			faceIDs = append(faceIDs, e.ID)
		}
	}
	if len(faceIDs) > 0 {
		if err := s.qd.DeletePoints("vi_faces", faceIDs); err != nil {
			log.Printf("[qdrant] 删除人脸 point 失败: %v", err)
		}
	}
	if len(plateIDs) > 0 {
		if err := s.qd.DeletePoints("vi_plates", plateIDs); err != nil {
			log.Printf("[qdrant] 删除车牌 point 失败: %v", err)
		}
	}

	return len(toDelete), nil
}

// rewriteEventsFile 把当前内存中的事件重写回 events.jsonl（覆盖写）。
func (s *Store) rewriteEventsFile() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	path := filepath.Join(s.cfg.DataDir, "events.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, e := range s.events {
		b, err := json.Marshal(e)
		if err != nil {
			continue
		}
		f.Write(append(b, '\n'))
	}
	return nil
}

// 中国车牌号正则：覆盖蓝牌/黄牌（7位）、新能源（8位）、港澳/领馆/使馆/警/挂/学/使领 等特殊后缀。
// 省份简称：京津沪渝冀豫云辽黑湘皖鲁新苏浙赣鄂桂甘晋蒙陕吉闽贵粤青藏川宁琼使领
// 第二位字母：A-HJ-NP-Z（排除 I、O）。特殊使/领 可多一位。
// 后半段：普通 5 位；新能源 D/F + 5位 或 5位 + D/F；特殊后缀挂学警港澳使领
var plateRe = regexp.MustCompile(`^(京|津|沪|渝|冀|豫|云|辽|黑|湘|皖|鲁|新|苏|浙|赣|鄂|桂|甘|晋|蒙|陕|吉|闽|贵|粤|青|藏|川|宁|琼|使|领)[A-HJ-NP-Z](([0-9A-HJ-NP-Z]{5}[DF])|([DF][0-9A-HJ-NP-Z]{5})|([0-9A-HJ-NP-Z]{4}[挂学警港澳使领])|([0-9A-HJ-NP-Z]{5}))$`)

// normalizePlate 规整车牌号并校验格式；非法返回空串。
// 规整规则：去空格/·/-、大写、去前后缀 "粤·A12345" → "粤A12345"。
func normalizePlate(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "·", "")
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "-", "")
	if !plateRe.MatchString(s) {
		return ""
	}
	return s
}

// NormalizePlate 导出版。
func NormalizePlate(s string) string { return normalizePlate(s) }

// LabelsFor 返回某次采集命中的标签（按身份查标签表）。
func (s *Store) LabelsFor(e *Event) []*Label {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Label
	for _, l := range s.labels {
		if l.Kind != e.Kind {
			continue
		}
		if (e.Kind == "face" && l.Target == e.PersonID) ||
			(e.Kind == "plate" && l.Target == e.PlateText) {
			out = append(out, l)
		}
	}
	return out
}

// Labels 返回当前全部标签快照。
func (s *Store) Labels() []*Label {
	s.reloadLabelsIfStale()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Label, len(s.labels))
	copy(out, s.labels)
	return out
}

// Reverse 把 WGS-84 经纬度做逆地理编码（带本地缓存）。
func (s *Store) Reverse(lat, lon *float64) string {
	if s == nil || s.geo == nil || lat == nil || lon == nil {
		return ""
	}
	return s.geo.Reverse(lat, lon)
}

// QDEnsure 触发 Qdrant Ensure（健康检查用）。
func (s *Store) QDEnsure() error {
	if s.qd == nil {
		return nil
	}
	return s.qd.Ensure()
}

func (s *Store) saveLabels() error {
	b, _ := json.MarshalIndent(s.labels, "", "  ")
	return os.WriteFile(s.labelsP, b, 0o644)
}

// AddLabel 为一次采集所属的身份打标签；重复的 名字+身份 会复用原标签。
func (s *Store) AddLabel(name, eventID string, beep bool) (*Label, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("标签名不能为空")
	}
	s.mu.Lock()
	var target *Event
	for i := len(s.events) - 1; i >= 0; i-- {
		if s.events[i].ID == eventID {
			target = s.events[i]
			break
		}
	}
	if target == nil {
		s.mu.Unlock()
		return nil, errors.New("采集记录不存在")
	}
	t := target.PersonID
	if target.Kind == "plate" {
		t = target.PlateText
	}
	return s.addLabelLocked(name, target.Kind, t, beep)
}

// AddLabelForPersonID 直接给 face 身份（personID）打标签，无需 event。
// 用于主界面"输入名字"自动打标签：即使本次采集被 cooldown 跳过也能把名字挂上。
func (s *Store) AddLabelForPersonID(name, personID string, beep bool) (*Label, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("标签名不能为空")
	}
	if personID == "" {
		return nil, errors.New("personID 为空")
	}
	s.mu.Lock()
	return s.addLabelLocked(name, "face", personID, beep)
}

// addLabelLocked 在已持有 s.mu 的前提下添加或复用同名同目标标签，返回前解锁并落盘。
func (s *Store) addLabelLocked(name, kind, target string, beep bool) (*Label, error) {
	for _, l := range s.labels {
		if l.Name == name && l.Kind == kind && l.Target == target {
			l.Beep = l.Beep || beep
			s.mu.Unlock()
			s.saveLabels()
			return l, nil
		}
	}
	l := &Label{
		ID:        newID(),
		Name:      name,
		Kind:      kind,
		Target:    target,
		Beep:      beep,
		CreatedAt: time.Now(),
	}
	s.labels = append(s.labels, l)
	s.mu.Unlock()
	if err := s.saveLabels(); err != nil {
		return nil, err
	}
	return l, nil
}

func (s *Store) SetBeep(id string, beep bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.labels {
		if l.ID == id {
			l.Beep = beep
			return s.saveLabels()
		}
	}
	return errors.New("标签不存在")
}

func (s *Store) DeleteLabel(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, l := range s.labels {
		if l.ID == id {
			s.labels = append(s.labels[:i], s.labels[i+1:]...)
			return s.saveLabels()
		}
	}
	return errors.New("标签不存在")
}

// Events 返回按时间倒序的全部事件快照。
func (s *Store) Events() []*Event {
	s.reloadEventsIfStale()
	s.reloadLabelsIfStale()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Event, len(s.events))
	for i, e := range s.events {
		out[len(s.events)-1-i] = e
	}
	return out
}

// reloadEventsIfStale 增量读取 events.jsonl 中自上次加载以来追加的新行。
// 场景：importfaces 子进程 append 新事件后，Web 进程的内存自动更新。
func (s *Store) reloadEventsIfStale() {
	st, err := os.Stat(s.eventsPath)
	if err != nil {
		return
	}
	currentSize := st.Size()

	s.mu.RLock()
	knownSize := s.eventsSize
	s.mu.RUnlock()

	if currentSize <= knownSize {
		return // 无新数据（文件被截断这种极端情况这里不管）
	}

	f, err := os.Open(s.eventsPath)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(knownSize, io.SeekStart); err != nil {
		return
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	s.mu.Lock()
	defer s.mu.Unlock()
	added := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		s.events = append(s.events, &e)
		k := identityKey(&e)
		if t, ok := s.cooldown[k]; !ok || e.CapturedAt.After(t) {
			s.cooldown[k] = e.CapturedAt
		}
		added++
	}
	s.eventsSize = currentSize
	if added > 0 {
		log.Printf("[store] 增量加载 %d 条新事件 (events.jsonl %d→%d bytes)", added, knownSize, currentSize)
	}
}

// reloadLabelsIfStale 检查 labels.json 是否被其他进程改过（mtime/size 任一变化），
// 变化则整文件重读覆盖内存中的 s.labels。
func (s *Store) reloadLabelsIfStale() {
	st, err := os.Stat(s.labelsP)
	if err != nil {
		return
	}
	s.mu.RLock()
	knownSize := s.labelsSize
	knownMtime := s.labelsMtime
	s.mu.RUnlock()

	if st.Size() == knownSize && st.ModTime().Equal(knownMtime) {
		return // 没变
	}

	b, err := os.ReadFile(s.labelsP)
	if err != nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var newLabels []*Label
	if err := json.Unmarshal(b, &newLabels); err == nil {
		s.labels = newLabels
	}
	s.labelsSize = st.Size()
	s.labelsMtime = st.ModTime()
	log.Printf("[store] 重新加载 labels.json (%d 条)", len(s.labels))
}

func (s *Store) AlertsSince(since time.Time, limit int) []Alert {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Alert
	for i := len(s.alerts) - 1; i >= 0; i-- {
		if len(out) >= limit {
			break
		}
		if s.alerts[i].CreatedAt.After(since) {
			out = append(out, s.alerts[i])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Reset 清空事件/标签/告警、重置 Qdrant 集合、删除采集图片目录。
func (s *Store) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.events = nil
	s.labels = nil
	s.alerts = nil
	s.cooldown = map[string]time.Time{}

	if err := s.eventsF.Truncate(0); err != nil {
		return fmt.Errorf("truncate events: %w", err)
	}
	if _, err := s.eventsF.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek events: %w", err)
	}
	if err := s.eventsF.Sync(); err != nil {
		return fmt.Errorf("sync events: %w", err)
	}

	if err := s.alertsF.Truncate(0); err != nil {
		return fmt.Errorf("truncate alerts: %w", err)
	}
	if _, err := s.alertsF.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek alerts: %w", err)
	}

	if err := os.WriteFile(s.labelsP, []byte("[]\n"), 0o644); err != nil {
		return fmt.Errorf("write labels: %w", err)
	}

	imgDir := filepath.Join(s.cfg.DataDir, "images")
	os.RemoveAll(imgDir)
	os.MkdirAll(imgDir, 0o755)
	rptDir := filepath.Join(s.cfg.DataDir, "reports")
	os.RemoveAll(rptDir)
	os.MkdirAll(rptDir, 0o755)

	if s.qd != nil {
		if err := s.qd.Recreate(); err != nil {
			log.Printf("[store] reset qdrant 失败（可忽略）: %v", err)
		}
	}

	return nil
}
