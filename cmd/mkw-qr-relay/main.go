package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type Config struct {
	ListenAddr           string
	PublicURL            string
	CloudreveURL         string
	SessionExpireSeconds int
	MaxSessions          int
}

type Session struct {
	ID               string    `json:"id"`
	Status           string    `json:"status"`
	Cloudreve        string    `json:"cloudreve"`
	WinDeviceName    string    `json:"win_device_name"`
	WinPublicKey     string    `json:"win_public_key"`
	MobileDeviceName string    `json:"mobile_device_name,omitempty"`
	MobilePublicKey  string    `json:"mobile_public_key,omitempty"`
	EncryptedPayload string    `json:"encrypted_payload,omitempty"`
	Nonce            string    `json:"nonce,omitempty"`
	Mac              string    `json:"mac,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	ExpiresAt        time.Time `json:"expires_at"`
	ConsumedAt       time.Time `json:"consumed_at,omitempty"`
}

type Store struct {
	mu       sync.Mutex
	sessions map[string]*Session
}

func NewStore() *Store {
	return &Store{sessions: make(map[string]*Session)}
}

func (s *Store) Cleanup(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.sessions {
		if now.After(sess.ExpiresAt) || (!sess.ConsumedAt.IsZero() && now.Sub(sess.ConsumedAt) > 30*time.Second) {
			delete(s.sessions, id)
		}
	}
}

func main() {
	envFile := flag.String("env", "", "optional env file")
	flag.Parse()
	if *envFile != "" {
		loadEnvFile(*envFile)
	}

	cfg := Config{
		ListenAddr:           env("LISTEN_ADDR", "127.0.0.1:8787"),
		PublicURL:            trimRightSlash(env("PUBLIC_URL", "http://127.0.0.1:8787")),
		CloudreveURL:         trimRightSlash(env("CLOUDREVE_URL", "")),
		SessionExpireSeconds: envInt("SESSION_EXPIRE_SECONDS", 120),
		MaxSessions:          envInt("MAX_SESSIONS", 5000),
	}
	if cfg.SessionExpireSeconds < 30 {
		cfg.SessionExpireSeconds = 30
	}

	store := NewStore()
	app := &App{cfg: cfg, store: store}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", app.handleHealth)
	mux.HandleFunc("/api/session/create", app.handleCreateSession)
	mux.HandleFunc("/api/session/", app.handleSession)
	// 兼容未被 Nginx strip 掉 /qr-login-relay 前缀的部署。
	mux.HandleFunc("/qr-login-relay/api/health", app.handleHealth)
	mux.HandleFunc("/qr-login-relay/api/session/create", app.handleCreateSession)
	mux.HandleFunc("/qr-login-relay/api/session/", app.handleSessionWithPrefix)

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for now := range ticker.C {
			store.Cleanup(now)
		}
	}()

	log.Printf("MKW QR Relay listening on %s, public_url=%s, cloudreve=%s", cfg.ListenAddr, cfg.PublicURL, cfg.CloudreveURL)
	if err := http.ListenAndServe(cfg.ListenAddr, withCORS(mux)); err != nil {
		log.Fatal(err)
	}
}

type App struct {
	cfg   Config
	store *Store
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"service": "mkw-qr-relay",
		"time":    time.Now().UTC().Format(time.RFC3339),
	})
}

func (a *App) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Cloudreve  string `json:"cloudreve"`
		DeviceName string `json:"device_name"`
		PublicKey  string `json:"public_key"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	req.Cloudreve = trimRightSlash(req.Cloudreve)
	if req.Cloudreve == "" {
		req.Cloudreve = a.cfg.CloudreveURL
	}
	if req.Cloudreve == "" || req.PublicKey == "" {
		writeError(w, http.StatusBadRequest, "cloudreve and public_key are required")
		return
	}

	a.store.mu.Lock()
	if len(a.store.sessions) >= a.cfg.MaxSessions {
		a.store.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, "too many active sessions")
		return
	}
	a.store.mu.Unlock()

	id := randomHex(32)
	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(a.cfg.SessionExpireSeconds) * time.Second)
	sess := &Session{
		ID:            id,
		Status:        "pending",
		Cloudreve:     req.Cloudreve,
		WinDeviceName: defaultString(req.DeviceName, "Windows 客户端"),
		WinPublicKey:  req.PublicKey,
		CreatedAt:     now,
		ExpiresAt:     expiresAt,
	}

	a.store.mu.Lock()
	a.store.sessions[id] = sess
	a.store.mu.Unlock()

	// 二维码内容使用紧凑格式，不再把 cloudreve/relay 域名和设备名明文写进二维码。
	// Android 端扫码后会使用“当前已登录 Cloudreve 站点”自动推导同域名中转地址：
	//   {Cloudreve站点}/qr-login-relay
	qrValues := url.Values{}
	qrValues.Set("v", "1")
	qrValues.Set("sid", id)
	qrValues.Set("pk", req.PublicKey)
	qrValues.Set("exp", fmt.Sprintf("%d", expiresAt.Unix()))
	qrPayload := "mkwqrlogin://login?" + qrValues.Encode()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"session_id": id,
		"status":     sess.Status,
		"expires_at": expiresAt.Format(time.RFC3339),
		"qr_payload": qrPayload,
	})
}

func (a *App) handleSessionWithPrefix(w http.ResponseWriter, r *http.Request) {
	r.URL.Path = strings.TrimPrefix(r.URL.Path, "/qr-login-relay")
	a.handleSession(w, r)
}

func (a *App) handleSession(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/session/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	id, action := parts[0], parts[1]

	switch action {
	case "status":
		a.handleStatus(w, r, id)
	case "scan":
		a.handleScan(w, r, id)
	case "confirm":
		a.handleConfirm(w, r, id)
	case "result":
		a.handleResult(w, r, id)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (a *App) getSession(id string) (*Session, error) {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	sess := a.store.sessions[id]
	if sess == nil {
		return nil, errors.New("session not found")
	}
	if time.Now().UTC().After(sess.ExpiresAt) {
		sess.Status = "expired"
	}
	return sess, nil
}

func (a *App) handleStatus(w http.ResponseWriter, _ *http.Request, id string) {
	sess, err := a.getSession(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"status":      sess.Status,
		"cloudreve":   sess.Cloudreve,
		"device_name": sess.WinDeviceName,
		"expires_at":  sess.ExpiresAt.Format(time.RFC3339),
	})
}

func (a *App) handleScan(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		DeviceName string `json:"device_name"`
	}
	_ = decodeJSON(r, &req)
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	sess := a.store.sessions[id]
	if sess == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	if time.Now().UTC().After(sess.ExpiresAt) {
		sess.Status = "expired"
		writeError(w, http.StatusGone, "session expired")
		return
	}
	if sess.Status == "pending" {
		sess.Status = "scanned"
		sess.MobileDeviceName = defaultString(req.DeviceName, "Android 手机端")
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": sess.Status})
}

func (a *App) handleConfirm(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		DeviceName       string `json:"device_name"`
		MobilePublicKey  string `json:"mobile_public_key"`
		EncryptedPayload string `json:"encrypted_payload"`
		Nonce            string `json:"nonce"`
		Mac              string `json:"mac"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.MobilePublicKey == "" || req.EncryptedPayload == "" || req.Nonce == "" || req.Mac == "" {
		writeError(w, http.StatusBadRequest, "missing encrypted payload")
		return
	}

	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	sess := a.store.sessions[id]
	if sess == nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	if time.Now().UTC().After(sess.ExpiresAt) {
		sess.Status = "expired"
		writeError(w, http.StatusGone, "session expired")
		return
	}
	if sess.Status == "confirmed" || sess.Status == "consumed" {
		writeError(w, http.StatusConflict, "session already used")
		return
	}

	sess.MobileDeviceName = defaultString(req.DeviceName, "Android 手机端")
	sess.MobilePublicKey = req.MobilePublicKey
	sess.EncryptedPayload = req.EncryptedPayload
	sess.Nonce = req.Nonce
	sess.Mac = req.Mac
	sess.Status = "confirmed"
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": sess.Status})
}

func (a *App) handleResult(w http.ResponseWriter, r *http.Request, id string) {
	sess, err := a.getSession(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	if sess.Status != "confirmed" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": sess.Status})
		return
	}
	if sess.EncryptedPayload == "" {
		writeError(w, http.StatusConflict, "result not ready")
		return
	}

	a.store.mu.Lock()
	sess.Status = "consumed"
	sess.ConsumedAt = time.Now().UTC()
	a.store.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                 true,
		"status":             "confirmed",
		"mobile_device_name": sess.MobileDeviceName,
		"mobile_public_key":  sess.MobilePublicKey,
		"encrypted_payload":  sess.EncryptedPayload,
		"nonce":              sess.Nonce,
		"mac":                sess.Mac,
	})
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "message": msg})
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	var out int
	if _, err := fmt.Sscanf(env(key, ""), "%d", &out); err == nil && out > 0 {
		return out
	}
	return def
}

func defaultString(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return strings.TrimSpace(v)
}
func trimRightSlash(v string) string { return strings.TrimRight(strings.TrimSpace(v), "/") }

func loadEnvFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		key := strings.TrimSpace(parts[0])
		val := strings.Trim(strings.TrimSpace(parts[1]), "\"'")
		if os.Getenv(key) == "" {
			_ = os.Setenv(key, val)
		}
	}
}
