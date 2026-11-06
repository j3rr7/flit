package hub

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 64 << 10 // 64 KiB
	sendBuffer     = 256
	maxTextBytes   = 256 << 10 // 256 KiB
)

type Client struct {
	id    string
	name  string
	color string

	hub  *Hub
	room *Room
	conn *websocket.Conn
	send chan []byte
	done chan struct{}
	log  *slog.Logger

	limiter   *rate.Limiter
	closeOnce sync.Once
}

func NewClient(h *Hub, conn *websocket.Conn, name string, log *slog.Logger) *Client {
	id := randomID()
	return &Client{
		id:      id,
		name:    name,
		color:   colorFor(id),
		hub:     h,
		conn:    conn,
		send:    make(chan []byte, sendBuffer),
		done:    make(chan struct{}),
		log:     log.With("client", id, "name", name),
		limiter: rate.NewLimiter(rate.Limit(60), 120),
	}
}

func (c *Client) SendInit(r *Room) {
	data := mustEnvelope(TypeInit, r.snapshot(c))
	select {
	case c.send <- data:
	default:
		c.close()
	}
}

func (c *Client) ReadPump() {
	defer c.close()

	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				c.log.Warn("read error", "err", err)
			}
			return
		}
		if !c.limiter.Allow() {
			c.log.Warn("rate limit exceeded")
			_ = c.conn.WriteControl(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "rate limited"),
				time.Now().Add(time.Second),
			)
			return
		}
		var env Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			c.log.Warn("bad envelope", "err", err)
			continue
		}
		c.handle(env)
	}
}

func (c *Client) handle(env Envelope) {
	switch env.Type {
	case TypeTextUpdate:
		var p TextUpdatePayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return
		}
		if len(p.Content) > maxTextBytes {
			return
		}
		c.room.ApplyText(c, p.Content)

	case TypeCursorMove:
		var p CursorMovePayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return
		}
		c.room.ApplyCursor(c, p.Target)
	}
}

func (c *Client) WritePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
		c.close()
	}()

	for {
		select {
		case <-c.done:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
			return
		case msg := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (c *Client) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		if c.room != nil {
			c.hub.Unregister(c)
			c.room.AnnounceLeave(c)
		}
	})
}

func randomID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func colorFor(id string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return fmt.Sprintf("hsl(%d,70%%,50%%)", h.Sum32()%360)
}
