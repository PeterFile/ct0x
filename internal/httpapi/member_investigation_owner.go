package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

// Only these exact field names belong on the owner write wire. The outer
// scalar decoder rejects invalid Unicode and DecodeJSON rejects duplicate keys
// (including nested keys). The field decoders distinguish null from omission.
type memberOwnerWorkFields map[string]any

func (dst memberOwnerWorkFields) UnmarshalJSON(body []byte) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return store.ErrInvalid
	}
	for name, value := range fields {
		field, ok := dst[name]
		if !ok || json.Unmarshal(value, field) != nil {
			return store.ErrInvalid
		}
	}
	return nil
}

type memberOwnerWorkVersion domain.ArtifactVersionRef

func (ref *memberOwnerWorkVersion) UnmarshalJSON(body []byte) error {
	var artifactID, versionID memberToolField[string]
	fields := memberOwnerWorkFields{"artifact_id": &artifactID, "version_id": &versionID}
	if fields.UnmarshalJSON(body) != nil || !artifactID.present || !versionID.present {
		return store.ErrInvalid
	}
	*ref = memberOwnerWorkVersion{ArtifactID: artifactID.value, VersionID: versionID.value}
	return nil
}

func (a memberToolAPI) delegateWork(w http.ResponseWriter, r *http.Request, token string) {
	key, ok := requestKey(w, r)
	if !ok {
		return
	}
	if key == "" {
		operationError(w, store.ErrInvalid)
		return
	}
	var memberID, question memberToolField[string]
	var allowTopic memberToolField[bool]
	var versions memberToolField[[]memberOwnerWorkVersion]
	fields := memberOwnerWorkFields{"member_id": &memberID, "question": &question, "allow_topic_messages": &allowTopic, "artifact_versions": &versions}
	if !decodeMemberWorkspaceJSON(w, r, &fields) {
		return
	}
	if !validWorkReportID(memberID.value) || !validWorkInputText(question.value, domain.MaxWorkQuestionBytes) {
		operationError(w, store.ErrInvalid)
		return
	}
	input := domain.DelegateWorkInput{MemberID: memberID.value, Question: question.value, AllowTopicMessages: allowTopic.value}
	for _, ref := range versions.value {
		input.ArtifactVersions = append(input.ArtifactVersions, domain.ArtifactVersionRef(ref))
	}
	// Keep the original key and payload; B owns subset validation, business
	// bounds and idempotent effects. Do not retry a write in the handler.
	const op = platform.DelegateWorkOperation
	d, err := a.tools.DelegateWork(r.Context(), token, input, key)
	if err != nil {
		taskWorkWriteError(w, op, err)
		return
	}
	if !validWorkReportID(d.ID) || !validWorkReportID(d.DeliveryID) || !validWorkReportID(d.MessageID) || !validWorkInputTimestamp(d.CreatedAt) {
		taskWorkWriteError(w, op, store.ErrTaskWorkMutationUnknown)
		return
	}
	WriteJSON(w, http.StatusCreated, struct {
		DelegationID string    `json:"delegation_id"`
		CreatedAt    time.Time `json:"created_at"`
		WakeQueued   bool      `json:"wake_queued"`
	}{d.ID, d.CreatedAt.UTC(), true})
}

func (a memberToolAPI) handbackWork(w http.ResponseWriter, r *http.Request, token string) {
	key, ok := requestKey(w, r)
	if !ok {
		return
	}
	if key == "" {
		operationError(w, store.ErrInvalid)
		return
	}
	var summary memberToolField[string]
	fields := memberOwnerWorkFields{"summary": &summary}
	if !decodeMemberWorkspaceJSON(w, r, &fields) {
		return
	}
	if !validWorkInputText(summary.value, domain.MaxWorkReportBytes) {
		operationError(w, store.ErrInvalid)
		return
	}
	const op = platform.HandbackWorkOperation
	h, err := a.tools.HandbackWork(r.Context(), token, domain.HandbackWorkInput{Summary: summary.value}, key)
	if err != nil {
		taskWorkWriteError(w, op, err)
		return
	}
	if !validWorkReportID(h.ID) || !validWorkReportID(h.NoticeMessageID) || !validWorkInputTimestamp(h.CreatedAt) {
		taskWorkWriteError(w, op, store.ErrTaskWorkMutationUnknown)
		return
	}
	// This is a durable non-waking notice, not a new assignment, a release of
	// resources, or the return of a single input.
	WriteJSON(w, http.StatusCreated, struct {
		HandbackID      string    `json:"handback_id"`
		NoticeMessageID string    `json:"notice_message_id"`
		CreatedAt       time.Time `json:"created_at"`
	}{h.ID, h.NoticeMessageID, h.CreatedAt.UTC()})
}
