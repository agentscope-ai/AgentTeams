package server

import (
	"net/http"
	"testing"
)

func TestHumanChangesStorageOutage(t *testing.T) {
	for _, tc := range []struct{ name, method, since string }{
		{"first-list", "ListObjectsRecursive", "since=v1:"},
		{"cursor-validation-list", "ListObjectsRecursive", "since=v1:00000000000000000001"},
		{"entry-read", "GetObject", "since=v1:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, store, _ := newHumanFilesRig(t)
			if r := hfPutFile(t, h, "alice", "a.md", hfSelf(), "data"); r.Code != http.StatusOK {
				t.Fatal(r.Body.String())
			}
			if tc.name == "cursor-validation-list" {
				tc.since = "since=v1:" + hfChangeIDs(t, store, "alice")[0]
			}
			h.oss = newFaultOSS(t, store, humanChangesPrefix("alice"), tc.method)
			r, _ := hfGetChanges(t, h, "alice", tc.since, hfSelf())
			assertStorageUnavailable(t, r, tc.name)
		})
	}
}
