package store

import (
	"reflect"
	"strings"
	"testing"
)

func TestWorkerArtifactFirstSQLAndTextTaskFailClosed(t *testing.T) {
	f, _, role := assignmentBoundWorker(t)
	workspaceOperationInstallSchema(t, f.s)
	for name, call := range map[string]func(*Store) error{
		"lookup": func(s *Store) error {
			_, _, err := s.LookupWorkerArtifact(ctx, role, WorkerArtifactInput{OperationID: "missing", Name: "成果"}, "k")
			return err
		},
		"publish": func(s *Store) error {
			_, err := s.PublishWorkerArtifact(ctx, role, WorkerArtifactInput{OperationID: "missing", Name: "成果"}, WorkerArtifactBlob{Hash: strings.Repeat("b", 64), Size: 17}, "k")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := assignmentQueryStore(t, f.path)
			if err := call(r.s); err == nil {
				t.Fatal("text Task published workspace artifact")
			}
			if len(r.queries) < 2 || !strings.HasPrefix(r.queries[0].query, "UPDATE turns SET claim_token=claim_token") || !strings.Contains(r.queries[1].query, memberToolBinding) {
				t.Fatalf("publication omitted same-transaction first SQL live role fence: %+v", r.queries)
			}
		})
	}
}

func TestWorkerArtifactStoreAPI(t *testing.T) {
	for _, name := range []string{"LookupWorkerArtifact", "PublishWorkerArtifact"} {
		if _, ok := reflect.TypeFor[*Store]().MethodByName(name); !ok {
			t.Fatalf("Store lacks %s: member publication must not use the human artifact entry", name)
		}
	}
}
