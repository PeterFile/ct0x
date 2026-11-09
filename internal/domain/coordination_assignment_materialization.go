package domain

// AssignmentMaterialization records durable intent, not delivery acceptance or execution.
type AssignmentMaterialization struct {
	Disposition string                     `json:"disposition"`
	Generation  int64                      `json:"generation"`
	Delivery    *AssignmentDeliveryReceipt `json:"delivery"`
	HeldReason  string                     `json:"held_reason"`
}
