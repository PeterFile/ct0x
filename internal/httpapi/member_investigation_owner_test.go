package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentworkspace/internal/platform"
	"agentworkspace/internal/store"
)

func TestInvestigationOwnerWriteUnknownFirst(t *testing.T) {
	for _, op := range []string{platform.DelegateWorkOperation, platform.HandbackWorkOperation} {
		t.Run(op, func(t *testing.T) {
			confirmed := &memberRoleHTTPSyntheticTxError{cause: store.ErrConflict, rolledBack: true}
			for _, tc := range []struct {
				err    error
				status int
				code   string
			}{
				{errors.Join(confirmed, store.ErrTaskWorkMutationUnknown, context.DeadlineExceeded), http.StatusInternalServerError, "internal_error"},
				{errors.Join(confirmed, &memberRoleHTTPSyntheticTxError{cause: store.ErrMemberToolDenied, unknown: true}), http.StatusInternalServerError, "internal_error"},
				{confirmed, http.StatusConflict, "conflict"},
				{store.ErrInvalid, http.StatusBadRequest, "invalid_input"},
			} {
				w := httptest.NewRecorder()
				taskWorkWriteError(w, op, tc.err)
				assertError(t, w.Code, w.Header(), w.Body.Bytes(), tc.status, tc.code)
				if body := w.Body.String(); strings.Contains(body, "PRIVATE-SQL") ||
					tc.status == http.StatusInternalServerError && (!strings.Contains(body, "结果尚未确认") || !strings.Contains(body, "原幂等键")) {
					t.Fatal("lost reconciliation evidence or leaked a Store cause")
				}
			}
		})
	}
}
