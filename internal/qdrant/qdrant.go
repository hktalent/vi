package qdrant

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// qdrantPointID 把任意业务 personID 确定性映射到 Qdrant 允许的 UUID 格式。
// Qdrant 要求 point ID 必须是 uint64 或 UUID (RFC4122)，不能是 "import_xxx" 这种业务字符串。
// 这里用 personID 的 MD5 摘要构造伪 UUIDv4（前 4 位设 0x4，后 2 位设 0x8/0x9 变 version/variant 位），
// 保证同 personID → 同 UUID → upsert 覆盖语义天然匹配。
func qdrantPointID(personID string) string {
	sum := md5.Sum([]byte(personID))
	b := sum[:]
	b[6] = (b[6] & 0x0f) | 0x40 // version = 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant = RFC4122
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]))
}

// Qdrant 是最小化的 Qdrant REST 客户端，只实现本系统需要的操作。
type Qdrant struct {
	base   string
	faces  string
	plates string
	cli    *http.Client
	fast   *http.Client // 健康检查/探测用短超时客户端

	ensureMu  sync.Mutex
	ensuredAt time.Time // Ensure 成功的时间（TTL 内直接返回，避免健康检查每次 4 个请求）
}

// ensureTTL Ensure 成功结果的复用窗口。集合结构不会频繁变化，
// 每次 /api/health 都跑 2 次 GET + 2 次建索引 PUT 纯属浪费（实测 130~175ms）。
const ensureTTL = 10 * time.Minute

func NewQdrant(base, faces, plates string) *Qdrant {
	return &Qdrant{
		base:   base,
		faces:  faces,
		plates: plates,
		cli:    &http.Client{Timeout: 15 * time.Second},
		// 健康探测超时不能太苛刻：Qdrant 在批量 upsert 时可能几百 ms 不响应，
		// 300ms 会误判成"未连接"
		fast: &http.Client{Timeout: time.Second},
	}
}

// Ping 只发一个轻量请求确认 Qdrant 还活着（健康检查用）。
func (q *Qdrant) Ping() error {
	resp, err := q.fast.Get(q.base + "/collections")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("qdrant ping: %s", resp.Status)
	}
	return nil
}

type Point struct {
	ID      string         `json:"id"`
	Score   float64        `json:"score"`
	Payload map[string]any `json:"payload"`
}

func (q *Qdrant) do(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, q.base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := q.cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("qdrant %s %s: %s %s", method, path, resp.Status, string(data))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Ensure 保证两个集合存在。人脸用 512 维余弦向量，车牌用文本稀疏向量。
// 成功后在 ensureTTL 内复用结果：调用方（健康检查、每次拷贝 Qdrant 前）不再重复建索引。
func (q *Qdrant) Ensure() error {
	q.ensureMu.Lock()
	defer q.ensureMu.Unlock()
	if !q.ensuredAt.IsZero() && time.Since(q.ensuredAt) < ensureTTL {
		return nil
	}
	if err := q.ensureOnce(); err != nil {
		q.ensuredAt = time.Time{} // 失败不缓存，下次重试
		return err
	}
	q.ensuredAt = time.Now()
	return nil
}

func (q *Qdrant) ensureOnce() error {
	var res struct {
		Result struct {
			Status string `json:"status"`
		} `json:"result"`
	}
	err := q.do("GET", "/collections/"+q.faces, nil, &res)
	if err != nil {
		if err := q.do("PUT", "/collections/"+q.faces, map[string]any{
			"vectors": map[string]any{"size": 512, "distance": "Cosine"},
		}, nil); err != nil {
			return fmt.Errorf("创建人脸集合失败: %w", err)
		}
	}
	err = q.do("GET", "/collections/"+q.plates, nil, &res)
	if err != nil {
		if err := q.do("PUT", "/collections/"+q.plates, map[string]any{
			"sparse_vectors": map[string]any{"text": map[string]any{}},
		}, nil); err != nil {
			return fmt.Errorf("创建车牌集合失败: %w", err)
		}
	}
	q.do("PUT", "/collections/"+q.faces+"/index?wait=false", map[string]any{
		"field_name": "person_id", "field_schema": "keyword",
	}, nil)
	q.do("PUT", "/collections/"+q.plates+"/index?wait=false", map[string]any{
		"field_name": "plate", "field_schema": "keyword",
	}, nil)
	return nil
}

// Recreate 用相同参数重建两个集合（会清空所有点）。
func (q *Qdrant) Recreate() error {
	if err := q.do("DELETE", "/collections/"+q.faces, nil, nil); err != nil {
		return fmt.Errorf("删除人脸集合: %w", err)
	}
	if err := q.do("DELETE", "/collections/"+q.plates, nil, nil); err != nil {
		return fmt.Errorf("删除车牌集合: %w", err)
	}
	return q.Ensure()
}

type searchResp struct {
	Result struct {
		Points []Point `json:"points"`
	} `json:"result"`
}

// SearchFace 返回最相似的人脸；低于阈值时 score 仍会返回，由调用方判定。
func (q *Qdrant) SearchFace(vec []float32, threshold float64) (Point, error) {
	var resp searchResp
	err := q.do("POST", "/collections/"+q.faces+"/points/query", map[string]any{
		"query":           vec,
		"limit":           1,
		"with_payload":    true,
		"score_threshold": threshold,
	}, &resp)
	if err != nil {
		return Point{}, err
	}
	if len(resp.Result.Points) == 0 {
		return Point{}, nil
	}
	return resp.Result.Points[0], nil
}

func (q *Qdrant) UpsertFace(id string, vec []float32, payload map[string]any) error {
	return q.do("PUT", "/collections/"+q.faces+"/points?wait=false", map[string]any{
		"points": []any{map[string]any{"id": qdrantPointID(id), "vector": vec, "payload": payload}},
	}, nil)
}

func (q *Qdrant) UpsertPlate(id string, indices []uint32, values []float32, payload map[string]any) error {
	return q.do("PUT", "/collections/"+q.plates+"/points?wait=false", map[string]any{
		"points": []any{map[string]any{
			"id":      qdrantPointID(id),
			"vector":  map[string]any{"text": map[string]any{"indices": indices, "values": values}},
			"payload": payload,
		}},
	}, nil)
}

// QueryPlate 用字符 n-gram 稀疏向量做模糊匹配。
func (q *Qdrant) QueryPlate(indices []uint32, values []float32, limit int) ([]Point, error) {
	var resp searchResp
	err := q.do("POST", "/collections/"+q.plates+"/points/query", map[string]any{
		"query":        map[string]any{"indices": indices, "values": values},
		"using":        "text",
		"limit":        limit,
		"with_payload": true,
	}, &resp)
	if err != nil {
		return nil, err
	}
	return resp.Result.Points, nil
}

// DeletePoints 从指定 collection 删除一批 point。
// ids 可以是业务 personID（任意字符串），内部自动映射到 Qdrant UUID。
func (q *Qdrant) DeletePoints(collection string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	uuids := make([]string, len(ids))
	for i, s := range ids {
		uuids[i] = qdrantPointID(s)
	}
	body := map[string]any{"points": uuids}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", q.base+"/collections/"+collection+"/points/delete", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := q.cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		x, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete points %s: %s", resp.Status, x)
	}
	return nil
}

// PlateSparse 把车牌号转成字符三元组稀疏向量，同一车牌总能得到同一向量。
func PlateSparse(plate string) ([]uint32, []float32) {
	r := []rune(plate)
	if len(r) == 0 {
		return nil, nil
	}
	type pair struct {
		h uint32
		c float32
	}
	seen := map[uint32]float32{}
	add := func(s string) {
		var h uint32 = 2166136261
		for _, ch := range []byte(s) {
			h ^= uint32(ch)
			h *= 16777619
		}
		seen[h%4096] += 1
	}
	add(string(r))
	for i := 0; i+1 < len(r); i++ {
		add(string(r[i : i+2]))
	}
	for i := 0; i+2 < len(r); i++ {
		add(string(r[i : i+3]))
	}
	out := make([]uint32, 0, len(seen))
	vals := make([]float32, 0, len(seen))
	for h, v := range seen {
		out = append(out, h)
		vals = append(vals, v)
	}
	return out, vals
}
