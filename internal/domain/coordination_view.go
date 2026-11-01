package domain

// TaskCoordinationView pairs current authority with its exact immutable admission.
// The live Task lifecycle is deliberately a separate read.
type TaskCoordinationView struct {
	Coordination TaskCoordination `json:"coordination"`
	Admission    AdmissionReceipt `json:"admission"`
}
