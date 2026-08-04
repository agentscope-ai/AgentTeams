package controller

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/netip"
	"strings"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	executionSandboxRunnerPort int32 = 8080
	executionSandboxRequeue          = 5 * time.Second
)

type ExecutionSandboxReconciler struct {
	client.Client

	RunnerImage    string
	ControllerName string
	EgressCeilings []v1beta1.DeepAgentsEgressRule
	Now            func() time.Time
}

func (r *ExecutionSandboxReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var sandbox v1beta1.ExecutionSandbox
	if err := r.Get(ctx, req.NamespacedName, &sandbox); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !sandbox.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}

	var worker v1beta1.Worker
	workerKey := client.ObjectKey{Name: sandbox.Spec.WorkerRef.Name, Namespace: sandbox.Namespace}
	if err := r.Get(ctx, workerKey, &worker); err != nil {
		return reconcile.Result{}, fmt.Errorf("get sandbox Worker %q: %w", sandbox.Spec.WorkerRef.Name, err)
	}
	if sandbox.Spec.WorkerRef.UID == "" || string(worker.UID) != sandbox.Spec.WorkerRef.UID {
		return reconcile.Result{}, fmt.Errorf("execution sandbox Worker UID does not match current Worker")
	}
	if worker.Spec.Runtime != "deepagents" {
		return reconcile.Result{}, fmt.Errorf("execution sandbox requires a deepagents Worker")
	}
	if worker.Spec.RuntimeConfig == nil || worker.Spec.RuntimeConfig.DeepAgents == nil ||
		worker.Spec.RuntimeConfig.DeepAgents.Execution.Mode != "sandbox" {
		return reconcile.Result{}, fmt.Errorf("deepagents Worker execution mode must be sandbox")
	}

	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	idleTimeout, err := executionSandboxDuration(sandbox.Spec.IdleTimeout, 30*time.Minute, "idleTimeout")
	if err != nil {
		return reconcile.Result{}, err
	}
	maxLifetime, err := executionSandboxDuration(sandbox.Spec.MaxLifetime, 8*time.Hour, "maxLifetime")
	if err != nil {
		return reconcile.Result{}, err
	}
	createdAt := sandbox.CreationTimestamp.Time
	if createdAt.IsZero() {
		createdAt = now
	}
	expiresAt := createdAt.Add(maxLifetime)
	lastActivity := createdAt
	if sandbox.Status.LastHeartbeat != nil {
		lastActivity = sandbox.Status.LastHeartbeat.Time
	}
	if !now.Before(expiresAt) || !now.Before(lastActivity.Add(idleTimeout)) {
		if err := r.Delete(ctx, &sandbox); err != nil && !apierrors.IsNotFound(err) {
			return reconcile.Result{}, fmt.Errorf("delete expired execution sandbox: %w", err)
		}
		return reconcile.Result{}, nil
	}

	effective := sandbox.DeepCopy()
	if strings.TrimSpace(effective.Spec.Image) == "" {
		effective.Spec.Image = strings.TrimSpace(r.RunnerImage)
	}
	allowedEgress, err := intersectSandboxEgress(effective.Spec.Egress, r.EgressCeilings)
	if err != nil {
		return reconcile.Result{}, err
	}
	tokenSecretName := sandbox.Name
	if err := r.ensureExecutionSandboxToken(ctx, effective, tokenSecretName); err != nil {
		return reconcile.Result{}, err
	}
	pod, service, policy, err := buildExecutionSandboxResources(
		effective, tokenSecretName, r.ControllerName, allowedEgress,
	)
	if err != nil {
		return reconcile.Result{}, err
	}
	for _, object := range []client.Object{service, policy, pod} {
		if err := r.Create(ctx, object); err != nil && !apierrors.IsAlreadyExists(err) {
			return reconcile.Result{}, fmt.Errorf("create execution sandbox %T: %w", object, err)
		}
	}

	var livePod corev1.Pod
	if err := r.Get(ctx, client.ObjectKeyFromObject(pod), &livePod); err != nil {
		return reconcile.Result{}, fmt.Errorf("get execution sandbox Pod: %w", err)
	}
	phase := "Pending"
	readyStatus := metav1.ConditionFalse
	reason := "PodPending"
	if executionSandboxPodReady(&livePod) {
		phase = "Ready"
		readyStatus = metav1.ConditionTrue
		reason = "PodReady"
	}
	sandbox.Status.ObservedGeneration = sandbox.Generation
	sandbox.Status.Phase = phase
	sandbox.Status.PodName = pod.Name
	sandbox.Status.Endpoint = fmt.Sprintf("http://%s.%s.svc:%d", service.Name, service.Namespace, executionSandboxRunnerPort)
	expiresAtMeta := metav1.NewTime(expiresAt)
	sandbox.Status.ExpiresAt = &expiresAtMeta
	apiMeta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             readyStatus,
		ObservedGeneration: sandbox.Generation,
		Reason:             reason,
		Message:            "execution sandbox runner Pod is " + strings.ToLower(phase),
	})
	if err := r.Status().Update(ctx, &sandbox); err != nil {
		return reconcile.Result{}, fmt.Errorf("update execution sandbox status: %w", err)
	}
	if phase != "Ready" {
		return reconcile.Result{RequeueAfter: executionSandboxRequeue}, nil
	}
	return reconcile.Result{}, nil
}

func executionSandboxDuration(raw string, fallback time.Duration, field string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(raw)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("execution sandbox %s must be a positive Go duration", field)
	}
	return duration, nil
}

func (r *ExecutionSandboxReconciler) ensureExecutionSandboxToken(
	ctx context.Context,
	sandbox *v1beta1.ExecutionSandbox,
	name string,
) error {
	var existing corev1.Secret
	key := client.ObjectKey{Name: name, Namespace: sandbox.Namespace}
	if err := r.Get(ctx, key, &existing); err == nil {
		if len(existing.Data["token"]) < 32 || len(existing.Data) != 1 {
			return fmt.Errorf("execution sandbox token Secret %q is malformed", name)
		}
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get execution sandbox token Secret: %w", err)
	}

	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return fmt.Errorf("generate execution sandbox runner token: %w", err)
	}
	token := []byte(base64.RawURLEncoding.EncodeToString(random))
	immutable := true
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: sandbox.Namespace,
			Labels: map[string]string{
				v1beta1.LabelController:       r.ControllerName,
				v1beta1.LabelExecutionSandbox: sandbox.Name,
				v1beta1.LabelWorker:           sandbox.Spec.WorkerRef.Name,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         v1beta1.SchemeGroupVersion.String(),
				Kind:               "ExecutionSandbox",
				Name:               sandbox.Name,
				UID:                sandbox.UID,
				Controller:         boolPointer(true),
				BlockOwnerDeletion: boolPointer(true),
			}},
		},
		Immutable: &immutable,
		Type:      corev1.SecretTypeOpaque,
		Data:      map[string][]byte{"token": token},
	}
	if err := r.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create execution sandbox token Secret: %w", err)
	}
	return nil
}

func executionSandboxPodReady(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (r *ExecutionSandboxReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.ExecutionSandbox{}).
		Owns(&corev1.Pod{}).
		Complete(r)
}

func intersectSandboxEgress(
	requested []v1beta1.DeepAgentsEgressRule,
	ceilings []v1beta1.DeepAgentsEgressRule,
) ([]v1beta1.DeepAgentsEgressRule, error) {
	requestedParsed, err := parseSandboxEgressRules(requested)
	if err != nil {
		return nil, fmt.Errorf("requested egress: %w", err)
	}
	ceilingParsed, err := parseSandboxEgressRules(ceilings)
	if err != nil {
		return nil, fmt.Errorf("egress ceiling: %w", err)
	}

	var result []v1beta1.DeepAgentsEgressRule
	seen := map[string]struct{}{}
	for _, req := range requestedParsed {
		for _, ceiling := range ceilingParsed {
			if req.protocol != ceiling.protocol {
				continue
			}
			intersection, ok := narrowerPrefix(req.prefix, ceiling.prefix)
			if !ok {
				continue
			}
			ports := intersectPorts(req.ports, ceiling.ports)
			if len(ports) == 0 {
				continue
			}
			key := intersection.String() + "/" + req.protocol
			for _, port := range ports {
				key += fmt.Sprintf("/%d", port)
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			result = append(result, v1beta1.DeepAgentsEgressRule{
				CIDR:     intersection.String(),
				Protocol: req.protocol,
				Ports:    ports,
			})
		}
	}
	return result, nil
}

type parsedSandboxEgressRule struct {
	prefix   netip.Prefix
	protocol string
	ports    []int32
}

func parseSandboxEgressRules(rules []v1beta1.DeepAgentsEgressRule) ([]parsedSandboxEgressRule, error) {
	parsed := make([]parsedSandboxEgressRule, 0, len(rules))
	for _, rule := range rules {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(rule.CIDR))
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", rule.CIDR, err)
		}
		prefix = prefix.Masked()
		protocol := strings.ToUpper(strings.TrimSpace(rule.Protocol))
		if protocol == "" {
			protocol = string(corev1.ProtocolTCP)
		}
		if protocol != string(corev1.ProtocolTCP) && protocol != string(corev1.ProtocolUDP) {
			return nil, fmt.Errorf("unsupported protocol %q", rule.Protocol)
		}
		if len(rule.Ports) == 0 {
			return nil, fmt.Errorf("CIDR %q must declare at least one port", rule.CIDR)
		}
		ports := make([]int32, len(rule.Ports))
		copy(ports, rule.Ports)
		for _, port := range ports {
			if port < 1 || port > 65535 {
				return nil, fmt.Errorf("invalid port %d for CIDR %q", port, rule.CIDR)
			}
		}
		parsed = append(parsed, parsedSandboxEgressRule{prefix: prefix, protocol: protocol, ports: ports})
	}
	return parsed, nil
}

func narrowerPrefix(a, b netip.Prefix) (netip.Prefix, bool) {
	if a.Addr().Is4() != b.Addr().Is4() {
		return netip.Prefix{}, false
	}
	if b.Contains(a.Addr()) && b.Bits() <= a.Bits() {
		return a, true
	}
	if a.Contains(b.Addr()) && a.Bits() <= b.Bits() {
		return b, true
	}
	return netip.Prefix{}, false
}

func intersectPorts(requested, ceilings []int32) []int32 {
	allowed := make(map[int32]struct{}, len(ceilings))
	for _, port := range ceilings {
		allowed[port] = struct{}{}
	}
	result := make([]int32, 0, len(requested))
	seen := map[int32]struct{}{}
	for _, port := range requested {
		if _, ok := allowed[port]; !ok {
			continue
		}
		if _, duplicate := seen[port]; duplicate {
			continue
		}
		seen[port] = struct{}{}
		result = append(result, port)
	}
	return result
}

func buildExecutionSandboxResources(
	sandbox *v1beta1.ExecutionSandbox,
	tokenSecretName string,
	controllerName string,
	allowedEgress []v1beta1.DeepAgentsEgressRule,
) (*corev1.Pod, *corev1.Service, *networkingv1.NetworkPolicy, error) {
	if sandbox == nil {
		return nil, nil, nil, fmt.Errorf("execution sandbox is required")
	}
	if strings.TrimSpace(sandbox.Spec.Image) == "" {
		return nil, nil, nil, fmt.Errorf("execution sandbox image is required")
	}
	if strings.TrimSpace(tokenSecretName) == "" {
		return nil, nil, nil, fmt.Errorf("runner token Secret name is required")
	}

	labels := map[string]string{
		v1beta1.LabelController:       controllerName,
		v1beta1.LabelExecutionSandbox: sandbox.Name,
		v1beta1.LabelWorker:           sandbox.Spec.WorkerRef.Name,
		v1beta1.LabelRuntime:          "deepagents-runner",
	}
	owner := metav1.OwnerReference{
		APIVersion:         v1beta1.SchemeGroupVersion.String(),
		Kind:               "ExecutionSandbox",
		Name:               sandbox.Name,
		UID:                sandbox.UID,
		Controller:         boolPointer(true),
		BlockOwnerDeletion: boolPointer(true),
	}
	runAsNonRoot := true
	runAsUser := int64(65532)
	readOnlyRoot := true
	allowPrivilegeEscalation := false
	automountToken := false
	resources, err := sandboxResourceRequirements(sandbox.Spec.Resources)
	if err != nil {
		return nil, nil, nil, err
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            sandbox.Name,
			Namespace:       sandbox.Namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: &automountToken,
			RestartPolicy:                corev1.RestartPolicyNever,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: &runAsNonRoot,
				RunAsUser:    &runAsUser,
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
			Containers: []corev1.Container{{
				Name:      "runner",
				Image:     sandbox.Spec.Image,
				Resources: resources,
				Ports: []corev1.ContainerPort{{
					Name:          "http",
					ContainerPort: executionSandboxRunnerPort,
				}},
				Env: []corev1.EnvVar{{
					Name: "AGENTTEAMS_RUNNER_TOKEN",
					ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: tokenSecretName},
						Key:                  "token",
					}},
				}},
				SecurityContext: &corev1.SecurityContext{
					ReadOnlyRootFilesystem:   &readOnlyRoot,
					AllowPrivilegeEscalation: &allowPrivilegeEscalation,
					RunAsNonRoot:             &runAsNonRoot,
					Capabilities: &corev1.Capabilities{
						Drop: []corev1.Capability{"ALL"},
					},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "workspace", MountPath: "/workspace"},
					{Name: "tmp", MountPath: "/tmp"},
				},
			}},
			Volumes: []corev1.Volume{
				{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
		},
	}

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            sandbox.Name,
			Namespace:       sandbox.Namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{v1beta1.LabelExecutionSandbox: sandbox.Name},
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       executionSandboxRunnerPort,
				TargetPort: intstr.FromString("http"),
			}},
		},
	}

	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:            sandbox.Name,
			Namespace:       sandbox.Namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{
				v1beta1.LabelExecutionSandbox: sandbox.Name,
			}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
					v1beta1.LabelWorker: sandbox.Spec.WorkerRef.Name,
				}}}},
				Ports: []networkingv1.NetworkPolicyPort{{
					Protocol: protocolPointer(corev1.ProtocolTCP),
					Port:     intstrPointer(intstr.FromInt32(executionSandboxRunnerPort)),
				}},
			}},
		},
	}
	for _, rule := range allowedEgress {
		protocol := corev1.Protocol(strings.ToUpper(strings.TrimSpace(rule.Protocol)))
		if protocol == "" {
			protocol = corev1.ProtocolTCP
		}
		egressRule := networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: rule.CIDR}}},
		}
		for _, port := range rule.Ports {
			egressRule.Ports = append(egressRule.Ports, networkingv1.NetworkPolicyPort{
				Protocol: protocolPointer(protocol),
				Port:     intstrPointer(intstr.FromInt32(port)),
			})
		}
		policy.Spec.Egress = append(policy.Spec.Egress, egressRule)
	}

	return pod, service, policy, nil
}

func sandboxResourceRequirements(in *v1beta1.AgentResourceRequirements) (corev1.ResourceRequirements, error) {
	result := corev1.ResourceRequirements{}
	if in == nil {
		return result, nil
	}
	requests, err := sandboxResourceList(in.Requests)
	if err != nil {
		return result, fmt.Errorf("sandbox resource requests: %w", err)
	}
	limits, err := sandboxResourceList(in.Limits)
	if err != nil {
		return result, fmt.Errorf("sandbox resource limits: %w", err)
	}
	result.Requests = requests
	result.Limits = limits
	return result, nil
}

func sandboxResourceList(values v1beta1.AgentResourceValues) (corev1.ResourceList, error) {
	result := corev1.ResourceList{}
	for name, raw := range map[corev1.ResourceName]string{
		corev1.ResourceCPU:    values.CPU,
		corev1.ResourceMemory: values.Memory,
	} {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		quantity, err := apiresource.ParseQuantity(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid %s quantity %q: %w", name, raw, err)
		}
		result[name] = quantity
	}
	return result, nil
}

func boolPointer(value bool) *bool { return &value }

func protocolPointer(value corev1.Protocol) *corev1.Protocol { return &value }

func intstrPointer(value intstr.IntOrString) *intstr.IntOrString { return &value }
