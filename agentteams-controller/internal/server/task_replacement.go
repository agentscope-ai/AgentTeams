package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

func taskReplacementID(child map[string]any, review taskReview) string {
	identity, _ := json.Marshal([]any{child["main_task_id"], child["task_id"], review.ID})
	sum := sha256.Sum256(identity)
	return "exec-" + hex.EncodeToString(sum[:16])
}

func taskReviewArchived(main, child map[string]any) bool {
	attempts := coordinationMap(main, "task_attempts")
	archived := coordinationMap(attempts, coordinationString(child, "task_id"))
	var review taskReview
	encoded, _ := json.Marshal(child["task_review"])
	_ = json.Unmarshal(encoded, &review)
	return review.ID != "" && !review.Accepted && review.ReplacementSpec != "" &&
		archived["review_id"] == review.ID && archived["submission_id"] == review.SubmissionID &&
		archived["assigned_to"] == child["assigned_to"] && archived["status"] == child["status"]
}

func validateTaskReplacement(main, child map[string]any) error {
	if main["task_plan"] == nil || main["status"] != "in_progress" || !taskReviewMembership(main, child) {
		return fmt.Errorf("replacement requires the active managed main-task plan")
	}
	// A rejected candidate cannot have released a consumer. Never silently
	// invalidate already running or accepted work if the saved graph disagrees.
	nodes, _ := main["tasks"].([]any)
	for _, item := range nodes {
		node, _ := item.(map[string]any)
		var dependencies []string
		encoded, _ := json.Marshal(node["depends_on"])
		if json.Unmarshal(encoded, &dependencies) != nil {
			return fmt.Errorf("invalid planned dependencies")
		}
		for _, dependency := range dependencies {
			if dependency == child["task_id"] && node["status"] != "planned" {
				return fmt.Errorf("replacement cannot change an activated consumer")
			}
		}
	}
	return nil
}

func applyTaskReplacement(main, child map[string]any, review taskReview) error {
	if err := validateTaskReplacement(main, child); err != nil {
		return err
	}
	if len(review.ReplacementNodes) > 0 {
		return applyTaskReplacementPlan(main, child, review)
	}
	oldID, newID := coordinationString(child, "task_id"), taskReplacementID(child, review)
	nodes, _ := main["tasks"].([]any)
	attempts := coordinationMap(main, "task_attempts")
	if attempts == nil {
		attempts = map[string]any{}
	}
	for i, item := range nodes {
		node, _ := item.(map[string]any)
		if node["task_id"] == oldID {
			archived, replacement := map[string]any{}, map[string]any{}
			for key, value := range node {
				archived[key], replacement[key] = value, value
			}
			archived["status"], archived["review_id"], archived["submission_id"] = review.Status, review.ID, review.SubmissionID
			attempts[oldID] = archived
			replacement["task_id"], replacement["status"] = newID, "planned"
			replacement["revision_of"], replacement["revision_spec"] = oldID, review.ReplacementSpec
			replacement["spec"] = coordinationString(node, "spec") + "\n\n# Revision requirements\n" + review.ReplacementSpec
			if class := coordinationString(child, "decision_class"); class != "" {
				replacement["decision_class"] = class
			}
			delete(replacement, "review_id")
			delete(replacement, "submission_id")
			nodes[i] = replacement
		}
		var dependencies []string
		encoded, _ := json.Marshal(node["depends_on"])
		_ = json.Unmarshal(encoded, &dependencies)
		for j, dependency := range dependencies {
			if dependency == oldID {
				dependencies[j] = newID
			}
		}
		node["depends_on"] = dependencies
	}
	main["tasks"], main["task_attempts"] = nodes, attempts
	return nil
}

// Replace only the failed node. Its immutable ancestors and all prior attempts
// stay intact; consumers receive all corrected outputs and wait for the full DAG.
func applyTaskReplacementPlan(main, child map[string]any, review taskReview) error {
	plan := taskPlanRequest{Nodes: append([]taskPlanNode(nil), review.ReplacementNodes...)}
	if err := normalizeTaskPlan(&plan); err != nil {
		return err
	}
	nodes, _ := main["tasks"].([]any)
	if len(nodes)+len(plan.Nodes)-1 > 50 {
		return fmt.Errorf("replacement exceeds execution node limit")
	}
	oldID := coordinationString(child, "task_id")
	ids := map[string]string{}
	for i, node := range plan.Nodes {
		id := taskReplacementID(child, review)
		if i != 0 {
			identity, _ := json.Marshal([]any{oldID, review.ID, node.ID})
			sum := sha256.Sum256(identity)
			id = "exec-" + hex.EncodeToString(sum[:16])
		}
		ids[node.ID] = id
	}
	attempts := coordinationMap(main, "task_attempts")
	if attempts == nil {
		attempts = map[string]any{}
	}
	rebuilt := []any{}
	for _, item := range nodes {
		node := item.(map[string]any)
		if node["task_id"] != oldID {
			rebuilt = append(rebuilt, node)
			continue
		}
		archived := map[string]any{}
		for key, value := range node {
			archived[key] = value
		}
		archived["status"], archived["review_id"], archived["submission_id"] = review.Status, review.ID, review.SubmissionID
		attempts[oldID] = archived
		for _, correction := range plan.Nodes {
			replacement := map[string]any{}
			for key, value := range node {
				replacement[key] = value
			}
			dependencies := []string{}
			for _, dependency := range correction.DependsOn {
				dependencies = append(dependencies, ids[dependency])
			}
			if len(dependencies) == 0 {
				encoded, _ := json.Marshal(node["depends_on"])
				_ = json.Unmarshal(encoded, &dependencies)
			}
			replacement["task_id"], replacement["step_id"], replacement["status"] = ids[correction.ID], "revision-"+review.ID[:8]+"-"+correction.ID, "planned"
			replacement["title"], replacement["assigned_to"], replacement["depends_on"] = correction.Title, correction.AssignedTo, dependencies
			replacement["revision_of"], replacement["revision_spec"] = oldID, review.ReplacementSpec
			replacement["spec"] = coordinationString(main, "spec") + "\n\n# Correction requirements\n" + review.ReplacementSpec + "\n\n# Your assignment\n" + correction.Spec
			if class := coordinationString(child, "decision_class"); class != "" {
				replacement["decision_class"] = class
			}
			delete(replacement, "review_id")
			delete(replacement, "submission_id")
			rebuilt = append(rebuilt, replacement)
		}
	}
	for _, item := range rebuilt {
		node := item.(map[string]any)
		var dependencies []string
		encoded, _ := json.Marshal(node["depends_on"])
		_ = json.Unmarshal(encoded, &dependencies)
		updated := map[string]bool{}
		for _, dependency := range dependencies {
			if dependency == oldID {
				for _, corrected := range ids {
					updated[corrected] = true
				}
			} else {
				updated[dependency] = true
			}
		}
		dependencies = []string{}
		for dependency := range updated {
			dependencies = append(dependencies, dependency)
		}
		sort.Strings(dependencies)
		node["depends_on"] = dependencies
	}
	main["tasks"], main["task_attempts"] = rebuilt, attempts
	return nil
}
