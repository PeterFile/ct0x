package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"agentworkspace/internal/domain"
)

// admissionReceiptInTx never joins the live Task or the mutable coordination projection.
func admissionReceiptInTx(ctx context.Context, tx *sql.Tx, groupID, decisionID string) (domain.AdmissionReceipt, error) {
	var receipt domain.AdmissionReceipt
	d, m := &receipt.Decision, &receipt.Mandate
	var decisionStamp, mandateStamp, dependencies string
	if err := tx.QueryRowContext(ctx, "SELECT "+admissionDecisionColumns+" FROM routing_decisions WHERE group_id=? AND id=?", groupID, decisionID).Scan(
		&d.ID, &d.TriggerID, &d.GroupID, &d.TopicID, &d.Kind, &d.ActorID, &d.PolicyRevision, &d.Reason,
		&d.TaskID, &d.TaskRevisionAtAdmission, &d.MandateID, &decisionStamp); err != nil {
		return domain.AdmissionReceipt{}, err
	}
	if err := tx.QueryRowContext(ctx, "SELECT "+admissionMandateColumns+" FROM mandates WHERE group_id=? AND decision_id=? AND id=?", groupID, d.ID, d.MandateID).Scan(
		&m.ID, &m.DecisionID, &m.OriginTriggerID, &m.GroupID, &m.TopicID, &m.TaskID, &m.TaskRevisionAtAdmission,
		&m.AuthorityKind, &m.AuthorizedBy, &m.Strength, &m.PolicyRevision, &m.Scope.Objective, &m.Scope.Description,
		&m.Scope.Acceptance, &m.Scope.AssigneeID, &dependencies, &mandateStamp); err != nil {
		return domain.AdmissionReceipt{}, err
	}
	var err error
	d.CreatedAt, err = time.Parse(timestampLayout, decisionStamp)
	if err != nil {
		return domain.AdmissionReceipt{}, err
	}
	m.CreatedAt, err = time.Parse(timestampLayout, mandateStamp)
	if err != nil {
		return domain.AdmissionReceipt{}, err
	}
	if err := json.Unmarshal([]byte(dependencies), &m.Scope.DependencyIDs); err != nil {
		return domain.AdmissionReceipt{}, err
	}
	receipt.Task = domain.TaskRef{ID: d.TaskID, GroupID: d.GroupID, RevisionAtAdmission: d.TaskRevisionAtAdmission}
	return receipt, nil
}
