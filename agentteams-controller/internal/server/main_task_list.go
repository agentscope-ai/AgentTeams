package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
)

// Main-task plans are authoritative Task records, not synthetic Projects.
// Expand planned children too: they may not have execution records until ready.
func (h *ProjectHandler) appendMainTasks(ctx context.Context, tasks []teamTask, prefixes []string, accessible map[string]bool, teamFilter string) ([]teamTask, error) {
	positions := map[string]int{}
	for i, t := range tasks {
		positions[t.TeamID+"\x00"+t.TaskID] = i
	}
	for _, prefix := range prefixes {
		if accessible != nil && !accessible[prefix] {
			continue
		}
		team := teamFromPrefix(prefix)
		if teamFilter != "" && team != teamFilter {
			continue
		}
		taskPrefix := strings.TrimSuffix(prefix, "projects/") + "tasks/"
		children, err := h.oss.ListObjects(ctx, taskPrefix)
		if err != nil {
			return nil, err
		}
		records := map[string]map[string]any{}
		type readResult struct {
			record map[string]any
			err    error
		}
		results := make([]readResult, len(children))
		var wg sync.WaitGroup
		sem := make(chan struct{}, 8)
		for i, child := range children {
			key, ok := metaKeyFromListResult(taskPrefix, child)
			if parts := strings.Split(child, "/"); len(parts) == 2 && parts[1] == "meta.json" {
				key, ok = taskPrefix+child, true
			}
			if !ok {
				continue
			}
			sem <- struct{}{}
			wg.Add(1)
			go func(i int, key string) {
				defer wg.Done()
				defer func() { <-sem }()
				b, err := h.oss.GetObject(ctx, key)
				if errors.Is(err, os.ErrNotExist) {
					return
				}
				if err != nil {
					results[i].err = err
					return
				}
				results[i].err = json.Unmarshal(b, &results[i].record)
			}(i, key)
		}
		wg.Wait()
		for _, result := range results {
			if result.err != nil {
				return nil, fmt.Errorf("read task metadata: %w", result.err)
			}
			id := coordinationString(result.record, "task_id")
			if isSafeTaskID(id) {
				records[id] = result.record
			}
		}
		for id, main := range records {
			if coordinationString(main, "task_kind") != "main" {
				continue
			}
			project := coordinationString(main, "project_id")
			add := func(record map[string]any, parent string) {
				nodeBytes, _ := json.Marshal(record)
				var node projectTaskMeta
				_ = json.Unmarshal(nodeBytes, &node)
				if node.Title == "" {
					node.Title = coordinationString(record, "task_title")
				}
				if node.Title == "" {
					node.Title = node.TaskID
				}
				node.Spec = ""
				if parent != "" {
					node.ParentTaskID = parent
				}
				row := teamTask{projectTaskMeta: node, ProjectID: project, TeamID: team, MainTaskID: id, TaskKind: coordinationString(record, "task_kind"), RoomID: coordinationString(main, "room_id"), SourceRoomID: coordinationString(main, "source_room_id"), CreatedAt: coordinationString(record, "created_at")}
				if row.CreatedAt == "" {
					row.CreatedAt = coordinationString(record, "assigned_at")
				}
				k := team + "\x00" + node.TaskID
				if pos, ok := positions[k]; ok {
					tasks[pos] = row
				} else {
					positions[k] = len(tasks)
					tasks = append(tasks, row)
				}
			}
			add(main, "")
			nodes, _ := main["tasks"].([]any)
			for _, item := range nodes {
				node, _ := item.(map[string]any)
				childID := coordinationString(node, "task_id")
				if !isSafeTaskID(childID) {
					return nil, fmt.Errorf("invalid subtask ID")
				}
				merged := map[string]any{}
				for k, v := range node {
					merged[k] = v
				}
				if execution := records[childID]; execution != nil {
					if coordinationString(execution, "main_task_id") != id {
						return nil, fmt.Errorf("subtask ownership mismatch")
					}
					for k, v := range execution {
						merged[k] = v
					}
				}
				add(merged, id)
			}
		}
	}
	return tasks, nil
}
