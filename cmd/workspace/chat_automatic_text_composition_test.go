package main

import (
	"database/sql"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
)

const (
	automaticTextDiscussion = "这次只讨论，不开工。去年同期列计入本季合计吗？"
	automaticTextWork       = "上一条只讨论；现在开始做这项纯文本交付。请在群里说明本季合计不包括去年同期列。验收是分别说明本季合计和同比对照两行口径。不改文件。"
	automaticTextSubmission = "本季合计：只汇总本季实际发生额。\n同比对照：去年同期列仅用于对照，不计入本季合计。"
)

func TestAutomaticChatTextCompositionUnregisteredFailsClosed(t *testing.T) {
	f := &coordinationCompositionFixture{native: memberToolsFixture(t), dir: t.TempDir()}
	base, stop, _, _ := startCoordinationComposition(t, 30*time.Second, f.args(t, "")...)
	var err error
	f.raw, err = sql.Open("sqlite", filepath.Join(f.dir, "workspace.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.raw.Close() })

	var src domain.Group
	coordinationCompositionHTTP(t, http.MethodPost, base+"/api/v1/groups", platform.GroupInput{Name: "未登记源群"}, "src-group", 201, &src)
	policyPath := base + "/api/v1/groups/" + src.ID + "/coordination/chat-policy?topic_id=t1"
	coordinationCompositionHTTP(t, http.MethodPut, policyPath, map[string]any{
		"enabled": true, "action": "text_submission", "max_admissions": 1, "expected_revision": 0, "automatic_text_dispatch": true,
	}, "auto-policy-unregistered", 400, nil)
	coordinationCompositionHTTP(t, http.MethodPost, base+"/api/v1/groups", platform.GroupInput{
		Name: "未登记导入群", ImportAuthorizationFromGroupID: src.ID,
	}, "import-unregistered", 400, nil)
	for _, table := range []string{"chat_policies", "tasks"} {
		if coordinationCompositionCount(t, f.raw, "SELECT count(*) FROM "+table) != 0 {
			t.Fatalf("unregistered fail-closed call wrote %s", table)
		}
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}
