package qdrant

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Qdrant 是最小化的 Qdrant REST 客户端，只实现本系统需要的操作。
type Qdrant struct {
	base   string
	faces  string
	plates string
	cli    *http.Client
}

func NewQdrant(base, faces, plates string) *Qdrant {
	return &Qdrant{
		base:   base,
		faces:  faces,
		plates: plates,
		cli:    &http.Client{Timeout: 15 * time.Second},
	}
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
func (q *Qdrant) Ensure() error {
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
		"points": []any{map[string]any{"id": id, "vector": vec, "payload": payload}},
	}, nil)
}

func (q *Qdrant) UpsertPlate(id string, indices []uint32, values []float32, payload map[string]any) error {
	return q.do("PUT", "/collections/"+q.plates+"/points?wait=false", map[string]any{
		"points": []any{map[string]any{
			"id":      id,
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
// ids 是 point_id 列表（UUID 字符串）。
func (q *Qdrant) DeletePoints(collection string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	body := map[string]any{"points": ids}
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
