package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"agentworkspace/internal/coordination"
	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
)

func TestTaskSuccessorHTTPRequiresMigrationWithoutChangingOrdinaryAdmission(t *testing.T) {
	a := openTaskSuccessorAPI(t, "")
	g := newGroup(t, a, "successor migration")
	base := "/api/v1/groups/" + g.ID
	message := decoded[domain.Message](t, a.request(http.MethodPost, base+"/messages",
		`{"content":"继续处理已接纳的成果。","topic_id":"release","recipient_ids":[]}`, "source"), http.StatusCreated)
	body := map[string]any{
		"topic_id": message.TopicID, "message_id": message.ID, "mode": "create_new",
		"reason":      "Authorize a new generation of work",
		"new_task":    map[string]string{"title": "successor", "acceptance": "Preserve the accepted version"},
		"predecessor": map[string]any{"task_id": "a", "expected_revision": 1, "submission_id": "s", "review_id": "r"},
	}
	request := func() []byte {
		t.Helper()
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		return payload
	}
	path := coordinationHTTPBase(g.ID) + "/requests"
	w := a.request(http.MethodPost, path, string(request()), "new-work")
	if w.Code != http.StatusConflict {
		t.Fatalf("explicit successor with no registered schema: got %d, want 409: %s", w.Code, w.Body)
	}
	delete(body, "predecessor")
	decoded[domain.AdmissionReceipt](t, a.request(http.MethodPost, path, string(request()), "new-work"), http.StatusCreated)
}

func openTaskSuccessorAPI(t *testing.T, path string) *groupAPI {
	t.Helper()
	a := openGroupAPI(t, path)
	local := platform.NewLocalUser(a.db)
	engine := coordination.New(a.db)
	routes := append(LocalUserRoutes(local), MessageRoutes(local)...)
	routes = append(routes, TaskRoutes(local)...)
	routes = append(routes, TaskLifecycleRoutes(local)...)
	routes = append(routes, CoordinationRoutes(platform.NewCoordinations(engine))...)
	routes = append(routes, DispatchRequestRoutes(platform.NewDispatchRequests(engine))...)
	a.handler = mustServer(t, DefaultAddress, routes...).Handler
	return a
}
