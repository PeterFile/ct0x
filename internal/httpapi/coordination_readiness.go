package httpapi

import (
	"context"
	"net/http"
	"strings"
	"unicode/utf8"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

type coordinationReadinessInspector interface {
	InspectRoleReadiness(context.Context, store.PendingDelivery) (platform.RoleReadinessInspection, error)
}

type coordinationReadinessAPI struct {
	db        *store.Store
	inspector coordinationReadinessInspector
}

// CoordinationReadinessRoutes is an opt-in local-user diagnostic, not a model
// tool or execution gate. The trusted caller must supply its Store and the same
// Preparer; the narrow interface is a test seam, not proof of authority/readiness.
// Observations are not reservations, and ordinary deliveries are explicitly non-role.
func CoordinationReadinessRoutes(db *store.Store, inspector coordinationReadinessInspector) []Route {
	a := coordinationReadinessAPI{db: db, inspector: inspector}
	inspect := noQuery(a.inspect)
	return []Route{
		{http.MethodGet, "/api/v1/groups/{group}/messages/{message}/deliveries/{delivery}/coordination-readiness", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			inspect(w, r)
		}},
	}
}

func (a coordinationReadinessAPI) inspect(w http.ResponseWriter, r *http.Request) {
	// Reject declared bodies, including chunked/unknown length, without decoding
	// any client-selected identity, execution configuration or model state.
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		operationError(w, store.ErrInvalid)
		return
	}
	groupID, messageID, deliveryID := r.PathValue("group"), r.PathValue("message"), r.PathValue("delivery")
	for _, id := range []string{groupID, messageID, deliveryID} {
		// Match the existing coordination opaque-ID contract without normalizing it.
		if id == "" || len(id) > 4096 || !utf8.ValidString(id) || strings.TrimSpace(id) != id || strings.ContainsAny(id, "\x00\r\n") {
			operationError(w, store.ErrInvalid)
			return
		}
	}
	message, err := a.db.GetMessage(r.Context(), groupID, messageID)
	if err != nil {
		operationError(w, err)
		return
	}
	for _, delivery := range message.Deliveries {
		if delivery.ID != deliveryID {
			continue
		}
		pending := store.PendingDelivery{GroupID: message.GroupID, SenderID: message.SenderID, Delivery: delivery}
		inspection, err := a.inspector.InspectRoleReadiness(r.Context(), pending)
		operationResult(w, http.StatusOK, inspection, err)
		return
	}
	operationError(w, store.ErrNotFound)
}
