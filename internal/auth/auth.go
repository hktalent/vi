package auth

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
	Salt      string    `json:"salt"`
	Hash      string    `json:"hash"`
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
	TokenTTL      = 24 * time.Hour
	MediaURLTTL   = 15 * time.Minute // 签名 URL 有效期（拷贝出去 15 分钟后失效）
	mediaSigKey   = "MEDIA_SIGNING_KEY"
	defaultUser   = "xxyxx"
	defaultPass   = "Miracle)))000"
)

// HashPassword = SHA-256(salt + password)，返回 hex
func HashPassword(salt, password string) string {
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

// LoadUsers 从 data/users.json 加载，文件不存在则创建默认用户。
func LoadUsers(dataDir string) error {
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

	salt := newSalt()
	u := User{
		Username:  defaultUser,
		Salt:      salt,
		Hash:      HashPassword(salt, defaultPass),
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

// Login 校验密码并签发 token
func Login(username, password string) (token string, u *User, err error) {
	name := strings.ToLower(strings.TrimSpace(username))
	userMu.RLock()
	u = userCache[name]
	userMu.RUnlock()
	if u == nil || u.Hash != HashPassword(u.Salt, password) {
		return "", nil, errors.New("用户名或密码错误")
	}
	token = newToken()
	userMu.Lock()
	tokenCache[token] = name
	tokenExpiry[token] = time.Now().Add(TokenTTL)
	userMu.Unlock()
	return token, u, nil
}

// ChangePassword 改当前登录用户的密码
func ChangePassword(dataDir, token, oldPassword, newPassword string) error {
	username, ok := Authenticate(token)
	if !ok {
		return errors.New("未登录或令牌已过期")
	}
	userMu.RLock()
	u := userCache[username]
	userMu.RUnlock()
	if u == nil {
		return errors.New("用户不存在")
	}
	if u.Hash != HashPassword(u.Salt, oldPassword) {
		return errors.New("原密码错误")
	}
	if len(newPassword) < 6 {
		return errors.New("新密码至少 6 位")
	}
	userMu.Lock()
	u.Salt = newSalt()
	u.Hash = HashPassword(u.Salt, newPassword)
	userMu.Unlock()
	saveUsers(dataDir)
	return nil
}

// Authenticate 校验 token，返回用户名和是否有效
func Authenticate(token string) (string, bool) {
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

// TokenExpiry 返回 token 的过期时间（已过期则返回零值）
func TokenExpiry(token string) time.Time {
	userMu.RLock()
	defer userMu.RUnlock()
	return tokenExpiry[token]
}

func newToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// RevokeToken 吊销某个 token（登出用）
func RevokeToken(token string) {
	userMu.Lock()
	delete(tokenCache, token)
	delete(tokenExpiry, token)
	userMu.Unlock()
}

// TokenFromHeader 从 Authorization header 或 URL query 里取 token
func TokenFromHeader(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return r.URL.Query().Get("token")
}

// —— media 签名 URL ——

// LoadMediaSecret 从 data/ 读取或首次生成 HMAC key
func LoadMediaSecret(dataDir string) error {
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
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	mediaSecret = hex.EncodeToString(b)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(mediaSecret), 0o600)
}

// buildSignedMediaURL 完整的带签名媒体 URL
func BuildSignedMediaURL(relPath string) string {
	exp := time.Now().Add(MediaURLTTL)
	mac := hmac.New(sha256.New, []byte(mediaSecret))
	mac.Write([]byte(relPath + "|" + strconv.FormatInt(exp.Unix(), 10)))
	sig := hex.EncodeToString(mac.Sum(nil))
	return "/media/" + relPath + "?sig=" + sig + "&exp=" + strconv.FormatInt(exp.Unix(), 10)
}

// isMediaSignedRequest 判断当前 /media/ 请求是否有合法签名
func IsMediaSignedRequest(r *http.Request) bool {
	sig := r.URL.Query().Get("sig")
	exp := r.URL.Query().Get("exp")
	if sig == "" || exp == "" {
		return false
	}
	expTs, err := strconv.ParseInt(exp, 10, 64)
	if err != nil {
		return false
	}
	if time.Now().Unix() > expTs {
		return false
	}
	p := strings.TrimPrefix(r.URL.Path, "/media/")
	mac := hmac.New(sha256.New, []byte(mediaSecret))
	mac.Write([]byte(p + "|" + exp))
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(sig))
}

// —— 中间件 ——

// RequireAuth 中间件：接受 HandlerFunc
func RequireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := Authenticate(TokenFromHeader(r)); !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="vi"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

// RequireMediaAuth 接受：主 token（Bearer 或 query） 或 签名 URL
func RequireMediaAuth(h http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if IsMediaSignedRequest(r) {
			h.ServeHTTP(w, r)
			return
		}
		if _, ok := Authenticate(TokenFromHeader(r)); ok {
			h.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="vi"`)
		w.WriteHeader(http.StatusUnauthorized)
	}
}
