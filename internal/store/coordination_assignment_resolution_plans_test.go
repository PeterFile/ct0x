package store

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

func TestCoordinationAssignmentResolutionV14ActualGuardPlans(t *testing.T) {
	s, _ := resolutionStore(t)
	guards := messageSequenceRows(t, s.db, `SELECT name,sql FROM sqlite_master WHERE type='trigger' AND
	 (tbl_name IN ('turns','deliveries') OR name IN ('coordination_dispatch_receipt_source',
	 'coordination_assignment_receipt_source','coordination_decisions_insert','coordination_assignments_decision_insert',
	 'coordination_dispatch_state_insert','coordination_dispatch_state_update')) ORDER BY name`)
	predicate := regexp.MustCompile(`(?s)SELECT RAISE\(ABORT, '[^']*'\) WHERE\s+(.*?);`)
	fields := regexp.MustCompile(`\b(NEW|OLD)\.[a-z_]+`)
	var scans []string
	for _, guard := range guards {
		matches := predicate.FindAllStringSubmatch(guard[1].(string), -1)
		if _, tail, found := strings.Cut(guard[1].(string), "\nWHEN "); found {
			when, _, _ := strings.Cut(tail, "\nBEGIN")
			matches = append(matches, []string{"", when})
		}
		if len(matches) == 0 {
			t.Fatalf("actual guard query was not inspected: %s", guard[0])
		}
		for i, match := range matches {
			var args []any
			seen := map[string]bool{}
			query := fields.ReplaceAllStringFunc("SELECT 1 WHERE "+match[1], func(field string) string {
				name := strings.ReplaceAll(field, ".", "_")
				if !seen[name] {
					seen[name] = true
					var value any = "missing"
					if strings.HasSuffix(field, ".state") {
						value = "starting"
					}
					args = append(args, sql.Named(name, value))
				}
				return ":" + name
			})
			t.Logf("actual final guard=%s predicate=%d", guard[0], i)
			plan := resolutionPlan(t, s, query, args...)
			for _, line := range strings.Split(plan, "\n") {
				if strings.HasPrefix(line, "SCAN ") && line != "SCAN CONSTANT ROW" || strings.Contains(line, "TEMP B-TREE") {
					scans = append(scans, fmt.Sprintf("%s[%d]: %s", guard[0], i, line))
				}
			}
		}
	}
	if len(scans) > 0 {
		t.Fatalf("declare exact extra-scale plan before any role-history measurement; scans/sorts in final guard queries:\n%s", strings.Join(scans, "\n"))
	}
}

func TestCoordinationAssignmentResolutionV14LegacyTaskLookupPlans(t *testing.T) {
	// Preserve the frozen N2a exact-scope query without adding another write index.
	const legacy = `SELECT s.request_id,s.generation,s.next_attempt_at FROM coordination_dispatch_state s
	 WHERE s.group_id=? AND s.task_id=? AND s.status IN ('pending','enqueued','held')`
	// This is the existing N2b production query, including its differently ordered IN list.
	const current = `SELECT EXISTS(SELECT 1 FROM coordination_dispatch_state
	 WHERE task_id=? AND group_id=? AND status IN ('pending','held','enqueued'))`
	for _, version := range []string{"v13", "v14"} {
		t.Run(version, func(t *testing.T) {
			var s *Store
			if version == "v13" {
				s, _ = assignmentV13Store(t)
			} else {
				s, _ = resolutionStore(t)
			}
			// Log both actual shapes; do not relabel an inherited N2b scan as a v14 regression.
			resolutionPlan(t, s, current, "missing-task", "missing-group")
			plan := resolutionPlan(t, s, legacy, "missing-group", "missing-task")
			if strings.Contains(plan, "SCAN s") || !strings.Contains(plan, "coordination_dispatch_task_unfinished") {
				t.Fatalf("v14 must preserve the proven N2a Task lookup plan: %s", plan)
			}
		})
	}
}
