package api

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"vi/internal/auth"
	"vi/internal/config"
	"vi/internal/report"
	"vi/internal/store"
)

// Server 是所有 HTTP 路由的入口。
type Server struct {
	cfg config.Config
	st  *store.Store
	mux *http.ServeMux
}

// NewServer 组装所有路由并返回一个 http.Handler。
func NewServer(cfg config.Config, st *store.Store) http.Handler {
	s := &Server{cfg: cfg, st: st, mux: http.NewServeMux()}

	// —— 公开接口（免登录）——
	s.mux.HandleFunc("POST /api/login", s.handleLogin)
	s.mux.HandleFunc("POST /api/logout", s.handleLogout)
	s.mux.HandleFunc("GET  /api/me", s.handleMe)
	s.mux.HandleFunc("POST /api/password", s.handleChangePassword)
	s.mux.HandleFunc("GET /api/health", s.handleHealth)
	// /api/capture 保持公开：实时采集是核心功能，首页免登录也能上传
	s.mux.HandleFunc("POST /api/capture", s.handleCapture)

	// —— 需登录 ——
	s.mux.HandleFunc("GET  /api/media-url", auth.RequireAuth(s.handleMediaSignedURL))

	s.mux.HandleFunc("GET /api/labels", auth.RequireAuth(s.handleLabels))
	s.mux.HandleFunc("POST /api/labels", auth.RequireAuth(s.handleAddLabel))
	s.mux.HandleFunc("PATCH /api/labels/{id}", auth.RequireAuth(s.handlePatchLabel))
	s.mux.HandleFunc("DELETE /api/labels/{id}", auth.RequireAuth(s.handleDeleteLabel))
	s.mux.HandleFunc("GET /api/identities", auth.RequireAuth(s.handleIdentities))

	s.mux.HandleFunc("GET /api/search/date", auth.RequireAuth(s.handleSearchDate))
	s.mux.HandleFunc("GET /api/search/plate", auth.RequireAuth(s.handleSearchPlate))
	s.mux.HandleFunc("GET /api/search/label", auth.RequireAuth(s.handleSearchLabel))
	s.mux.HandleFunc("GET /api/search/place", auth.RequireAuth(s.handleSearchPlace))

	s.mux.HandleFunc("GET /api/stats", auth.RequireAuth(s.handleStats))
	s.mux.HandleFunc("GET /api/alerts", auth.RequireAuth(s.handleAlerts))
	s.mux.HandleFunc("POST /api/summary/run", auth.RequireAuth(s.handleSummaryRun))
	s.mux.HandleFunc("GET /api/summary/list", auth.RequireAuth(s.handleSummaryList))

	s.mux.HandleFunc("POST /api/admin/reset", auth.RequireAuth(s.handleAdminReset))

	web := http.FileServer(http.Dir(cfg.WebDir))
	vendor := http.FileServer(http.Dir(filepath.Join(cfg.WebDir, "vendor")))
	media := http.FileServer(http.Dir(filepath.Join(cfg.DataDir, "images")))
	models := http.FileServer(http.Dir(cfg.ModelsDir))
	reports := http.FileServer(http.Dir(filepath.Join(cfg.DataDir, "reports")))
	s.mux.Handle("/media/", wrapCache(auth.RequireMediaAuth(http.StripPrefix("/media/", media)), "public, max-age=3600"))
	s.mux.Handle("/models/", wrapCache(http.StripPrefix("/models/", models), "public, max-age=31536000, immutable"))
	s.mux.Handle("/vendor/", wrapCache(http.StripPrefix("/vendor/", vendor), "public, max-age=31536000, immutable"))
	s.mux.Handle("/reports/", http.StripPrefix("/reports/", reports))
	s.mux.Handle("/", wrapCache(web, "public, max-age=60, must-revalidate"))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Embedder-Policy", "require-corp")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		s.mux.ServeHTTP(w, r)
	})
}

// wrapCache 给子路由统一设置 Cache-Control。
func wrapCache(h http.Handler, policy string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", policy)
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, format string, args ...any) {
	writeJSON(w, code, map[string]string{"error": fmt.Sprintf(format, args...)})
}

// —— auth handlers（方法必须在 api 包里，因为 receiver 是 *Server）——

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	token, u, err := auth.Login(body.Username, body.Password)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":    token,
		"username": u.Username,
		"expires":  time.Now().Add(auth.TokenTTL).Unix(),
	})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	token := auth.TokenFromHeader(r)
	username, ok := auth.Authenticate(token)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未登录"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"username": username,
		"expires":  auth.TokenExpiry(token).Unix(),
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	auth.RevokeToken(auth.TokenFromHeader(r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	token := auth.TokenFromHeader(r)
	if err := auth.ChangePassword(s.cfg.DataDir, token, body.OldPassword, body.NewPassword); err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	// 改密后吊销当前 token，强制重新登录
	auth.RevokeToken(token)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleMediaSignedURL GET /api/media-url?path=xxx
func (s *Server) handleMediaSignedURL(w http.ResponseWriter, r *http.Request) {
	if _, ok := auth.Authenticate(auth.TokenFromHeader(r)); !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "需要登录"})
		return
	}
	relPath := r.URL.Query().Get("path")
	if relPath == "" {
		writeErr(w, http.StatusBadRequest, "缺少 path 参数")
		return
	}
	if strings.Contains(relPath, "..") || strings.HasPrefix(relPath, "/") {
		writeErr(w, http.StatusBadRequest, "非法路径")
		return
	}
	signed := auth.BuildSignedMediaURL(relPath)
	writeJSON(w, http.StatusOK, map[string]any{
		"url":     signed,
		"expires": time.Now().Add(auth.MediaURLTTL).Unix(),
		"path":    relPath,
	})
}

// —— 业务 handlers ——

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	qdOK := true
	if err := s.st.QDEnsure(); err != nil {
		qdOK = false
	}
	models := map[string]bool{}
	for _, f := range []string{"face_landmarker.task", "arc.onnx", "face_age.onnx", "face_gender.onnx", "face_glasses.onnx", "plate_det.onnx", "plate_rec.onnx", "chars.json"} {
		_, err := os.Stat(filepath.Join(s.cfg.ModelsDir, f))
		models[f] = err == nil
	}
	writeJSON(w, 200, map[string]any{
		"ok":               true,
		"qdrant":           qdOK,
		"models":           models,
		"face_threshold":   s.cfg.FaceThresh,
		"cooldown_seconds": int(s.cfg.Cooldown.Seconds()),
		"events":           len(s.st.Events()),
		"labels":           len(s.st.Labels()),
	})
}

type labelView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Target      string `json:"target"`
	Beep        bool   `json:"beep"`
	Count       int    `json:"count"`
	LastSeen    string `json:"last_seen,omitempty"`
	SampleImage string `json:"sample_image,omitempty"`
}

type eventView struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	CapturedAt string   `json:"captured_at"`
	Lat        *float64 `json:"lat,omitempty"`
	Lon        *float64 `json:"lon,omitempty"`
	Address    string   `json:"address"`
	Image      string   `json:"image"`
	PlateText  string   `json:"plate_text,omitempty"`
	PlateConf  float64  `json:"plate_conf,omitempty"`
	PersonID   string   `json:"person_id"`
	Sim        float64  `json:"sim,omitempty"`
	Gender     string   `json:"gender,omitempty"`
	AgeRange   string   `json:"age_range,omitempty"`
	Glasses    string   `json:"glasses,omitempty"`
	Labels     []string `json:"labels"`
}

func (s *Server) view(e *store.Event) eventView {
	v := eventView{
		ID:         e.ID,
		Kind:       e.Kind,
		CapturedAt: e.CapturedAt.In(s.cfg.Location).Format("2006-01-02 15:04:05"),
		Lat:        e.Lat,
		Lon:        e.Lon,
		Address:    e.Address,
		PlateText:  e.PlateText,
		PlateConf:  e.PlateConf,
		PersonID:   e.PersonID,
		Sim:        e.Sim,
		Gender:     e.Gender,
		AgeRange:   e.AgeRange,
		Glasses:    e.Glasses,
		Labels:     []string{},
	}
	if e.Image != "" {
		v.Image = "/media/" + strings.ReplaceAll(e.Image, string(filepath.Separator), "/")
	}
	for _, l := range s.st.LabelsFor(e) {
		v.Labels = append(v.Labels, l.Name)
	}
	return v
}

func (s *Server) labelCounts() (map[string]int, map[string]*store.Event) {
	counts := map[string]int{}
	last := map[string]*store.Event{}
	for _, e := range s.st.Events() {
		for _, l := range s.st.LabelsFor(e) {
			counts[l.ID]++
			if _, ok := last[l.ID]; !ok {
				last[l.ID] = e
			}
		}
	}
	return counts, last
}

func (s *Server) handleLabels(w http.ResponseWriter, r *http.Request) {
	counts, last := s.labelCounts()
	out := []labelView{}
	for _, l := range s.st.Labels() {
		v := labelView{ID: l.ID, Name: l.Name, Kind: l.Kind, Target: l.Target, Beep: l.Beep, Count: counts[l.ID]}
		if e := last[l.ID]; e != nil {
			v.LastSeen = e.CapturedAt.In(s.cfg.Location).Format("2006-01-02 15:04:05")
			v.SampleImage = "/media/" + strings.ReplaceAll(e.Image, string(filepath.Separator), "/")
		}
		out = append(out, v)
	}
	writeJSON(w, 200, map[string]any{"items": out})
}

func (s *Server) handleAddLabel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string `json:"name"`
		EventID string `json:"event_id"`
		Beep    *bool  `json:"beep"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, 400, "参数解析失败")
		return
	}
	beep := true
	if req.Beep != nil {
		beep = *req.Beep
	}
	l, err := s.st.AddLabel(req.Name, req.EventID, beep)
	if err != nil {
		writeErr(w, 400, "%v", err)
		return
	}
	writeJSON(w, 200, map[string]any{"label": l})
}

func (s *Server) handlePatchLabel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Beep *bool `json:"beep"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.Beep == nil {
		writeErr(w, 400, "参数解析失败")
		return
	}
	if err := s.st.SetBeep(id, *req.Beep); err != nil {
		writeErr(w, 404, "%v", err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleDeleteLabel(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteLabel(r.PathValue("id")); err != nil {
		writeErr(w, 404, "%v", err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleCapture(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
	if err := r.ParseMultipartForm(12 << 20); err != nil {
		writeErr(w, 400, "表单解析失败: %v", err)
		return
	}
	kind := r.FormValue("kind")
	if kind != "face" && kind != "plate" {
		writeErr(w, 400, "kind 必须是 face 或 plate")
		return
	}
	ts := parseTime(r.FormValue("captured_at"), s.cfg.Location)
	if ts.IsZero() {
		ts = time.Now().In(s.cfg.Location)
	}

	lat, lon := parseLatLon(r.FormValue("lat"), r.FormValue("lon"))

	var vec []float32
	if v := r.FormValue("vector"); v != "" {
		if err := json.Unmarshal([]byte(v), &vec); err != nil {
			writeErr(w, 400, "特征向量格式错误")
			return
		}
	}
	if kind == "face" && len(vec) != 512 {
		writeErr(w, 400, "人脸特征维度应为 512，实际 %d", len(vec))
		return
	}
	plate := r.FormValue("plate_text")
	plateConf, _ := strconv.ParseFloat(r.FormValue("plate_conf"), 64)
	gender := normAttr(r.FormValue("gender"), "male", "female")
	ageRange := normAttr(r.FormValue("age_range"), "0-2", "3-9", "10-19", "20-29", "30-39", "40-49", "50-59", "60-69", "70+")
	glasses := normAttr(r.FormValue("glasses"), "yes", "no")

	file, _, err := r.FormFile("image")
	if err != nil {
		writeErr(w, 400, "缺少图片")
		return
	}
	defer file.Close()
	imgBytes, err := io.ReadAll(io.LimitReader(file, 8<<20))
	if err != nil || len(imgBytes) == 0 {
		writeErr(w, 400, "图片读取失败")
		return
	}
	ct := http.DetectContentType(imgBytes)
	if !strings.HasPrefix(ct, "image/") {
		writeErr(w, 400, "上传内容不是图片")
		return
	}

	rel, err := s.st.SaveImage(kind, imgBytes, ct, ts)
	if err != nil {
		writeErr(w, 500, "图片保存失败: %v", err)
		return
	}
	res, err := s.st.Capture(store.CaptureInput{
		Kind:       kind,
		CapturedAt: ts,
		Lat:        lat,
		Lon:        lon,
		Vector:     vec,
		PlateText:  plate,
		PlateConf:  plateConf,
		ImagePath:  rel,
		Gender:     gender,
		AgeRange:   ageRange,
		Glasses:    glasses,
	})
	if err != nil {
		os.Remove(filepath.Join(s.cfg.DataDir, "images", rel))
		writeErr(w, 400, "%v", err)
		return
	}
	if res.Skipped {
		os.Remove(filepath.Join(s.cfg.DataDir, "images", rel))
		writeJSON(w, 200, map[string]any{"ok": true, "skipped": true})
		return
	}

	names := []string{}
	beep := false
	for _, l := range res.Labels {
		names = append(names, l.Name)
		beep = beep || l.Beep
	}
	writeJSON(w, 200, map[string]any{
		"ok":         true,
		"skipped":    false,
		"address":    res.Event.Address,
		"event":      s.view(res.Event),
		"labels":     names,
		"alert":      beep,
		"alert_text": strings.Join(names, "、"),
	})
}

func parseTime(v string, loc *time.Location) time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02 15:04"} {
		var t time.Time
		var err error
		if layout == time.RFC3339 {
			t, err = time.Parse(layout, v)
		} else {
			t, err = time.ParseInLocation(layout, v, loc)
		}
		if err == nil {
			return t.In(loc)
		}
	}
	return time.Time{}
}

func parseLatLon(la, lo string) (*float64, *float64) {
	if la == "" || lo == "" {
		return nil, nil
	}
	a, err1 := strconv.ParseFloat(la, 64)
	b, err2 := strconv.ParseFloat(lo, 64)
	if err1 != nil || err2 != nil || math.IsNaN(a) || math.IsNaN(b) || math.Abs(a) > 90 || math.Abs(b) > 180 {
		return nil, nil
	}
	return &a, &b
}

func normAttr(v string, allowed ...string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	return ""
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, filter func(*store.Event) bool) {
	page := 1
	if p := r.URL.Query().Get("page"); p != "" {
		page, _ = strconv.Atoi(p)
	}
	if page < 1 {
		page = 1
	}
	const size = 20
	var matched []*store.Event
	for _, e := range s.st.Events() {
		if filter(e) {
			matched = append(matched, e)
		}
	}
	start := (page - 1) * size
	end := start + size
	if start > len(matched) {
		start = len(matched)
	}
	if end > len(matched) {
		end = len(matched)
	}
	items := []eventView{}
	for _, e := range matched[start:end] {
		items = append(items, s.view(e))
	}
	writeJSON(w, 200, map[string]any{
		"page":      page,
		"page_size": size,
		"total":     len(matched),
		"has_more":  end < len(matched),
		"items":     items,
	})
}

func (s *Server) handleSearchDate(w http.ResponseWriter, r *http.Request) {
	day := r.URL.Query().Get("date")
	t, err := time.ParseInLocation("2006-01-02", day, s.cfg.Location)
	if err != nil {
		writeErr(w, 400, "日期格式应为 YYYY-MM-DD")
		return
	}
	next := t.AddDate(0, 0, 1)
	s.list(w, r, func(e *store.Event) bool {
		ts := e.CapturedAt.In(s.cfg.Location)
		return !ts.Before(t) && ts.Before(next)
	})
}

func (s *Server) handleSearchPlate(w http.ResponseWriter, r *http.Request) {
	q := store.NormalizePlate(r.URL.Query().Get("plate"))
	if q == "" {
		writeErr(w, 400, "缺少车牌号")
		return
	}
	s.list(w, r, func(e *store.Event) bool {
		return e.Kind == "plate" && strings.Contains(e.PlateText, q)
	})
}

func (s *Server) handleSearchLabel(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("name"))
	if q == "" {
		writeErr(w, 400, "缺少标签名")
		return
	}
	targets := map[string]bool{}
	for _, l := range s.st.Labels() {
		if strings.Contains(l.Name, q) {
			targets[l.Kind+"|"+l.Target] = true
		}
	}
	if len(targets) == 0 {
		s.list(w, r, func(e *store.Event) bool { return false })
		return
	}
	s.list(w, r, func(e *store.Event) bool {
		if e.Kind == "face" {
			return targets["face|"+e.PersonID]
		}
		return targets["plate|"+e.PlateText]
	})
}

func (s *Server) handleSearchPlace(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if q == "" {
		writeErr(w, 400, "缺少地名/街道")
		return
	}
	s.list(w, r, func(e *store.Event) bool {
		return strings.Contains(strings.ToLower(e.Address), q)
	})
}

type identity struct {
	Key      string
	Last     *store.Event
	Count    int
	First    time.Time
	LabelIDs []string
	Labels   []string
	Beeps    bool
}

func (s *Server) handleIdentities(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	if kind != "face" && kind != "plate" {
		writeErr(w, 400, "kind 必须是 face 或 plate")
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	page := 1
	if p := r.URL.Query().Get("page"); p != "" {
		page, _ = strconv.Atoi(p)
	}
	if page < 1 {
		page = 1
	}
	const size = 20

	groups := map[string]*identity{}
	for _, e := range s.st.Events() {
		if e.Kind != kind {
			continue
		}
		key := e.PersonID
		if kind == "plate" {
			key = e.PlateText
		}
		g, ok := groups[key]
		if !ok {
			g = &identity{Key: key, Last: e, Count: 0, First: e.CapturedAt, Labels: []string{}}
			groups[key] = g
		}
		g.Count++
		if e.CapturedAt.Before(g.First) {
			g.First = e.CapturedAt
		}
		if e.CapturedAt.After(g.Last.CapturedAt) {
			g.Last = e
		}
		for _, l := range s.st.LabelsFor(e) {
			if !contains(g.LabelIDs, l.ID) {
				g.LabelIDs = append(g.LabelIDs, l.ID)
				g.Labels = append(g.Labels, l.Name)
				g.Beeps = g.Beeps || l.Beep
			}
		}
	}
	var arr []*identity
	for _, g := range groups {
		if q != "" {
			hay := strings.ToLower(g.Key + " " + strings.Join(g.Labels, " "))
			if !strings.Contains(hay, q) {
				continue
			}
		}
		arr = append(arr, g)
	}
	sort.Slice(arr, func(i, j int) bool { return arr[i].Last.CapturedAt.After(arr[j].Last.CapturedAt) })

	start := (page - 1) * size
	if start > len(arr) {
		start = len(arr)
	}
	end := start + size
	if end > len(arr) {
		end = len(arr)
	}
	items := []map[string]any{}
	for _, g := range arr[start:end] {
		it := map[string]any{
			"key":        g.Key,
			"count":      g.Count,
			"first":      g.First.In(s.cfg.Location).Format("2006-01-02 15:04:05"),
			"last":       g.Last.CapturedAt.In(s.cfg.Location).Format("2006-01-02 15:04:05"),
			"image":      "/media/" + strings.ReplaceAll(g.Last.Image, string(filepath.Separator), "/"),
			"event_id":   g.Last.ID,
			"address":    g.Last.Address,
			"labels":     g.Labels,
			"beeps":      g.Beeps,
			"plate_text": g.Last.PlateText,
			"gender":     g.Last.Gender,
			"age_range":  g.Last.AgeRange,
			"glasses":    g.Last.Glasses,
		}
		items = append(items, it)
	}
	writeJSON(w, 200, map[string]any{"page": page, "page_size": size, "total": len(arr), "items": items})
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	loc := s.cfg.Location
	from := strings.TrimSpace(r.URL.Query().Get("from"))
	to := strings.TrimSpace(r.URL.Query().Get("to"))
	start, err1 := time.ParseInLocation("2006-01-02", from, loc)
	end, err2 := time.ParseInLocation("2006-01-02", to, loc)
	if err1 != nil || err2 != nil {
		writeErr(w, 400, "from/to 格式应为 YYYY-MM-DD")
		return
	}
	if end.Before(start) {
		start, end = end, start
	}
	end = end.AddDate(0, 0, 1)

	type dayStat struct {
		Date   string `json:"date"`
		Faces  int    `json:"faces"`
		Plates int    `json:"plates"`
	}
	days := map[string]*dayStat{}
	faces, plates := 0, 0
	persons := map[string]bool{}
	uniquePlates := map[string]bool{}
	labeled := 0
	for _, e := range s.st.Events() {
		ts := e.CapturedAt.In(loc)
		if ts.Before(start) || !ts.Before(end) {
			continue
		}
		d := ts.Format("2006-01-02")
		ds := days[d]
		if ds == nil {
			ds = &dayStat{Date: d}
			days[d] = ds
		}
		if e.Kind == "face" {
			faces++
			ds.Faces++
			persons[e.PersonID] = true
		} else {
			plates++
			ds.Plates++
			uniquePlates[e.PlateText] = true
		}
		if len(s.st.LabelsFor(e)) > 0 {
			labeled++
		}
	}
	var daily []dayStat
	for d := start; d.Before(end); d = d.AddDate(0, 0, 1) {
		k := d.Format("2006-01-02")
		if ds, ok := days[k]; ok {
			daily = append(daily, *ds)
		} else {
			daily = append(daily, dayStat{Date: k})
		}
	}
	writeJSON(w, 200, map[string]any{
		"from":           start.Format("2006-01-02"),
		"to":             end.AddDate(0, 0, -1).Format("2006-01-02"),
		"faces":          faces,
		"plates":         plates,
		"persons":        len(persons),
		"unique_plates":  len(uniquePlates),
		"labeled_events": labeled,
		"daily":          daily,
	})
}

func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	since := time.Time{}
	if v := r.URL.Query().Get("since"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			since = t
		}
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	writeJSON(w, 200, map[string]any{"items": s.st.AlertsSince(since, limit)})
}

func (s *Server) handleAdminReset(w http.ResponseWriter, r *http.Request) {
	if err := s.st.Reset(); err != nil {
		writeErr(w, 500, "清库失败: %v", err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "msg": "已清空所有事件/标签/告警与向量点"})
}

func (s *Server) handleSummaryRun(w http.ResponseWriter, r *http.Request) {
	loc := s.cfg.Location
	day := r.URL.Query().Get("date")
	if day == "" {
		day = time.Now().In(loc).Format("2006-01-02")
	}
	t, err := time.ParseInLocation("2006-01-02", day, loc)
	if err != nil {
		writeErr(w, 400, "日期格式应为 YYYY-MM-DD")
		return
	}
	p, err := report.WriteSummary(s.cfg, s.st, t)
	if err != nil {
		writeErr(w, 500, "生成总结失败: %v", err)
		return
	}
	name := filepath.Base(p)
	writeJSON(w, 200, map[string]any{"ok": true, "file": name, "url": "/reports/" + name})
}

func (s *Server) handleSummaryList(w http.ResponseWriter, r *http.Request) {
	dir := filepath.Join(s.cfg.DataDir, "reports")
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeJSON(w, 200, map[string]any{"items": []any{}})
		return
	}
	type item struct {
		Name  string `json:"name"`
		URL   string `json:"url"`
		MTime string `json:"mtime"`
		Size  int64  `json:"size"`
	}
	items := []item{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, item{
			Name:  e.Name(),
			URL:   "/reports/" + e.Name(),
			MTime: info.ModTime().In(s.cfg.Location).Format("2006-01-02 15:04:05"),
			Size:  info.Size(),
		})
	}
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
