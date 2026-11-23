package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
