package hub

import (
	"encoding/json/jsontext"
)

type MessageType int8

const (
	TypeInit       MessageType = 1
	TypeTextUpdate MessageType = 2
	TypeCursorMove MessageType = 3
	TypePeerJoin   MessageType = 4
	TypePeerLeave  MessageType = 5
	TypeJoin       MessageType = 6
	TypeJoinError  MessageType = 7
)

type Envelope struct {
	Payload jsontext.Value `json:"p"`
	Type    MessageType    `json:"t"`
}

type InitPayload struct {
	Self    string            `json:"u"` // 'u' for user/self ID
	Peers   map[string]Cursor `json:"s"` // 's' for session/peers map
	RoomID  string            `json:"r"` // 'r' for room ID
	Content string            `json:"c"` // 'c' for full text content
}

type TextUpdatePayload struct {
	Content string `json:"c"` // 'c' for delta or partial text updates
}

type Cursor struct {
	ID    string `json:"u"` // 'u' for user ID
	Index int64  `json:"i"` // 'i' for character/byte index
	Name  string `json:"n"` // 'n' for display name
	Color string `json:"k"` // 'k' for color code (e.g., HEX or ID)
}

type CursorMovePayload struct {
	Target Cursor `json:"g"` // 'g' for tracking target position
}

type PeerLeavePayload struct {
	ID string `json:"u"` // 'u' for user ID
}

type JoinPayload struct {
	Password string `json:"w,omitempty"`
}

type JoinErrorPayload struct {
	Reason string `json:"e"` // "wrong_password" | "room_not_found" | "rate_limited" | "protocol_error"
}

func EncodeJoinError(reason string) []byte {
	return mustEnvelope(TypeJoinError, JoinErrorPayload{Reason: reason})
}
