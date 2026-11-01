package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentworkspace/internal/domain"
	"agentworkspace/internal/platform"
	"agentworkspace/internal/provider"
	"agentworkspace/internal/store"
)

func TestRunTaskAndHistoryRoutes(t *testing.T) {
	const index = "<!doctype html><title>Task and history entry</title>"
	webDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDir, "index.html"), []byte(index), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--listen", "127.0.0.1:0", "--data-dir", t.TempDir(), "--web-dir", webDir}
	base, stop := startRunWithSearch(t, provider.Search{}, args...)
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	getJSON := func(t *testing.T, path string, status int, result any) {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Accept", "text/html")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != status || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") || !json.Valid(body) {
			t.Fatalf("GET %s: status=%d type=%q body=%s err=%v", path, response.StatusCode, response.Header.Get("Content-Type"), body, err)
		}
		if status >= 400 && !chinese(string(body)) {
			t.Fatalf("GET %s: error must remain Chinese JSON: %s", path, body)
		}
		if result != nil {
			if err := json.Unmarshal(body, result); err != nil {
				t.Fatal(err)
			}
		}
	}

	var group, other domain.Group
	executionJSON(t, "POST", base+"/api/v1/groups", platform.GroupInput{Name: "任务与历史接线"}, &group)
	executionJSON(t, "POST", base+"/api/v1/groups", platform.GroupInput{Name: "隔离群组"}, &other)
	groupPath := "/api/v1/groups/" + group.ID
	otherPath := "/api/v1/groups/" + other.ID
	var member domain.Member
	executionJSON(t, "POST", base+groupPath+"/members", platform.MemberInput{Name: "未绑定成员", Role: "读取公开历史"}, &member)
	const query = "历史接线 100%_"
	messageInput := platform.MessageInput{Content: query + " 公开消息", RecipientIDs: []string{}, TopicID: "topic"}
	var message, foreign domain.Message
	executionJSON(t, "POST", base+groupPath+"/messages", messageInput, &message)
	messageInput.Content = query + " 外群消息"
	executionJSON(t, "POST", base+otherPath+"/messages", messageInput, &foreign)
	if message.ID == "" || message.GroupID != group.ID || message.Content != query+" 公开消息" || message.Deliveries == nil || len(message.Deliveries) != 0 || len(foreign.Deliveries) != 0 {
		t.Fatalf("public-only fixtures must persist without model deliveries: %+v %+v", message, foreign)
	}

	var task domain.Task
	t.Run("tasks", func(t *testing.T) {
		input := platform.TaskInput{Title: "持久任务", Description: "验证命令路由", Acceptance: "创建后可查询", AssigneeID: member.ID}
		executionJSON(t, "POST", base+groupPath+"/tasks", input, &task)
		if task.ID == "" || task.GroupID != group.ID || task.Title != input.Title || task.Description != input.Description || task.Acceptance != input.Acceptance || task.AssigneeID != member.ID || task.Status != domain.TaskStatusOpen {
			t.Fatalf("task route did not persist the requested task: %+v", task)
		}
		var detail domain.Task
		getJSON(t, groupPath+"/tasks/"+task.ID, http.StatusOK, &detail)
		var page struct {
			Items      []domain.Task `json:"items"`
			NextCursor string        `json:"next_cursor"`
		}
		getJSON(t, groupPath+"/tasks", http.StatusOK, &page)
		if !reflect.DeepEqual(detail, task) || !reflect.DeepEqual(page.Items, []domain.Task{task}) || page.NextCursor != "" {
			t.Fatalf("task list/detail differ from creation: detail=%+v page=%+v", detail, page)
		}
		getJSON(t, otherPath+"/tasks/"+task.ID, http.StatusNotFound, nil)
	})

	t.Run("history", func(t *testing.T) {
		var page store.HistoryPage
		getJSON(t, groupPath+"/history?q="+url.QueryEscape(query), http.StatusOK, &page)
		var detail domain.Message
		getJSON(t, groupPath+"/history/"+message.ID, http.StatusOK, &detail)
		if !reflect.DeepEqual(page.Items, []domain.Message{message}) || page.NextCursor != "" || !reflect.DeepEqual(detail, message) {
			t.Fatalf("history search/read lost content or leaked a foreign message: page=%+v detail=%+v", page, detail)
		}
		getJSON(t, otherPath+"/history/"+message.ID, http.StatusNotFound, nil)
	})

	t.Run("html_navigation", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodGet, base+"/groups/"+group.ID+"/tasks", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Accept", "text/html")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/html") || string(body) != index {
			t.Fatalf("HTML navigation: status=%d type=%q body=%s err=%v", response.StatusCode, response.Header.Get("Content-Type"), body, err)
		}
	})

	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if t.Failed() {
		return
	}
	base, stop = startRunWithSearch(t, provider.Search{}, args...)
	t.Run("reopened", func(t *testing.T) {
		var taskDetail domain.Task
		getJSON(t, groupPath+"/tasks/"+task.ID, http.StatusOK, &taskDetail)
		var messageDetail domain.Message
		getJSON(t, groupPath+"/history/"+message.ID, http.StatusOK, &messageDetail)
		if !reflect.DeepEqual(taskDetail, task) || !reflect.DeepEqual(messageDetail, message) {
			t.Fatalf("command restart lost task/history receipts: task=%+v message=%+v", taskDetail, messageDetail)
		}
	})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}
