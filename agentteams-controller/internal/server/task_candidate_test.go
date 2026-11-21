package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestTaskCandidatePublicViewAndImmutableReportRead(t *testing.T) {
	h, store, leader, submissionID := taskReviewFixture(t)
	r := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/t1", nil)
	r.SetPathValue("id", "t1")
	w := httptest.NewRecorder()
	h.GetTaskResource(w, withCaller(r, leader))
	var response struct {
		Task taskResource `json:"task"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Task.Candidate == nil {
		t.Fatalf("candidate view: %d %s", w.Code, w.Body.String())
	}
	c := response.Task.Candidate
	if c.ID != submissionID || c.Report != "Verified candidate" || c.ReportTruncated || len(c.Artifacts) != 1 || c.Artifacts[0].Name != "report.md" || c.ReportFile.ArtifactID != "report-"+submissionID {
		t.Fatalf("wrong immutable candidate: %+v", c)
	}
	if strings.Contains(w.Body.String(), "teams/alpha/shared/") || strings.Contains(w.Body.String(), "process-1") {
		t.Fatalf("private storage/execution metadata leaked: %s", w.Body.String())
	}
	read := func(version string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/t1/files?artifactId="+url.QueryEscape(c.ReportFile.ArtifactID)+"&version="+version, nil)
		r.SetPathValue("id", "t1")
		w := httptest.NewRecorder()
		h.GetTaskFiles(w, withCaller(r, leader))
		return w
	}
	if got := read(c.ReportFile.Version); got.Code != 200 || got.Body.String() != c.Report {
		t.Fatalf("report read: %d %s", got.Code, got.Body.String())
	}
	if got := read("stale"); got.Code != 409 {
		t.Fatalf("wrong report version accepted: %d %s", got.Code, got.Body.String())
	}
	key := "teams/alpha/shared/tasks/t1/meta.json"
	data, _ := store.GetObject(t.Context(), key)
	var record map[string]any
	_ = json.Unmarshal(data, &record)
	record["submission_id"] = "different-submission"
	if publicTaskCandidate(record) != nil {
		t.Fatal("mismatched candidate exposed")
	}
	putTask(store, key, record)
	if got := read(c.ReportFile.Version); got.Code != 503 {
		t.Fatalf("inconsistent report reference accepted: %d %s", got.Code, got.Body.String())
	}
}

func TestTaskCandidatePreviewIsBoundedAndDoesNotUseCurrentHeads(t *testing.T) {
	h, store, worker, artifact := taskSubmitFixture(t)
	report := strings.Repeat("验", 4100)
	body, _ := json.Marshal(taskSubmitRequest{Summary: "Summary", Report: report, Artifacts: []string{artifact}})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/t1/submit", strings.NewReader(string(body)))
	r.SetPathValue("id", "t1")
	r.Header.Set("X-AgentTeams-Process-ID", "process-1")
	w := httptest.NewRecorder()
	h.SubmitTask(w, withCaller(r, worker))
	if w.Code != 200 {
		t.Fatalf("submit: %d %s", w.Code, w.Body.String())
	}
	data, _ := store.GetObject(t.Context(), "teams/alpha/shared/tasks/t1/meta.json")
	var record map[string]any
	_ = json.Unmarshal(data, &record)
	c := publicTaskCandidate(record)
	if c == nil || !c.ReportTruncated || c.Report != strings.Repeat("验", 4096)+"…" || c.ReportFile.Size != int64(len(report)) {
		t.Fatal("invalid bounded candidate preview")
	}
	var restored strings.Builder
	for offset := int64(0); ; {
		query := url.Values{"artifactId": {c.ReportFile.ArtifactID}, "version": {c.ReportFile.Version}, "view": {"text"}, "offset": {strconv.FormatInt(offset, 10)}, "limit": {"1024"}}
		r := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/t1/files?"+query.Encode(), nil)
		r.SetPathValue("id", "t1")
		w := httptest.NewRecorder()
		h.GetTaskFiles(w, withCaller(r, worker))
		var page struct {
			Text       string `json:"text"`
			Truncated  bool   `json:"truncated"`
			NextOffset int64  `json:"nextOffset"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || page.NextOffset <= offset {
			t.Fatalf("report text page: %d %s", w.Code, w.Body.String())
		}
		restored.WriteString(page.Text)
		if !page.Truncated {
			break
		}
		offset = page.NextOffset
	}
	if restored.String() != report {
		t.Fatal("paged report changed UTF-8 content")
	}
	index := record["task_files"].(map[string]any)
	delete(index["artifacts"].(map[string]any), artifact)
	if publicTaskCandidate(record) != nil {
		t.Fatal("missing exact submitted artifact was replaced by another reference")
	}
}
