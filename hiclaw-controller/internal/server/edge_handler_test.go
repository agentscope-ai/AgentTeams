package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/edgebootstrap"
	"github.com/hiclaw/hiclaw-controller/internal/httputil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	edgeTestNamespace      = "test-ns"
	edgeTestController     = "test-controller"
	edgeTestUUID           = "test-uuid-123"
	edgeTestWorkerName     = "edge-worker-1"
	edgeTestIssuedToken    = "fake-sa-token"
	edgeTestRotatedUUIDNew = "test-uuid-rotated-456"
)

// stubEdgeProvisioner is a controllable mock of the EdgeProvisioner
// interface. Tests adjust the embedded fields to drive specific behaviors.
type stubEdgeProvisioner struct {
	token             string
	ensureErr         error
	deleteErr         error
	requestErr        error
	ensureCalls       int
	deleteCalls       int
	requestCalls      int
	lastEnsureWorker  string
	lastDeleteWorker  string
	lastRequestWorker string
}

func (s *stubEdgeProvisioner) EnsureServiceAccount(_ context.Context, workerName string) error {
	s.ensureCalls++
	s.lastEnsureWorker = workerName
	return s.ensureErr
}

func (s *stubEdgeProvisioner) DeleteServiceAccount(_ context.Context, workerName string) error {
	s.deleteCalls++
	s.lastDeleteWorker = workerName
	return s.deleteErr
}

func (s *stubEdgeProvisioner) RequestSAToken(_ context.Context, workerName string) (string, time.Time, error) {
	s.requestCalls++
	s.lastRequestWorker = workerName
	if s.requestErr != nil {
		return "", time.Time{}, s.requestErr
	}
	return s.token, time.Now().UTC().Add(time.Hour), nil
}

type stubAuthCacheInvalidator struct {
	calls int
}

func (s *stubAuthCacheInvalidator) InvalidateCache() {
	s.calls++
}

func newEdgeTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("add hiclaw scheme: %v", err)
	}
	return scheme
}

// newEdgeWorker builds an Edge-mode Worker CR with the given UUID label
// and (optionally) a previously-applied UUID annotation.
func newEdgeWorker(name, uuid, appliedUUID string) *v1beta1.Worker {
	deployMode := v1beta1.DeployModeEdge
	w := &v1beta1.Worker{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: edgeTestNamespace,
			Labels: map[string]string{
				v1beta1.LabelWorkerEdgeUUID: uuid,
				v1beta1.LabelController:     edgeTestController,
			},
		},
		Spec: v1beta1.WorkerSpec{
			Model:      "gpt-4",
			DeployMode: &deployMode,
		},
	}
	if appliedUUID != "" {
		w.Annotations = map[string]string{
			v1beta1.AnnotationEdgeAppliedUUID: appliedUUID,
		}
	}
	return w
}

func newEdgeHandlerWith(t *testing.T, prov EdgeProvisioner, objs ...client.Object) (*EdgeHandler, client.Client) {
	t.Helper()
	scheme := newEdgeTestScheme(t)
	k8sClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		Build()
	h := NewEdgeHandler(k8sClient, prov, newEdgeTestSigner(t), edgeTestNamespace, edgeTestController, nil)
	return h, k8sClient
}

func newEdgeHandlerWithInvalidator(t *testing.T, prov EdgeProvisioner, invalidator AuthCacheInvalidator, objs ...client.Object) (*EdgeHandler, client.Client) {
	t.Helper()
	scheme := newEdgeTestScheme(t)
	k8sClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		Build()
	h := NewEdgeHandler(k8sClient, prov, newEdgeTestSigner(t), edgeTestNamespace, edgeTestController, invalidator)
	return h, k8sClient
}

func newEdgeTestSigner(t *testing.T) *edgebootstrap.Service {
	t.Helper()
	signer := edgebootstrap.New(fake.NewSimpleClientset(), edgeTestNamespace, edgeTestController)
	if err := signer.Ensure(context.Background()); err != nil {
		t.Fatalf("ensure signing secret: %v", err)
	}
	return signer
}

func jwtBody(t *testing.T, handler *EdgeHandler, workerUUID string) string {
	t.Helper()
	signed, err := handler.signer.Sign(context.Background(), workerUUID, time.Hour, time.Now().UTC())
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	return fmt.Sprintf(`{"jwtToken":%q}`, signed.Token)
}

func doExchange(handler *EdgeHandler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/edge/token", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ExchangeToken(rec, req)
	return rec
}

func decodeErrorMessage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var er httputil.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &er); err != nil {
		t.Fatalf("decode error response: %v (body=%s)", err, rec.Body.String())
	}
	return er.Message
}

// TestEdgeExchangeToken_Success verifies the happy path: a valid JWT resolves
// to exactly one Edge Worker, the SA is ensured, a token is issued, and the
// applied-UUID annotation is recorded on the Worker.
func TestEdgeExchangeToken_Success(t *testing.T) {
	worker := newEdgeWorker(edgeTestWorkerName, edgeTestUUID, "")
	worker.Labels[v1beta1.LabelTeam] = "team-a"
	prov := &stubEdgeProvisioner{token: edgeTestIssuedToken}
	handler, k8sClient := newEdgeHandlerWith(t, prov, worker)

	rec := doExchange(handler, jwtBody(t, handler, edgeTestUUID))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp EdgeTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Token != edgeTestIssuedToken {
		t.Fatalf("token=%q, want %q", resp.Token, edgeTestIssuedToken)
	}
	if resp.ExpiresAt == "" {
		t.Fatal("expiresAt must be set")
	}
	if resp.JWTToken == "" || resp.JWTExpiresAt == "" {
		t.Fatalf("refreshed JWT fields must be set, got jwtToken=%q jwtExpiresAt=%q", resp.JWTToken, resp.JWTExpiresAt)
	}
	if resp.WorkerName != edgeTestWorkerName {
		t.Fatalf("workerName=%q, want %q", resp.WorkerName, edgeTestWorkerName)
	}
	if resp.WorkerResourceName != edgeTestWorkerName {
		t.Fatalf("workerResourceName=%q, want %q", resp.WorkerResourceName, edgeTestWorkerName)
	}
	if resp.RuntimeName != edgeTestWorkerName || resp.TeamName != "team-a" {
		t.Fatalf("runtime/team=%q/%q, want %q/team-a", resp.RuntimeName, resp.TeamName, edgeTestWorkerName)
	}

	if prov.ensureCalls != 1 || prov.lastEnsureWorker != edgeTestWorkerName {
		t.Fatalf("expected EnsureServiceAccount(%q) once, got calls=%d worker=%q",
			edgeTestWorkerName, prov.ensureCalls, prov.lastEnsureWorker)
	}
	if prov.requestCalls != 1 || prov.lastRequestWorker != edgeTestWorkerName {
		t.Fatalf("expected RequestSAToken(%q) once, got calls=%d worker=%q",
			edgeTestWorkerName, prov.requestCalls, prov.lastRequestWorker)
	}

	var updated v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: edgeTestWorkerName, Namespace: edgeTestNamespace}, &updated); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if got := updated.Annotations[v1beta1.AnnotationEdgeAppliedUUID]; got != edgeTestUUID {
		t.Fatalf("applied-uuid annotation=%q, want %q", got, edgeTestUUID)
	}
}

// TestEdgeExchangeToken_EmptyJWT verifies the handler rejects requests
// with an empty jwtToken body field.
func TestEdgeExchangeToken_EmptyJWT(t *testing.T) {
	prov := &stubEdgeProvisioner{token: edgeTestIssuedToken}
	handler, _ := newEdgeHandlerWith(t, prov)

	rec := doExchange(handler, `{"jwtToken":""}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if msg := decodeErrorMessage(t, rec); !strings.Contains(msg, "jwtToken is required") {
		t.Fatalf("error message=%q, want to contain %q", msg, "jwtToken is required")
	}
	if prov.ensureCalls != 0 || prov.requestCalls != 0 {
		t.Fatalf("expected no provisioner calls, got ensure=%d request=%d", prov.ensureCalls, prov.requestCalls)
	}
}

func TestEdgeExchangeToken_LegacyUUIDBodyRejected(t *testing.T) {
	prov := &stubEdgeProvisioner{token: edgeTestIssuedToken}
	handler, _ := newEdgeHandlerWith(t, prov)

	rec := doExchange(handler, `{"uuid":"`+edgeTestUUID+`"}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if prov.ensureCalls != 0 || prov.requestCalls != 0 {
		t.Fatalf("expected no provisioner calls, got ensure=%d request=%d", prov.ensureCalls, prov.requestCalls)
	}
}

func TestEdgeExchangeToken_InvalidJWTRejected(t *testing.T) {
	prov := &stubEdgeProvisioner{token: edgeTestIssuedToken}
	handler, _ := newEdgeHandlerWith(t, prov)

	rec := doExchange(handler, `{"jwtToken":"not-a-jwt"}`)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
	if prov.ensureCalls != 0 || prov.requestCalls != 0 {
		t.Fatalf("expected no provisioner calls, got ensure=%d request=%d", prov.ensureCalls, prov.requestCalls)
	}
}

// TestEdgeExchangeToken_UUIDNotFound verifies a 404 is returned when no
// Worker carries the requested UUID label.
func TestEdgeExchangeToken_UUIDNotFound(t *testing.T) {
	other := newEdgeWorker("other-worker", "other-uuid", "")
	prov := &stubEdgeProvisioner{token: edgeTestIssuedToken}
	handler, _ := newEdgeHandlerWith(t, prov, other)

	rec := doExchange(handler, jwtBody(t, handler, "unknown-uuid"))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if msg := decodeErrorMessage(t, rec); !strings.Contains(msg, "no worker found for the given uuid") {
		t.Fatalf("error message=%q, want to contain %q", msg, "no worker found for the given uuid")
	}
	if prov.ensureCalls != 0 || prov.requestCalls != 0 {
		t.Fatalf("expected no provisioner calls, got ensure=%d request=%d", prov.ensureCalls, prov.requestCalls)
	}
}

// TestEdgeExchangeToken_NotEdgeMode verifies a 403 is returned when the
// matched Worker is not configured for Edge deployment.
func TestEdgeExchangeToken_NotEdgeMode(t *testing.T) {
	worker := newEdgeWorker(edgeTestWorkerName, edgeTestUUID, "")
	local := v1beta1.DeployModeLocal
	worker.Spec.DeployMode = &local

	prov := &stubEdgeProvisioner{token: edgeTestIssuedToken}
	handler, _ := newEdgeHandlerWith(t, prov, worker)

	rec := doExchange(handler, jwtBody(t, handler, edgeTestUUID))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if msg := decodeErrorMessage(t, rec); !strings.Contains(msg, "worker deploy mode is not Edge") {
		t.Fatalf("error message=%q, want to contain %q", msg, "worker deploy mode is not Edge")
	}
	if prov.ensureCalls != 0 || prov.requestCalls != 0 {
		t.Fatalf("expected no provisioner calls, got ensure=%d request=%d", prov.ensureCalls, prov.requestCalls)
	}
}

// TestEdgeExchangeToken_NilDeployMode verifies a 403 is returned when
// spec.deployMode is unset (treated as non-Edge).
func TestEdgeExchangeToken_NilDeployMode(t *testing.T) {
	worker := newEdgeWorker(edgeTestWorkerName, edgeTestUUID, "")
	worker.Spec.DeployMode = nil

	prov := &stubEdgeProvisioner{token: edgeTestIssuedToken}
	handler, _ := newEdgeHandlerWith(t, prov, worker)

	rec := doExchange(handler, jwtBody(t, handler, edgeTestUUID))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestEdgeExchangeToken_MultipleMatches verifies a 409 is returned when
// more than one Worker carries the same edge UUID label.
func TestEdgeExchangeToken_MultipleMatches(t *testing.T) {
	a := newEdgeWorker("edge-worker-a", edgeTestUUID, "")
	b := newEdgeWorker("edge-worker-b", edgeTestUUID, "")

	prov := &stubEdgeProvisioner{token: edgeTestIssuedToken}
	handler, _ := newEdgeHandlerWith(t, prov, a, b)

	rec := doExchange(handler, jwtBody(t, handler, edgeTestUUID))

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if msg := decodeErrorMessage(t, rec); !strings.Contains(msg, "multiple workers found for the given uuid") {
		t.Fatalf("error message=%q, want to contain %q", msg, "multiple workers found for the given uuid")
	}
	if prov.ensureCalls != 0 || prov.requestCalls != 0 {
		t.Fatalf("expected no provisioner calls, got ensure=%d request=%d", prov.ensureCalls, prov.requestCalls)
	}
}

// TestEdgeExchangeToken_OldUUIDAfterRotation verifies that once the
// edge UUID label has been rotated to a new value, the old UUID can
// no longer be exchanged for a token.
func TestEdgeExchangeToken_OldUUIDAfterRotation(t *testing.T) {
	// Worker now carries the rotated UUID; the original UUID has been
	// replaced on the label and is not present anywhere.
	worker := newEdgeWorker(edgeTestWorkerName, edgeTestRotatedUUIDNew, edgeTestUUID)

	prov := &stubEdgeProvisioner{token: edgeTestIssuedToken}
	handler, _ := newEdgeHandlerWith(t, prov, worker)

	rec := doExchange(handler, jwtBody(t, handler, edgeTestUUID))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 after rotation, got %d: %s", rec.Code, rec.Body.String())
	}
	if msg := decodeErrorMessage(t, rec); !strings.Contains(msg, "no worker found for the given uuid") {
		t.Fatalf("error message=%q, want to contain %q", msg, "no worker found for the given uuid")
	}
	if prov.ensureCalls != 0 || prov.requestCalls != 0 {
		t.Fatalf("expected no provisioner calls, got ensure=%d request=%d", prov.ensureCalls, prov.requestCalls)
	}
}

// TestEdgeExchangeToken_NewUUIDAfterRotation verifies the rotated UUID
// is honored after the label is updated, and the applied-UUID
// annotation is rewritten to the new value.
func TestEdgeExchangeToken_NewUUIDAfterRotation(t *testing.T) {
	// Worker label now reflects the new UUID; annotation still records
	// the previously-issued UUID.
	worker := newEdgeWorker(edgeTestWorkerName, edgeTestRotatedUUIDNew, edgeTestUUID)

	prov := &stubEdgeProvisioner{token: "rotated-token"}
	invalidator := &stubAuthCacheInvalidator{}
	handler, k8sClient := newEdgeHandlerWithInvalidator(t, prov, invalidator, worker)

	rec := doExchange(handler, jwtBody(t, handler, edgeTestRotatedUUIDNew))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp EdgeTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Token != "rotated-token" {
		t.Fatalf("token=%q, want %q", resp.Token, "rotated-token")
	}
	if resp.WorkerName != edgeTestWorkerName {
		t.Fatalf("workerName=%q, want %q", resp.WorkerName, edgeTestWorkerName)
	}
	if prov.deleteCalls != 1 || prov.lastDeleteWorker != edgeTestWorkerName {
		t.Fatalf("expected DeleteServiceAccount(%q) once, got calls=%d worker=%q",
			edgeTestWorkerName, prov.deleteCalls, prov.lastDeleteWorker)
	}
	if invalidator.calls != 1 {
		t.Fatalf("InvalidateCache calls=%d, want 1", invalidator.calls)
	}
	if prov.ensureCalls != 1 || prov.requestCalls != 1 {
		t.Fatalf("ensure/request calls=%d/%d, want 1/1", prov.ensureCalls, prov.requestCalls)
	}

	var updated v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: edgeTestWorkerName, Namespace: edgeTestNamespace}, &updated); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if got := updated.Annotations[v1beta1.AnnotationEdgeAppliedUUID]; got != edgeTestRotatedUUIDNew {
		t.Fatalf("applied-uuid annotation=%q, want %q", got, edgeTestRotatedUUIDNew)
	}
}

func TestEdgeExchangeToken_NewUUIDAfterRotationDeleteSAFails(t *testing.T) {
	worker := newEdgeWorker(edgeTestWorkerName, edgeTestRotatedUUIDNew, edgeTestUUID)

	prov := &stubEdgeProvisioner{token: "rotated-token", deleteErr: errors.New("delete boom")}
	invalidator := &stubAuthCacheInvalidator{}
	handler, k8sClient := newEdgeHandlerWithInvalidator(t, prov, invalidator, worker)

	rec := doExchange(handler, jwtBody(t, handler, edgeTestRotatedUUIDNew))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
	if prov.deleteCalls != 1 || prov.lastDeleteWorker != edgeTestWorkerName {
		t.Fatalf("expected DeleteServiceAccount(%q) once, got calls=%d worker=%q",
			edgeTestWorkerName, prov.deleteCalls, prov.lastDeleteWorker)
	}
	if invalidator.calls != 0 {
		t.Fatalf("InvalidateCache calls=%d, want 0 after failed delete", invalidator.calls)
	}
	if prov.ensureCalls != 0 || prov.requestCalls != 0 {
		t.Fatalf("ensure/request calls=%d/%d, want 0/0", prov.ensureCalls, prov.requestCalls)
	}

	var updated v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: edgeTestWorkerName, Namespace: edgeTestNamespace}, &updated); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if got := updated.Annotations[v1beta1.AnnotationEdgeAppliedUUID]; got != edgeTestUUID {
		t.Fatalf("applied-uuid annotation=%q, want old %q after failed delete", got, edgeTestUUID)
	}
}

// TestEdgeExchangeToken_ControllerScoping verifies the handler ignores
// Workers owned by a different controller instance, even when their
// edge UUID label matches.
func TestEdgeExchangeToken_ControllerScoping(t *testing.T) {
	worker := newEdgeWorker(edgeTestWorkerName, edgeTestUUID, "")
	worker.Labels[v1beta1.LabelController] = "other-controller"

	prov := &stubEdgeProvisioner{token: edgeTestIssuedToken}
	handler, _ := newEdgeHandlerWith(t, prov, worker)

	rec := doExchange(handler, jwtBody(t, handler, edgeTestUUID))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for cross-controller UUID, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestEdgeExchangeToken_InvalidJSON verifies a malformed body is
// rejected with 400 before any client/provisioner work.
func TestEdgeExchangeToken_InvalidJSON(t *testing.T) {
	prov := &stubEdgeProvisioner{token: edgeTestIssuedToken}
	handler, _ := newEdgeHandlerWith(t, prov)

	rec := doExchange(handler, `{not-json`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if msg := decodeErrorMessage(t, rec); !strings.Contains(msg, "invalid request body") {
		t.Fatalf("error message=%q, want to contain %q", msg, "invalid request body")
	}
}

// TestEdgeExchangeToken_EnsureSAFails verifies provisioner errors during
// SA ensure surface as 500 and abort the flow before token issuance.
func TestEdgeExchangeToken_EnsureSAFails(t *testing.T) {
	worker := newEdgeWorker(edgeTestWorkerName, edgeTestUUID, "")
	prov := &stubEdgeProvisioner{
		token:     edgeTestIssuedToken,
		ensureErr: errors.New("boom"),
	}
	handler, _ := newEdgeHandlerWith(t, prov, worker)

	rec := doExchange(handler, jwtBody(t, handler, edgeTestUUID))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
	if prov.requestCalls != 0 {
		t.Fatalf("expected RequestSAToken not to be called, got %d", prov.requestCalls)
	}
}

// TestEdgeExchangeToken_EffectiveWorkerName verifies the response
// reflects spec.workerName when set, falling back to metadata.name
// otherwise (per WorkerSpec.EffectiveWorkerName).
func TestEdgeExchangeToken_EffectiveWorkerName(t *testing.T) {
	worker := newEdgeWorker(edgeTestWorkerName, edgeTestUUID, "")
	worker.Spec.WorkerName = "runtime-id-override"

	prov := &stubEdgeProvisioner{token: edgeTestIssuedToken}
	handler, _ := newEdgeHandlerWith(t, prov, worker)

	rec := doExchange(handler, jwtBody(t, handler, edgeTestUUID))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp EdgeTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.WorkerName != "runtime-id-override" {
		t.Fatalf("workerName=%q, want %q", resp.WorkerName, "runtime-id-override")
	}
	if resp.WorkerResourceName != edgeTestWorkerName {
		t.Fatalf("workerResourceName=%q, want CR name %q", resp.WorkerResourceName, edgeTestWorkerName)
	}

	// SA-related provisioner calls remain keyed by the CR name so the
	// in-cluster ServiceAccount lookup is unambiguous.
	if prov.lastEnsureWorker != edgeTestWorkerName {
		t.Fatalf("EnsureServiceAccount worker=%q, want CR name %q", prov.lastEnsureWorker, edgeTestWorkerName)
	}
	if prov.lastRequestWorker != edgeTestWorkerName {
		t.Fatalf("RequestSAToken worker=%q, want CR name %q", prov.lastRequestWorker, edgeTestWorkerName)
	}
}

// TestEdgeExchangeToken_AnnotationStableOnRepeat verifies a repeated
// exchange with the same UUID does not rewrite the annotation needlessly.
// (We cannot directly observe Update-skipping with the fake client, but
// we confirm the annotation value remains correct after a re-exchange.)
func TestEdgeExchangeToken_AnnotationStableOnRepeat(t *testing.T) {
	worker := newEdgeWorker(edgeTestWorkerName, edgeTestUUID, edgeTestUUID)
	prov := &stubEdgeProvisioner{token: edgeTestIssuedToken}
	handler, k8sClient := newEdgeHandlerWith(t, prov, worker)

	rec := doExchange(handler, jwtBody(t, handler, edgeTestUUID))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var updated v1beta1.Worker
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: edgeTestWorkerName, Namespace: edgeTestNamespace}, &updated); err != nil {
		t.Fatalf("get worker: %v", err)
	}
	if got := updated.Annotations[v1beta1.AnnotationEdgeAppliedUUID]; got != edgeTestUUID {
		t.Fatalf("applied-uuid annotation=%q, want %q", got, edgeTestUUID)
	}
}
