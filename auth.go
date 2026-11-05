package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// User 持久化在 data/users.json（标准库 SHA-256 + 随机盐）
type User struct {
	Username  string    `json:"username"`
	Salt      string    `json:"salt"`      // 16 字节随机盐
	Hash      string    `json:"hash"`      // SHA-256(salt + password)
	CreatedAt time.Time `json:"created_at"`
}

var (
	userMu      sync.RWMutex
	userCache   = map[string]*User{} // username → *User
	tokenCache  = map[string]string{} // token → username
	tokenExpiry = map[string]time.Time{}
	mediaSecret = "" // 持久化 HMAC key
)

const (
	tokenTTL     = 24 * time.Hour
	mediaURLTTL  = 15 * time.Minute // 签名 URL 有效期（拷贝出去 15 分钟后失效）
	mediaSigKey  = "MEDIA_SIGNING_KEY" // data 目录下文件名
	defaultUser  = "xxyxx"
	defaultPass  = "Miracle)))000"
)

// hashPassword = SHA-256(salt + password)，返回 hex
func hashPassword(salt, password string) string {
	h := sha256.New()
	h.Write([]byte(salt))
	h.Write([]byte(password))
	return hex.EncodeToString(h.Sum(nil))
}

func newSalt() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// loadUsers 从 data/users.json 加载，文件不存在则创建默认用户。
func loadUsers(dataDir string) error {
	userMu.Lock()
	defer userMu.Unlock()

	path := filepath.Join(dataDir, "users.json")
	data, err := os.ReadFile(path)
	if err == nil {
		var users []User
		if err := json.Unmarshal(data, &users); err != nil {
			return fmt.Errorf("解析 users.json: %w", err)
		}
		for i := range users {
			userCache[users[i].Username] = &users[i]
		}
		log.Printf("[auth] 载入 %d 个用户", len(userCache))
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	// 首次运行：创建默认用户
	salt := newSalt()
	u := User{
		Username:  defaultUser,
		Salt:      salt,
		Hash:      hashPassword(salt, defaultPass),
		CreatedAt: time.Now(),
	}
	userCache[u.Username] = &u

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body, _ := json.MarshalIndent([]User{u}, "", "  ")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return err
	}
	log.Printf("[auth] 首次启动，创建默认用户 %s / %s （可通过「修改密码」更改）", defaultUser, defaultPass)
	return nil
}

// saveUsers 持久化
func saveUsers(dataDir string) {
	userMu.RLock()
	defer userMu.RUnlock()
	path := filepath.Join(dataDir, "users.json")
	users := make([]User, 0, len(userCache))
	for _, u := range userCache {
		users = append(users, *u)
	}
	body, _ := json.MarshalIndent(users, "", "  ")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		log.Printf("[auth] 保存 users.json 失败: %v", err)
	}
}

// login 校验密码并签发 token
func login(username, password string) (token string, u *User, err error) {
	name := strings.ToLower(strings.TrimSpace(username))
	userMu.RLock()
	u = userCache[name]
	userMu.RUnlock()
	if u == nil || u.Hash != hashPassword(u.Salt, password) {
		return "", nil, errors.New("用户名或密码错误")
	}
	token = newToken()
	userMu.Lock()
	tokenCache[token] = name
	tokenExpiry[token] = time.Now().Add(tokenTTL)
	userMu.Unlock()
	return token, u, nil
}

// changePassword 改当前登录用户的密码
func changePassword(dataDir, token, oldPassword, newPassword string) error {
	username, ok := authenticate(token)
	if !ok {
		return errors.New("未登录或令牌已过期")
	}
	userMu.RLock()
	u := userCache[username]
	userMu.RUnlock()
	if u == nil {
		return errors.New("用户不存在")
	}
	if u.Hash != hashPassword(u.Salt, oldPassword) {
		return errors.New("原密码错误")
	}
	if len(newPassword) < 6 {
		return errors.New("新密码至少 6 位")
	}
	userMu.Lock()
	u.Salt = newSalt()
	u.Hash = hashPassword(u.Salt, newPassword)
	userMu.Unlock()
	saveUsers(dataDir)
	return nil
}

func authenticate(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	userMu.RLock()
	defer userMu.RUnlock()
	username, ok := tokenCache[token]
	if !ok {
		return "", false
	}
	if exp, ok2 := tokenExpiry[token]; ok2 && time.Now().After(exp) {
		return "", false
	}
	return username, true
}

func newToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func revokeToken(token string) {
	userMu.Lock()
	delete(tokenCache, token)
	delete(tokenExpiry, token)
	userMu.Unlock()
}

func tokenFromHeader(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return r.URL.Query().Get("token")
}

// requireAuth 中间件：接受 HandlerFunc（业务 handler）或 Handler（如 FileServer）
func requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := authenticate(tokenFromHeader(r)); !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="vi"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "需要登录"})
			return
		}
		h(w, r)
	}
}

// requireAuthHandler 中间件：接受任意 http.Handler（如 FileServer + StripPrefix）
func requireAuthHandler(h http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := authenticate(tokenFromHeader(r)); !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="vi"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "需要登录"})
			return
		}
		h.ServeHTTP(w, r)
	}
}

// —— auth handlers ——

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
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	token, u, err := login(body.Username, body.Password)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":    token,
		"username": u.Username,
		"expires":  time.Now().Add(tokenTTL).Unix(),
	})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	token := tokenFromHeader(r)
	username, ok := authenticate(token)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "未登录"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"username": username,
		"expires":  tokenExpiry[token].Unix(),
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	revokeToken(tokenFromHeader(r))
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
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	token := tokenFromHeader(r)
	if err := changePassword(s.cfg.DataDir, token, body.OldPassword, body.NewPassword); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// 改密后吊销当前 token，强制重新登录（更安全）
	revokeToken(token)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// —— media 签名 URL ——

// loadMediaSecret 从 data/ 读取或首次生成 HMAC key
func loadMediaSecret(dataDir string) error {
	path := filepath.Join(dataDir, mediaSigKey)
	data, err := os.ReadFile(path)
	if err == nil {
		mediaSecret = strings.TrimSpace(string(data))
		if len(mediaSecret) >= 16 {
			return nil
		}
	}
	if !errors.Is(err, os.ErrNotExist) && err != nil {
		return err
	}
	// 首次生成
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	mediaSecret = hex.EncodeToString(b)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(mediaSecret), 0o600)
}

// signMedia 生成签名 HMAC-SHA256(path + "|" + exp)
func signMedia(relPath string, exp time.Time) string {
	mac := hmac.New(sha256.New, []byte(mediaSecret))
	mac.Write([]byte(relPath + "|" + strconv.FormatInt(exp.Unix(), 10)))
	return hex.EncodeToString(mac.Sum(nil))
}

// buildSignedMediaURL 完整的带签名媒体 URL（15 分钟有效）
func buildSignedMediaURL(relPath string) string {
	exp := time.Now().Add(mediaURLTTL)
	sig := signMedia(relPath, exp)
	return "/media/" + relPath + "?sig=" + sig + "&exp=" + strconv.FormatInt(exp.Unix(), 10)
}

// verifyMediaSig 验证签名（过期或篡改返回 false）
func verifyMediaSig(relPath, sig, expStr string) bool {
	if sig == "" || expStr == "" {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return false
	}
	if time.Now().Unix() > exp {
		return false // 过期
	}
	expected := signMedia(relPath, time.Unix(exp, 0))
	return hmac.Equal([]byte(expected), []byte(sig))
}

// isMediaSignedRequest 判断当前 /media/ 请求是否有合法签名（不需要主 token）
func isMediaSignedRequest(r *http.Request) bool {
	sig := r.URL.Query().Get("sig")
	exp := r.URL.Query().Get("exp")
	if sig == "" || exp == "" {
		return false
	}
	// URL.Path 是 /media/xxx.jpg，strip prefix 后是 xxx.jpg
	// 但这里拿到的是原始路径，需要自己去掉 /media/ 前缀
	p := strings.TrimPrefix(r.URL.Path, "/media/")
	return verifyMediaSig(p, sig, exp)
}

// handleMediaSignedURL GET /api/media-url?path=xxx
// 用已登录 token 换取一个带签名的 15min 有效期 /media/ 临时 URL
func (s *Server) handleMediaSignedURL(w http.ResponseWriter, r *http.Request) {
	if _, ok := authenticate(tokenFromHeader(r)); !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "需要登录"})
		return
	}
	relPath := r.URL.Query().Get("path")
	if relPath == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 path 参数"})
		return
	}
	// 安全：禁止路径穿越
	if strings.Contains(relPath, "..") || strings.HasPrefix(relPath, "/") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "非法路径"})
		return
	}
	signed := buildSignedMediaURL(relPath)
	writeJSON(w, http.StatusOK, map[string]any{
		"url":       signed,
		"expires":   time.Now().Add(mediaURLTTL).Unix(),
		"path":      relPath,
	})
}

// requireMediaAuth 接受：主 token（Bearer 或 query） 或 签名 URL
func requireMediaAuth(h http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 1. 签名 URL 合法 → 放行（不需要主 token）
		if isMediaSignedRequest(r) {
			h.ServeHTTP(w, r)
			return
		}
		// 2. 主 token 合法 → 放行
		if _, ok := authenticate(tokenFromHeader(r)); ok {
			h.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="vi"`)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "需要登录"})
	}
}
