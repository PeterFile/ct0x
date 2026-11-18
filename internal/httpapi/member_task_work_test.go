package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentworkspace/internal/store"
)

func TestFreshTaskWorkWriteUncertaintyOutranksBusinessCause(t *testing.T) {
	for _, op := range []string{"ack_work_input", "hand_back_work_input", "submit_result"} {
		for _, cause := range []error{store.ErrConflict, store.ErrMemberToolDenied, store.ErrInvalid, store.ErrNotFound, context.Canceled, context.DeadlineExceeded} {
			w := httptest.NewRecorder()
			freshTaskWorkWriteError(w, op, errors.Join(store.ErrTaskWorkMutationUnknown, &memberRoleHTTPSyntheticTxError{cause: cause, rolledBack: true}))
			if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), `"code":"internal_error"`) {
				t.Fatalf("%s with %v: %d %s", op, cause, w.Code, w.Body)
			}
		}
		w := httptest.NewRecorder()
		freshTaskWorkWriteError(w, op, &memberRoleHTTPSyntheticTxError{cause: store.ErrConflict, rolledBack: true})
		if w.Code != http.StatusConflict {
			t.Fatalf("confirmed %s conflict lost: %d", op, w.Code)
		}
	}
}
