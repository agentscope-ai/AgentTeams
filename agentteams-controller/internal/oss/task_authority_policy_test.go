package oss

import "testing"

func TestMemberStorageCannotOverwriteAuthoritativeTaskMetadata(t *testing.T) {
	for _, manager := range []bool{false, true} {
		policy := NewMinIOAdminClient(Config{}).buildWorkerPolicy("alice", "storage", "team", manager)
		for _, key := range []string{"shared/tasks/*/meta.json", "teams/*/shared/tasks/*/meta.json", "agents/*/runtime/runtime.yaml", "agents/*/runtime/compat-config.json*"} {
			resource := "arn:aws:s3:::storage/" + key
			for _, action := range []string{"s3:PutObject", "s3:DeleteObject"} {
				denied := false
				for _, statement := range policy.Statement {
					if statement.Effect == "Deny" && stringSliceContains(statement.Resource, resource) && stringSliceContains(statement.Action, action) {
						denied = true
					}
				}
				if !denied {
					t.Errorf("manager=%v: direct %s of %s must be denied; control records are Controller-owned", manager, action, key)
				}
			}
		}
		for _, statement := range policy.Statement {
			if statement.Effect == "Deny" && (stringSliceContains(statement.Action, "s3:GetObject") || stringSliceContains(statement.Action, "s3:ListBucket")) {
				t.Errorf("metadata write protection must preserve current read/list scope: %+v", statement)
			}
		}
	}
}
