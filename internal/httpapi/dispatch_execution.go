package httpapi

import "net/http"

func (a dispatchRequestAPI) getDispatchExecution(w http.ResponseWriter, r *http.Request) {
	view, err := a.service.GetDispatchExecution(r.Context(), r.PathValue("group"), r.PathValue("task"), r.PathValue("request"))
	operationResult(w, http.StatusOK, view, err)
}
