package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func assistantCall(h *AssistantHandler, caller *authpkg.CallerIdentity, method, resource string, payload any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(payload)
	r := withCaller(httptest.NewRequest(method, "/api/v1/assistant/"+resource, strings.NewReader(string(b))), caller)
	w := httptest.NewRecorder()
	if strings.HasPrefix(resource, "business") {
		h.Business(w, r)
	} else if strings.HasPrefix(resource, "messages") {
		h.Send(w, r)
	} else {
		h.Context(w, r)
	}
	return w
}

func TestAssistantContextUsesStorageCASAcrossControllers(t *testing.T) {
	coord, caller, route := assistantFixture(t, "qwenpaw")
	h1, h2 := NewAssistantHandler(coord.deps), NewAssistantHandler(coord.deps)
	first := assistantCall(h1, caller, "GET", "context?team=alpha", nil)
	var view map[string]any
	_ = json.Unmarshal(first.Body.Bytes(), &view)
	if first.Code != 200 || view["revision"] == nil {
		t.Fatal(first.Code, first.Body.String())
	}
	input := map[string]any{"owner": route.owner, "revision": view["revision"], "enabled": false}
	if w := assistantCall(h1, caller, "PUT", "context?team=alpha", input); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	input["enabled"] = true
	if w := assistantCall(h2, caller, "PUT", "context?team=alpha", input); w.Code != 409 {
		t.Fatal("stale controller overwrote settings", w.Code, w.Body.String())
	}
	state, _ := h2.contextState(t.Context(), route)
	if state.Enabled {
		t.Fatal("disabled setting lost")
	}
}

func TestAssistantManagerUsesRegisteredHumanRoomsAndCurrentPermissions(t *testing.T) {
	coord, _ := coordinationFixture(t)
	for _, name := range []string{"alice", "bob"} {
		human := &v1.Human{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID("uid-" + name)},
			Spec: v1.HumanSpec{PermissionLevel: 1}, Status: v1.HumanStatus{MatrixUserID: "@" + name + ":test", ManagerRooms: map[string]string{"default": "!" + name + ":test"}}}
		if err := coord.deps.Client.Create(t.Context(), human); err != nil {
			t.Fatal(err)
		}
	}
	h := NewAssistantHandler(coord.deps)
	manager := &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "default"}
	a, err := h.routeForRecipient(t.Context(), manager, "", "default", "alice")
	if err != nil || a.room != "!alice:test" {
		t.Fatal(a, err)
	}
	b, err := h.routeForRecipient(t.Context(), manager, "", "default", "bob")
	if err != nil || b.room != "!bob:test" || a.prefix == b.prefix {
		t.Fatal(b, err)
	}
	// An authenticated administrator cannot select another user's private context.
	user := &authpkg.CallerIdentity{Role: authpkg.RoleAdmin, Username: "alice", MatrixUserID: "@alice:test"}
	if _, err = h.routeForRecipient(t.Context(), user, "", "default", "bob"); err == nil {
		t.Fatal("cross-user context exposed")
	}
	var human v1.Human
	_ = coord.deps.Client.Get(t.Context(), client.ObjectKey{Name: "alice", Namespace: "default"}, &human)
	human.Spec.PermissionLevel = 2
	_ = coord.deps.Client.Update(t.Context(), &human)
	if _, err = h.routeForRecipient(t.Context(), manager, "", "default", "alice"); err == nil {
		t.Fatal("demoted L1 retained assistant route")
	}
}

func TestAssistantAdminHandoffKeepsBusinessAndIsolatesPrivateContext(t *testing.T) {
	coord, caller, route := assistantFixture(t, "qwenpaw")
	h := coord.assistant
	view := assistantCall(h, caller, "GET", "business?team=alpha", nil)
	var business map[string]any
	_ = json.Unmarshal(view.Body.Bytes(), &business)
	if w := assistantCall(h, caller, "PUT", "business?team=alpha", map[string]any{"owner": route.owner, "revision": business["revision"], "summary": "shared business progress"}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	private := assistantCall(h, caller, "GET", "context?team=alpha", nil)
	var personal map[string]any
	_ = json.Unmarshal(private.Body.Bytes(), &personal)
	if w := assistantCall(h, caller, "PUT", "context?team=alpha", map[string]any{"owner": route.owner, "revision": personal["revision"], "summary": "alice private interests"}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var team v1.Team
	_ = coord.deps.Client.Get(t.Context(), client.ObjectKey{Name: "alpha", Namespace: "default"}, &team)
	team.Spec.Admin = &v1.TeamAdminSpec{Name: "bob", MatrixUserID: "@bob:test"}
	team.Status.LeaderDMRoomID = "!bob-private:test"
	_ = coord.deps.Client.Update(t.Context(), &team)
	next := assistantCall(h, caller, "GET", "context?team=alpha", nil)
	if next.Code != 200 || strings.Contains(next.Body.String(), "alice private") || !strings.Contains(next.Body.String(), "shared business progress") {
		t.Fatal(next.Code, next.Body.String())
	}
	if w := assistantCall(h, caller, "PUT", "context?team=alpha", map[string]any{"owner": route.owner, "revision": "", "summary": "old executor"}); w.Code != 409 {
		t.Fatal("old Admin execution wrote into new user scope", w.Code)
	}
}

func TestAssistantConcurrentSendClaimsOneReceiptAcrossControllers(t *testing.T) {
	coord, caller, route := assistantFixture(t, "qwenpaw")
	h1, h2 := NewAssistantHandler(coord.deps), NewAssistantHandler(coord.deps)
	entered, release := make(chan struct{}), make(chan struct{})
	var sends atomic.Int32
	send := func(context.Context, string, string, string, string, map[string]any) (string, error) {
		if sends.Add(1) == 1 {
			close(entered)
			<-release
		}
		return "$one", nil
	}
	h1.send, h2.send = send, send
	body := map[string]any{"owner": route.owner, "requestId": "same-cycle", "text": "digest", "summary": "notes"}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- assistantCall(h1, caller, "POST", "messages?team=alpha", body) }()
	<-entered
	w := assistantCall(h2, caller, "POST", "messages?team=alpha", body)
	close(release)
	if first := <-done; first.Code != 200 {
		t.Fatal(first.Code, first.Body.String())
	}
	if w.Code != 409 || sends.Load() != 1 {
		t.Fatal("parallel delivery escaped CAS lease", w.Code, sends.Load())
	}
	if w = assistantCall(h2, caller, "POST", "messages?team=alpha", body); w.Code != 200 || sends.Load() != 1 {
		t.Fatal("restart resent completed receipt", w.Code, sends.Load())
	}
}

func TestAssistantSendPreservesConcurrentHumanNotesAndDisabledSetting(t *testing.T) {
	coord, caller, route := assistantFixture(t, "qwenpaw")
	h := coord.assistant
	view := assistantCall(h, caller, "GET", "context?team=alpha", nil)
	var original map[string]any
	_ = json.Unmarshal(view.Body.Bytes(), &original)
	h.send = func(context.Context, string, string, string, string, map[string]any) (string, error) {
		w := assistantCall(h, caller, "PUT", "context?team=alpha", map[string]any{"owner": route.owner, "revision": original["revision"], "enabled": false, "summary": "Human's newer interests"})
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		return "$already-in-flight", nil
	}
	w := assistantCall(h, caller, "POST", "messages?team=alpha", map[string]any{"owner": route.owner, "requestId": "during-edit", "text": "digest", "summary": "old notes", "contextRevision": original["revision"]})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	state, _ := h.contextState(t.Context(), route)
	if state.Enabled || state.Summary != "Human's newer interests" || state.LastMessageID != "$already-in-flight" {
		t.Fatal("delivery overwrote user edits or lost send fact", state)
	}
}

func TestAssistantSendDoesNotRestoreInterestsClearedDuringInference(t *testing.T) {
	coord, caller, route := assistantFixture(t, "qwenpaw")
	h := coord.assistant
	h.send = func(context.Context, string, string, string, string, map[string]any) (string, error) {
		w := assistantCall(h, caller, "PUT", "context?team=alpha", map[string]any{"owner": route.owner, "revision": "", "summary": ""})
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		return "$sent-after-clear", nil
	}
	w := assistantCall(h, caller, "POST", "messages?team=alpha", map[string]any{"owner": route.owner, "requestId": "clear-during-inference", "text": "digest", "summary": "interests the user removed"})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	state, _ := h.contextState(t.Context(), route)
	if state.Summary != "" {
		t.Fatal("cleared private notes were resurrected", state.Summary)
	}
}

func TestAssistantDoesNotResendUnknownOutcomeAfterLeaderChanges(t *testing.T) {
	coord, caller, route := assistantFixture(t, "qwenpaw")
	receipt := assistantReceipt{Owner: "previous-leader", RequestID: "uncertain", Text: "digest", RoomID: route.room, Status: "sending", Sender: "@previous:test", LeaseUntil: time.Now().Add(-time.Minute)}
	b, _ := json.Marshal(receipt)
	_ = coord.deps.OSS.PutObject(t.Context(), path.Join(route.prefix, "receipts/uncertain.json"), b)
	coord.assistant.send = func(context.Context, string, string, string, string, map[string]any) (string, error) {
		t.Fatal("unknown send repeated under a different Matrix sender")
		return "", nil
	}
	w := assistantCall(coord.assistant, caller, "POST", "messages?team=alpha", map[string]any{"owner": route.owner, "requestId": "uncertain", "text": "digest"})
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestAssistantManagerHandoffPreservesContextAndRevokesOldOwner(t *testing.T) {
	coord, _ := coordinationFixture(t)
	human := &v1.Human{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"}, Spec: v1.HumanSpec{PermissionLevel: 1}, Status: v1.HumanStatus{MatrixUserID: "@alice:test", ManagerRooms: map[string]string{"default": "!alice:test"}}}
	if err := coord.deps.Client.Create(t.Context(), human); err != nil {
		t.Fatal(err)
	}
	h := NewAssistantHandler(coord.deps)
	caller := &authpkg.CallerIdentity{Role: authpkg.RoleManager, Username: "default"}
	resource := "context?manager=default&user=alice"
	w := assistantCall(h, caller, "GET", resource, nil)
	var old map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &old)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = assistantCall(h, caller, "PUT", resource, map[string]any{"owner": old["owner"], "revision": old["revision"], "summary": "alice context"}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var manager v1.Manager
	_ = coord.deps.Client.Get(t.Context(), client.ObjectKey{Name: "default", Namespace: "default"}, &manager)
	manager.Generation++
	_ = coord.deps.Client.Update(t.Context(), &manager)
	if w = assistantCall(h, caller, "PUT", resource, map[string]any{"owner": old["owner"], "revision": "", "summary": "old execution"}); w.Code != 409 {
		t.Fatal("replaced Manager retained write authority", w.Code)
	}
	w = assistantCall(h, caller, "GET", resource, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "alice context") || !strings.Contains(w.Body.String(), "!alice:test") {
		t.Fatal("Manager handoff lost private continuity", w.Body.String())
	}
}

func TestAssistantSchedulerQueuesEachL1PrivateRoomIndependently(t *testing.T) {
	coord, _, _ := assistantFixture(t, "qwenpaw")
	var manager v1.Manager
	key := client.ObjectKey{Name: "default", Namespace: "default"}
	_ = coord.deps.Client.Get(t.Context(), key, &manager)
	manager.Status.RoomID, manager.Status.Phase = "!system:test", "Running"
	_ = coord.deps.Client.Update(t.Context(), &manager)
	for _, name := range []string{"alice", "bob"} {
		human := &v1.Human{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: v1.HumanSpec{PermissionLevel: 1}, Status: v1.HumanStatus{MatrixUserID: "@" + name + ":test", ManagerRooms: map[string]string{"default": "!" + name + ":test"}}}
		if err := coord.deps.Client.Create(t.Context(), human); err != nil {
			t.Fatal(err)
		}
	}
	coord.scheduleAssistants(t.Context())
	coord.scheduleAssistants(t.Context())
	entries, _, err := coord.deps.OSS.ListObjectsRecursive(t.Context(), coordinationPending, "", 100)
	if err != nil || len(entries) != 3 {
		t.Fatal("one Team and two independent Manager reminders expected", len(entries), err)
	}
	rooms := map[string]bool{}
	for _, entry := range entries {
		var delivery coordinationDelivery
		if err := coord.assistant.read(t.Context(), path.Join(coordinationPending, entry.Name), &delivery); err != nil {
			t.Fatal(err)
		}
		rooms[delivery.Event.RoomID] = true
	}
	if !rooms["!alice:test"] || !rooms["!bob:test"] || rooms["!system:test"] {
		t.Fatal("private recipients shared or fell back to system room", rooms)
	}
}
