package httpapi

import "agentworkspace/internal/platform"

// Preserve the legacy string DTO for Go callers while distinguishing an
// omitted (or null) wire selector from an explicit selection of main.
type localMessageRequest struct {
	platform.MessageInput
	TopicID *string        `json:"topic_id"`
	QuoteID messageQuoteID `json:"reply_to_message_id"`
}
