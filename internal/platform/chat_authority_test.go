package platform

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"agentworkspace/internal/contextpack"
	"agentworkspace/internal/store"
)

func TestChatAuthorityPreparationContextHasCommandSource(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	group, err := db.CreateGroup(ctx, store.CreateGroupInput{Name: "source"})
	if err != nil {
		t.Fatal(err)
	}
	message, err := NewLocalUser(db).CreateMessage(ctx, group.ID, MessageInput{Content: "讨论三种状态，不执行工作。"}, "actual-source")
	if err != nil {
		t.Fatal(err)
	}
	in := contextpack.Input{Scope: contextpack.Scope{GroupID: group.ID, ConversationID: "encoder-test"}}
	if err := appendPreparationMessages(&in, "", message, nil); err != nil {
		t.Fatal(err)
	}
	var header struct {
		SourceKind string `json:"source_kind"`
		SourceID   string `json:"source_id"`
	}
	if len(in.Pending) != 1 || json.Unmarshal([]byte(in.Pending[0].Content), &header) != nil || header.SourceKind != "local_user_chat" || header.SourceID == "" {
		t.Fatalf("preparation hides actual command provenance: %+v", in.Pending)
	}
}
