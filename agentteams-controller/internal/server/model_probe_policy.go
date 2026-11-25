package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"

	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/httputil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const modelProbePolicyName = "agentteams-model-probe-policy"

type modelProbePolicy struct {
	AllowedHosts        []string `json:"allowedHosts"`
	AllowPrivateNetwork bool     `json:"allowPrivateNetwork"`
}

type modelProbePolicyView struct {
	modelProbePolicy
	Revision string `json:"revision"`
	Source   string `json:"source,omitempty"`
}

func environmentModelProbePolicy() modelProbePolicy {
	policy := modelProbePolicy{AllowedHosts: []string{}, AllowPrivateNetwork: os.Getenv("MODEL_PROBE_ALLOW_PRIVATE_NETWORK") == "1"}
	for _, host := range strings.Split(os.Getenv("MODEL_PROBE_ALLOW_HOSTS"), ",") {
		if host = strings.ToLower(strings.TrimSpace(host)); host != "" {
			policy.AllowedHosts = append(policy.AllowedHosts, host)
		}
	}
	return policy
}

var modelProbeHostname = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$`)

func normalizeModelProbePolicy(policy *modelProbePolicy) error {
	if policy.AllowedHosts == nil {
		return fmt.Errorf("allowedHosts must be an array of hostnames or IP addresses")
	}
	hosts, seen := []string{}, map[string]bool{}
	for _, host := range policy.AllowedHosts {
		host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
		if ip := net.ParseIP(host); ip != nil {
			host = ip.String()
		} else if len(host) > 253 || !modelProbeHostname.MatchString(host) {
			return fmt.Errorf("invalid allowed host %q: use a hostname or IP without scheme, port, path, wildcard or CIDR", host)
		}
		if !seen[host] {
			hosts = append(hosts, host)
			seen[host] = true
		}
	}
	policy.AllowedHosts = hosts
	return nil
}

func (h *ResourceHandler) readModelProbePolicy(ctx context.Context) (modelProbePolicy, *corev1.ConfigMap, error) {
	if h.referenceReader() == nil {
		return modelProbePolicy{}, nil, fmt.Errorf("configuration storage unavailable")
	}
	cm := &corev1.ConfigMap{}
	err := h.referenceReader().Get(ctx, client.ObjectKey{Namespace: h.namespace, Name: modelProbePolicyName}, cm)
	if apierrors.IsNotFound(err) {
		return environmentModelProbePolicy(), nil, nil
	}
	if err != nil {
		return modelProbePolicy{}, nil, err
	}
	var policy modelProbePolicy
	if err := json.Unmarshal([]byte(cm.Data["policy.json"]), &policy); err != nil {
		return policy, nil, err
	}
	if err := normalizeModelProbePolicy(&policy); err != nil {
		return policy, nil, err
	}
	return policy, cm, nil
}

func modelProbePolicyResponse(policy modelProbePolicy, cm *corev1.ConfigMap) modelProbePolicyView {
	view := modelProbePolicyView{modelProbePolicy: policy, Source: "environment"}
	if cm != nil {
		view.Revision, view.Source = cm.ResourceVersion, "persistent"
	}
	return view
}

func modelProbePolicyAdmin(w http.ResponseWriter, r *http.Request) bool {
	caller := authpkg.CallerFromContext(r.Context())
	if caller == nil {
		httputil.WriteError(w, 401, "authentication required")
		return false
	}
	if caller.Role != authpkg.RoleAdmin {
		httputil.WriteError(w, 403, "only administrators can configure model probe addresses")
		return false
	}
	return true
}

func (h *ResourceHandler) GetModelProbePolicy(w http.ResponseWriter, r *http.Request) {
	if !modelProbePolicyAdmin(w, r) {
		return
	}
	policy, cm, err := h.readModelProbePolicy(r.Context())
	if err != nil {
		httputil.WriteError(w, 503, "cannot read model probe address configuration")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httputil.WriteJSON(w, 200, modelProbePolicyResponse(policy, cm))
}

func (h *ResourceHandler) PutModelProbePolicy(w http.ResponseWriter, r *http.Request) {
	if !modelProbePolicyAdmin(w, r) {
		return
	}
	_, cm, err := h.readModelProbePolicy(r.Context())
	if err != nil || h.client == nil {
		httputil.WriteError(w, 503, "cannot read model probe address configuration")
		return
	}
	var input modelProbePolicyView
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		httputil.WriteError(w, 400, "invalid model probe address configuration")
		return
	}
	if err := normalizeModelProbePolicy(&input.modelProbePolicy); err != nil {
		httputil.WriteError(w, 400, err.Error())
		return
	}
	revision := ""
	if cm != nil {
		revision = cm.ResourceVersion
	}
	if input.Revision != revision {
		httputil.WriteError(w, 409, "model probe address configuration changed; reload before saving")
		return
	}
	data, _ := json.Marshal(input.modelProbePolicy)
	if cm == nil {
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: modelProbePolicyName, Namespace: h.namespace}, Data: map[string]string{"policy.json": string(data)}}
		err = h.client.Create(r.Context(), cm)
	} else {
		cm.Data["policy.json"] = string(data)
		err = h.client.Update(r.Context(), cm)
	}
	if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) || apierrors.IsNotFound(err) {
		httputil.WriteError(w, 409, "model probe address configuration changed; reload before saving")
		return
	}
	if err != nil {
		httputil.WriteError(w, 503, "cannot save model probe address configuration")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httputil.WriteJSON(w, 200, modelProbePolicyResponse(input.modelProbePolicy, cm))
}

func (policy modelProbePolicy) hostAllowed(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if policy.AllowPrivateNetwork {
		return true
	}
	for _, allowed := range policy.AllowedHosts {
		if strings.EqualFold(strings.TrimSuffix(allowed, "."), host) {
			return true
		}
		if ip, approved := net.ParseIP(host), net.ParseIP(allowed); ip != nil && approved != nil && ip.Equal(approved) {
			return true
		}
	}
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified() && !ip.IsMulticast()
	}
	if host == "localhost" {
		return false
	}
	for _, suffix := range []string{".svc", ".svc.cluster.local", ".cluster.local", ".local"} {
		if strings.HasSuffix(host, suffix) {
			return false
		}
	}
	return true
}
