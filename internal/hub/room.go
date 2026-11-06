package hub

import (
	"encoding/json/v2"
	"sync"
	"time"
)

type Room struct {
	id string

	mu      sync.RWMutex
	content []byte
	clients map[*Client]struct{}
	cursors map[string]Cursor

	pwhash string

	evict *time.Timer
}

func newRoom(id string) *Room {
	return &Room{
		id:      id,
		clients: make(map[*Client]struct{}),
		cursors: make(map[string]Cursor),
	}
}

func (r *Room) addClient(c *Client) {
	r.mu.Lock()
	r.clients[c] = struct{}{}
	r.cursors[c.id] = Cursor{ID: c.id, Name: c.name, Color: c.color}
	r.mu.Unlock()
}

func (r *Room) removeClient(c *Client) {
	r.mu.Lock()
	delete(r.clients, c)
	delete(r.cursors, c.id)
	r.mu.Unlock()
}

func (r *Room) size() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.clients)
}

func (r *Room) snapshot(self *Client) InitPayload {
	r.mu.RLock()
	defer r.mu.RUnlock()
	peers := make(map[string]Cursor, len(r.cursors))
	for id, cur := range r.cursors {
		peers[id] = cur
	}
	return InitPayload{
		Self:    self.id,
		Peers:   peers,
		RoomID:  r.id,
		Content: string(r.content),
	}
}

func (r *Room) ApplyText(sender *Client, content string) {
	r.mu.Lock()
	old := r.content
	newContent := []byte(content)
	r.content = newContent

	prefix := 0
	for prefix < len(old) && prefix < len(newContent) && old[prefix] == newContent[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(old)-prefix && suffix < len(newContent)-prefix &&
		old[len(old)-1-suffix] == newContent[len(newContent)-1-suffix] {
		suffix++
	}
	oldEnd := len(old) - suffix
	newEnd := len(newContent) - suffix
	delta := newEnd - oldEnd

	if delta != 0 || oldEnd != prefix {
		for id, cur := range r.cursors {
			if id == sender.id {
				continue
			}
			idx := int(cur.Index)
			switch {
			case idx <= prefix:
			case idx >= oldEnd:
				cur.Index = int64(idx + delta)
			default:
				cur.Index = int64(prefix)
			}
			r.cursors[id] = cur
		}
	}
	r.mu.Unlock()

	r.broadcast(mustEnvelope(TypeTextUpdate, TextUpdatePayload{Content: content}), sender)
}

func (r *Room) ApplyCursor(sender *Client, cur Cursor) {
	cur.ID = sender.id
	cur.Name = sender.name
	cur.Color = sender.color
	r.mu.Lock()
	r.cursors[sender.id] = cur
	r.mu.Unlock()
	r.broadcast(mustEnvelope(TypeCursorMove, CursorMovePayload{Target: cur}), nil)
}

func (r *Room) AnnounceJoin(c *Client) {
	r.mu.RLock()
	cur, ok := r.cursors[c.id]
	r.mu.RUnlock()
	if !ok {
		return
	}
	r.broadcast(mustEnvelope(TypePeerJoin, cur), c)
}

func (r *Room) AnnounceLeave(c *Client) {
	r.broadcast(mustEnvelope(TypePeerLeave, PeerLeavePayload{ID: c.id}), c)
}

func (r *Room) broadcast(data []byte, except *Client) {
	r.mu.RLock()
	var slow []*Client
	for c := range r.clients {
		if c == except {
			continue
		}
		select {
		case c.send <- data:
		default:
			slow = append(slow, c)
		}
	}
	r.mu.RUnlock()

	for _, c := range slow {
		c.log.Warn("slow client, disconnecting")
		c.close()
	}
}

func mustEnvelope(t MessageType, payload any) []byte {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	data, err := json.Marshal(Envelope{Type: t, Payload: raw})
	if err != nil {
		panic(err)
	}
	return data
}

func (r *Room) HasPassword() bool { return r.pwhash != "" }

func (r *Room) VerifyPassword(plain string) bool {
	return r.pwhash != "" && VerifyPassword(r.pwhash, plain)
}

func (r *Room) SetPassword(hash string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pwhash != "" {
		return false
	}
	r.pwhash = hash
	return true
}
