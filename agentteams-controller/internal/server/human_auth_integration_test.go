package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/gateway"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type humanWhoami struct{}

func (humanWhoami) Whoami(_ context.Context, token string) (string, error) {
	if token == "alice" || token == "bob" {
		return "@" + token + ":matrix.local", nil
	}
	return "", fmt.Errorf("invalid token")
}

func TestHumanControllerGatewayFlow(t *testing.T) {
	calls := 0
	console := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/system/init":
			fmt.Fprint(w, `{}`)
		case "/session/login":
			http.SetCookie(w, &http.Cookie{Name: "internal", Value: "secret"})
			fmt.Fprint(w, `{}`)
		default:
			calls++
			if r.Header.Get("Authorization") != "" {
				t.Error("user token leaked downstream")
			}
			if c, err := r.Cookie("internal"); err != nil || c.Value != "secret" {
				t.Error("missing internal gateway credential")
			}
			fmt.Fprint(w, `{"data":[{"name":"provider","tokens":["private-key"]}]}`)
		}
	}))
	defer console.Close()
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	alice := &v1beta1.Human{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default"}, Spec: v1beta1.HumanSpec{PermissionLevel: 1}, Status: v1beta1.HumanStatus{MatrixUserID: "@alice:matrix.local"}}
	bob := &v1beta1.Human{ObjectMeta: metav1.ObjectMeta{Name: "bob", Namespace: "default"}, Spec: v1beta1.HumanSpec{PermissionLevel: 2}}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(alice, bob).Build()
	authenticator := auth.NewMatrixTokenAuthenticator(k8s, "default", humanWhoami{})
	gw := gateway.NewHigressClient(gateway.Config{ConsoleURL: console.URL, AdminUser: "internal-admin", AdminPassword: "internal-password"}, console.Client())
	server := NewHTTPServer(":0", ServerDeps{Client: k8s, Namespace: "default", Gateway: gw, AuthMw: auth.NewMiddleware(authenticator, nil, auth.NewAuthorizer(), k8s, "default")})
	request := func(method, path, token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		server.Mux.ServeHTTP(w, r)
		return w
	}
	for _, token := range []string{"alice", "bob"} {
		w := request("GET", "/api/v1/me", token, "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"`+token+`"`) {
			t.Fatalf("me: %d %s", w.Code, w.Body)
		}
	}
	if w := request("GET", "/api/v1/me", "", ""); w.Code != 401 {
		t.Fatalf("unauthenticated me: %d", w.Code)
	}
	for _, method := range []string{"GET", "POST", "PUT", "DELETE"} {
		path := "/api/v1/gateway/ai/providers"
		if method == "PUT" || method == "DELETE" {
			path += "/provider"
		}
		if w := request(method, path, "bob", `{}`); w.Code != 403 {
			t.Fatalf("L2 %s: %d %s", method, w.Code, w.Body)
		}
	}
	if calls != 0 {
		t.Fatal("unauthorized request reached downstream")
	}
	w := request("GET", "/api/v1/gateway/ai/providers", "alice", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "private-key") || !strings.Contains(w.Body.String(), `"tokenCount":1`) {
		t.Fatalf("L1 provider listing: %d %s", w.Code, w.Body)
	}
	if w := request("POST", "/api/v1/gateway/ai/providers", "alice", `{}`); w.Code != 400 {
		t.Fatalf("invalid provider accepted: %d", w.Code)
	}
	for _, resource := range []string{"providers", "routes"} {
		payload := `{"name":"sample","type":"openai","tokens":["new-key"],"pathPredicate":{"matchType":"PRE","matchValue":"/"},"upstreams":[{"provider":"sample","weight":100}]}`
		for _, method := range []string{"POST", "PUT", "DELETE"} {
			path := "/api/v1/gateway/ai/" + resource
			if method != "POST" {
				path += "/sample"
			}
			if w := request(method, path, "alice", payload); w.Code != 200 {
				t.Fatalf("L1 %s %s: %d %s", method, resource, w.Code, w.Body)
			}
		}
	}
	if w := request("GET", "/api/v1/gateway/ai/secrets", "alice", ""); w.Code != 404 {
		t.Fatalf("arbitrary downstream surface exposed: %d", w.Code)
	}
	current := &v1beta1.Human{}
	if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(alice), current); err != nil {
		t.Fatal(err)
	}
	current.Spec.PermissionLevel = 2
	if err := k8s.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if w := request("GET", "/api/v1/gateway/ai/providers", "alice", ""); w.Code != 403 {
		t.Fatalf("downgraded admin retained access: %d", w.Code)
	}
	if err := k8s.Delete(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if w := request("GET", "/api/v1/me", "alice", ""); w.Code != 401 {
		t.Fatalf("deleted Human retained identity: %d", w.Code)
	}
}
