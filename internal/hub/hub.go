package hub

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

var (
	ErrRoomNotFound = errors.New("hub: room not found")
	ErrRoomExists   = errors.New("hub: room already exists")
)

const idleCreateTimeout = time.Hour

type Hub struct {
	mu         sync.Mutex
	rooms      map[string]*Room
	evictAfter time.Duration
	log        *slog.Logger
}

func New(evictAfter time.Duration, log *slog.Logger) *Hub {
	return &Hub{
		rooms:      make(map[string]*Room),
		evictAfter: evictAfter,
		log:        log,
	}
}

func NewRoomID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func (h *Hub) CreateRoom(id, pwhash string) (*Room, error) {
	if id == "" {
		id = NewRoomID()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.rooms[id]; exists {
		return nil, ErrRoomExists
	}
	r := newRoom(id)
	r.pwhash = pwhash
	h.rooms[id] = r
	timeout := h.evictAfter
	if timeout < idleCreateTimeout {
		timeout = idleCreateTimeout
	}
	roomID, room := id, r
	r.evict = time.AfterFunc(timeout, func() { h.evictRoom(roomID, room) })
	h.log.Info("room created", "room", id, "locked", pwhash != "")
	return r, nil
}

func (h *Hub) GetRoom(id string) *Room {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rooms[id]
}

func (h *Hub) Join(roomID string, c *Client) (*Room, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[roomID]
	if !ok {
		return nil, ErrRoomNotFound
	}
	if r.evict != nil {
		r.evict.Stop()
		r.evict = nil
	}
	c.room = r
	r.addClient(c)
	h.log.Info("client joined", "room", roomID, "client", c.id, "peers", r.size())
	return r, nil
}

func (h *Hub) Unregister(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	r := c.room
	if r == nil {
		return
	}
	r.removeClient(c)
	h.log.Info("client left", "room", r.id, "client", c.id, "peers", r.size())

	if r.size() == 0 && r.evict == nil {
		roomID, room := r.id, r
		r.evict = time.AfterFunc(h.evictAfter, func() { h.evictRoom(roomID, room) })
	}
}

func (h *Hub) evictRoom(roomID string, r *Room) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if cur, ok := h.rooms[roomID]; !ok || cur != r {
		return
	}
	if r.size() != 0 {
		return
	}
	delete(h.rooms, roomID)
	h.log.Info("room evicted", "room", roomID)
}
