package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTaskHierarchyReplanAndProjection(t *testing.T) {
	tasks, err := normalizeReplanTasks([]json.RawMessage{
		json.RawMessage(`{"taskId":"p-main","title":"交付筛选功能"}`),
		json.RawMessage(`{"taskId":"p-child","parentTaskId":"p-main"}`),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tasks[1].ParentTaskID != "p-main" {
		t.Fatalf("lost parent: %+v", tasks[1])
	}
	replanned, err := normalizeReplanTasks([]json.RawMessage{json.RawMessage(`{"taskId":"p-child"}`)}, map[string]projectTaskMeta{"p-child": tasks[1]})
	if err != nil || replanned[0].ParentTaskID != "p-main" {
		t.Fatalf("replan lost parent: %+v %v", replanned, err)
	}
	cleared, err := normalizeReplanTasks([]json.RawMessage{json.RawMessage(`{"taskId":"p-child","parent_task_id":""}`)}, map[string]projectTaskMeta{"p-child": tasks[1]})
	if err != nil || cleared[0].ParentTaskID != "" {
		t.Fatalf("cannot clear parent: %+v %v", cleared, err)
	}
	response := (&ProjectHandler{}).buildWorkflow(&projectMeta{ProjectID: "p", Status: "active", Tasks: tasks}, "", false, time.Time{})
	if response.Nodes[1].ParentTaskID != "p-main" {
		t.Fatalf("projection lost parent: %+v", response.Nodes)
	}
	if len(response.Next) != 1 || response.Next[0] != "p-child" {
		t.Fatalf("summary must not dispatch: %v", response.Next)
	}
}

func TestTaskHierarchyValidation(t *testing.T) {
	tasks := []projectTaskMeta{{TaskID: "main", Status: "completed"}, {TaskID: "child", ParentTaskID: "main", Status: "planned"}}
	if err := validateTaskGraph(tasks); err == nil || !strings.Contains(err.Error(), "unfinished subtasks") {
		t.Fatalf("premature completion accepted: %v", err)
	}
	tasks[0].Status = "planned"
	tasks[0].ParentTaskID = "child"
	if err := validateTaskGraph(tasks); err == nil || !strings.Contains(err.Error(), "parent cycle") {
		t.Fatalf("parent cycle accepted: %v", err)
	}
	tasks[0].ParentTaskID = "missing"
	if err := validateTaskGraph(tasks); err == nil || !strings.Contains(err.Error(), "unknown parent") {
		t.Fatalf("unknown parent accepted: %v", err)
	}
}
