package domain

type GroupCoordinatorBinding struct {
	GroupID  string `json:"group_id"`
	MemberID string `json:"member_id"`
	Revision int64  `json:"revision"`
}

type GroupDispatchPolicy struct {
	GroupID          string   `json:"group_id"`
	Revision         int64    `json:"revision"`
	AllowedWorkerIDs []string `json:"allowed_worker_ids"`
}
