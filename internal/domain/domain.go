package domain

import (
	"encoding/json"
	"time"
)

type MemberStatus string

const (
	MemberStatusActive MemberStatus = "active"
	MemberStatusLeft   MemberStatus = "left"
)

type DeliveryState string

const (
	DeliveryStateQueued   DeliveryState = "queued"
	DeliveryStateAccepted DeliveryState = "accepted"
	DeliveryStateFailed   DeliveryState = "failed"
)

type Group struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

type Member struct {
	ID         string       `json:"id"`
	GroupID    string       `json:"group_id"`
	Name       string       `json:"name"`
	Role       string       `json:"role"`
	ProviderID string       `json:"provider_id"`
	Model      string       `json:"model"`
	CreatorID  string       `json:"creator_id"`
	OwnerID    string       `json:"owner_id"`
	Status     MemberStatus `json:"status"`
	Workspace  string       `json:"workspace"`
	CreatedAt  time.Time    `json:"created_at"`
}

type Conversation struct {
	ID        string    `json:"id"`
	GroupID   string    `json:"group_id"`
	MemberID  string    `json:"member_id"`
	TopicID   string    `json:"topic_id"`
	CreatedAt time.Time `json:"created_at"`
}

type Message struct {
	ID         string     `json:"id"`
	GroupID    string     `json:"group_id"`
	TopicID    string     `json:"topic_id"`
	SenderID   string     `json:"sender_id"`
	Content    string     `json:"content"`
	CreatedAt  time.Time  `json:"created_at"`
	Deliveries []Delivery `json:"deliveries"`
}

func (m Message) MarshalJSON() ([]byte, error) {
	type message Message
	if m.Deliveries == nil {
		m.Deliveries = []Delivery{}
	}
	return json.Marshal(message(m))
}

type Delivery struct {
	ID             string        `json:"id"`
	MessageID      string        `json:"message_id"`
	ConversationID string        `json:"conversation_id"`
	RecipientID    string        `json:"recipient_id"`
	State          DeliveryState `json:"state"`
	TurnID         string        `json:"turn_id"`
	Error          string        `json:"error"`
	CreatedAt      time.Time     `json:"created_at"`
}
