package store

import "context"

// ThreadDecisionHandoffInstalled is a read-only composition probe for the
// native adapter. It neither enables the consumer nor grants a scoped operation.
// Do not use a schema version or an empty queue as installation evidence.
func (s *Store) ThreadDecisionHandoffInstalled(ctx context.Context) (bool, error) {
	var installed bool
	err := s.db.QueryRowContext(ctx, `SELECT count(*)=5 FROM main.sqlite_schema
	 WHERE (type='table' AND name IN ('decision_requests','decision_request_versions','decision_answers','decision_thread_answers'))
	 OR (type='view' AND name='decision_thread_answerable')`).Scan(&installed)
	if err != nil {
		return false, decisionRequestError("验证待决事项接线所需的数据库记录失败", err)
	}
	return installed, nil
}
