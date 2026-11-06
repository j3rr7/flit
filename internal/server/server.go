package server

import (
	"bytes"
	"encoding/json"
	"flit/internal/hub"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		return u.Host == r.Host
	},
}

func New(h *hub.Hub, staticFS fs.FS, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()

	limiter := newIPLimiter(rate.Every(6*time.Second), 5, 8192)

	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		handleWS(h, log, limiter, w, r)
	})
	mux.HandleFunc("/api/rooms", func(w http.ResponseWriter, r *http.Request) {
		handleCreateRoom(h, log, w, r)
	})
	mux.HandleFunc("POST /api/rooms/{id}/password", func(w http.ResponseWriter, r *http.Request) {
		handleSetPassword(h, log, w, r)
	})
	mux.HandleFunc("/api/rooms/", func(w http.ResponseWriter, r *http.Request) {
		handleGetRoom(h, log, w, r)
	})
	mux.HandleFunc("/", rootHandler(h, log, staticFS))
	return mux
}

type createRoomReq struct {
	Password string `json:"password,omitempty"`
}

type createRoomResp struct {
	ID          string `json:"id"`
	HasPassword bool   `json:"hasPassword"`
}

func handleCreateRoom(h *hub.Hub, log *slog.Logger, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req createRoomReq
	if r.ContentLength > 0 {
		if err := json.Unmarshal(readAll(r), &req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
	}
	pwhash := ""
	if req.Password != "" {
		var err error
		pwhash, err = hub.HashPassword(req.Password)
		if err != nil {
			log.Error("hash password", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	id := hub.NewRoomID()
	if _, err := h.CreateRoom(id, pwhash); err != nil {
		http.Error(w, "room id collision", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(createRoomResp{ID: id, HasPassword: pwhash != ""})
}

func handleSetPassword(h *hub.Hub, log *slog.Logger, w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" || len(id) > 64 {
		http.Error(w, "bad room id", http.StatusBadRequest)
		return
	}
	var req createRoomReq
	if r.ContentLength > 0 {
		if err := json.Unmarshal(readAll(r), &req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
	}
	if req.Password == "" {
		http.Error(w, "password required", http.StatusBadRequest)
		return
	}
	room := h.GetRoom(id)
	if room == nil {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}
	hash, err := hub.HashPassword(req.Password)
	if err != nil {
		log.Error("hash password", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !room.SetPassword(hash) {
		http.Error(w, "already locked", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleGetRoom(h *hub.Hub, log *slog.Logger, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/rooms/")
	if id == "" || strings.ContainsAny(id, "/") || len(id) > 64 {
		http.Error(w, "bad room id", http.StatusBadRequest)
		return
	}
	room := h.GetRoom(id)
	resp := map[string]bool{"exists": room != nil, "hasPassword": room != nil && room.HasPassword()}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func rootHandler(h *hub.Hub, log *slog.Logger, staticFS fs.FS) http.HandlerFunc {
	spa := spaHandler(staticFS, log)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			id := hub.NewRoomID()
			if _, err := h.CreateRoom(id, ""); err != nil {
				http.Error(w, "failed to create room", http.StatusInternalServerError)
				return
			}
			http.Redirect(w, r, "/r/"+id, http.StatusFound)
			return
		}
		spa.ServeHTTP(w, r)
	}
}

func handleWS(h *hub.Hub, log *slog.Logger, limiter *ipLimiter, w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	roomID := strings.TrimSpace(q.Get("room"))
	if roomID == "" || len(roomID) > 64 {
		http.Error(w, "room required", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(q.Get("name"))
	if name == "" {
		name = "anon"
	}
	if len(name) > 32 {
		name = name[:32]
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Warn("upgrade failed", "err", err)
		return
	}

	conn.SetReadLimit(4096)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		_ = conn.Close()
		return
	}
	var env hub.Envelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Type != hub.TypeJoin {
		writeJoinErrorAndClose(conn, "protocol_error")
		return
	}
	var jp hub.JoinPayload
	_ = json.Unmarshal(env.Payload, &jp)

	if !limiter.Allow(clientIP(r)) {
		writeJoinErrorAndClose(conn, "rate_limited")
		return
	}

	room := h.GetRoom(roomID)
	if room == nil {
		writeJoinErrorAndClose(conn, "room_not_found")
		return
	}
	if room.HasPassword() && !room.VerifyPassword(jp.Password) {
		writeJoinErrorAndClose(conn, "wrong_password")
		return
	}

	conn.SetReadDeadline(time.Time{})
	c := hub.NewClient(h, conn, name, log)
	if _, err := h.Join(roomID, c); err != nil {
		writeJoinErrorAndClose(conn, "room_not_found")
		return
	}
	go c.WritePump()
	c.SendInit(room)
	room.AnnounceJoin(c)
	c.ReadPump()
}

func writeJoinErrorAndClose(conn *websocket.Conn, reason string) {
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	_ = conn.WriteMessage(websocket.TextMessage, hub.EncodeJoinError(reason))
	_ = conn.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.ClosePolicyViolation, reason))
	_ = conn.Close()
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type ipLimiter struct {
	mu    sync.Mutex
	m     map[string]*rate.Limiter
	limit rate.Limit
	burst int
	max   int
}

func newIPLimiter(limit rate.Limit, burst, max int) *ipLimiter {
	return &ipLimiter{m: make(map[string]*rate.Limiter), limit: limit, burst: burst, max: max}
}

func (l *ipLimiter) Allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	lim, ok := l.m[ip]
	if !ok {
		if len(l.m) >= l.max {
			for k := range l.m {
				delete(l.m, k)
				break
			}
		}
		lim = rate.NewLimiter(l.limit, l.burst)
		l.m[ip] = lim
	}
	return lim.Allow()
}

func spaHandler(staticFS fs.FS, log *slog.Logger) http.Handler {
	indexHTML, err := fs.ReadFile(staticFS, "index.html")
	if err != nil {
		panic("flit: web/index.html missing from embedded FS: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(staticFS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			serveIndex(w, r, indexHTML)
			return
		}

		name := strings.TrimPrefix(r.URL.Path, "/")

		if info, err := fs.Stat(staticFS, name); err == nil && !info.IsDir() {
			fileServer.ServeHTTP(w, r)
			return
		}

		log.Debug("spa fallback", "path", r.URL.Path)
		serveIndex(w, r, indexHTML)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, content []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(content))
}

func readAll(r *http.Request) []byte {
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r.Body)
	return buf.Bytes()
}
