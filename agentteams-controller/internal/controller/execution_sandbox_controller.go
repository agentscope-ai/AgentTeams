package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/sandboxpolicy"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
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
	executionSandboxSpecHash         = "agentteams.io/execution-sandbox-spec-hash"
)

type ExecutionSandboxReconciler struct {
	client.Client

	RunnerImage      string
	ControllerName   string
	DefaultRuntime   string
	EgressCeilings   []v1beta1.DeepAgentsEgressRule
	EphemeralStorage sandboxpolicy.Policy
	Now              func() time.Time
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
	if backend.ResolveRuntime(worker.Spec.Runtime, r.DefaultRuntime) != backend.RuntimeDeepAgents {
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
	effectiveResources, podResources, emptyDirLimit, err := r.EphemeralStorage.Resolve(effective.Spec.Resources)
	if err != nil {
		return reconcile.Result{}, r.failInvalidResources(ctx, &sandbox, err)
	}
	effective.Spec.Resources = effectiveResources
	allowedEgress, err := intersectSandboxEgress(effective.Spec.Egress, r.EgressCeilings)
	if err != nil {
		return reconcile.Result{}, err
	}
	tokenSecretName := sandbox.Name
	if err := r.ensureExecutionSandboxToken(ctx, effective, tokenSecretName); err != nil {
		return reconcile.Result{}, err
	}
	pod, service, policy, err := buildExecutionSandboxResources(
		effective, tokenSecretName, r.ControllerName, allowedEgress, podResources, emptyDirLimit,
	)
	if err != nil {
		return reconcile.Result{}, err
	}
	for _, object := range []client.Object{service, policy, pod} {
		recreatePending, err := r.ensureExecutionSandboxObject(ctx, object)
		if err != nil {
			return reconcile.Result{}, err
		}
		if recreatePending {
			return reconcile.Result{RequeueAfter: executionSandboxRequeue}, nil
		}
	}

	var livePod corev1.Pod
	if err := r.Get(ctx, client.ObjectKeyFromObject(pod), &livePod); err != nil {
		return reconcile.Result{}, fmt.Errorf("get execution sandbox Pod: %w", err)
	}
	phase := "Pending"
	readyStatus := metav1.ConditionFalse
	reason := "PodPending"
	message := "execution sandbox runner Pod is pending"
	switch livePod.Status.Phase {
	case corev1.PodFailed:
		phase = "Failed"
		reason = "PodFailed"
		message = "execution sandbox runner Pod terminated with phase failed"
	case corev1.PodSucceeded:
		phase = "Failed"
		reason = "PodCompleted"
		message = "execution sandbox runner Pod terminated unexpectedly"
	case corev1.PodPending, corev1.PodRunning, corev1.PodUnknown, "":
		// Readiness is carried by the PodReady condition while the Pod is active.
	}
	if phase == "Pending" && executionSandboxPodReady(&livePod) {
		phase = "Ready"
		readyStatus = metav1.ConditionTrue
		reason = "PodReady"
		message = "execution sandbox runner Pod is ready"
	}
	statusBefore := sandbox.Status.DeepCopy()
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
		Message:            message,
	})
	if !apiequality.Semantic.DeepEqual(statusBefore, &sandbox.Status) {
		if err := r.Status().Update(ctx, &sandbox); err != nil {
			return reconcile.Result{}, fmt.Errorf("update execution sandbox status: %w", err)
		}
	}
	nextLifecycleCheck := expiresAt.Sub(now)
	if idleRemaining := lastActivity.Add(idleTimeout).Sub(now); idleRemaining < nextLifecycleCheck {
		nextLifecycleCheck = idleRemaining
	}
	if phase == "Pending" && nextLifecycleCheck > executionSandboxRequeue {
		nextLifecycleCheck = executionSandboxRequeue
	}
	return reconcile.Result{RequeueAfter: nextLifecycleCheck}, nil
}

func (r *ExecutionSandboxReconciler) failInvalidResources(
	ctx context.Context,
	sandbox *v1beta1.ExecutionSandbox,
	resolveErr error,
) error {
	for _, object := range []client.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: sandbox.Name, Namespace: sandbox.Namespace}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: sandbox.Name, Namespace: sandbox.Namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: sandbox.Name, Namespace: sandbox.Namespace}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: sandbox.Name, Namespace: sandbox.Namespace}},
	} {
		if err := r.Delete(ctx, object); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete invalid execution sandbox %T: %w", object, err)
		}
	}

	statusBefore := sandbox.Status.DeepCopy()
	sandbox.Status.ObservedGeneration = sandbox.Generation
	sandbox.Status.Phase = "Failed"
	sandbox.Status.Endpoint = ""
	sandbox.Status.PodName = ""
	apiMeta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: sandbox.Generation,
		Reason:             "InvalidResources",
		Message:            resolveErr.Error(),
	})
	if !apiequality.Semantic.DeepEqual(statusBefore, &sandbox.Status) {
		if err := r.Status().Update(ctx, sandbox); err != nil {
			return fmt.Errorf("update invalid execution sandbox status: %w", err)
		}
	}
	return nil
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
		if len(existing.Data["token"]) < 32 || len(existing.Data) != 1 ||
			existing.Immutable == nil || !*existing.Immutable ||
			!metav1.IsControlledBy(&existing, sandbox) ||
			existing.Labels[v1beta1.LabelExecutionSandbox] != sandbox.Name ||
			existing.Labels[v1beta1.LabelWorker] != sandbox.Spec.WorkerRef.Name ||
			existing.Labels[v1beta1.LabelController] != r.ControllerName {
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

func (r *ExecutionSandboxReconciler) ensureExecutionSandboxObject(ctx context.Context, desired client.Object) (bool, error) {
	live, ok := desired.DeepCopyObject().(client.Object)
	if !ok {
		return false, fmt.Errorf("execution sandbox object %T is not a Kubernetes client object", desired)
	}
	key := client.ObjectKeyFromObject(desired)
	if err := r.Get(ctx, key, live); apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return false, fmt.Errorf("create execution sandbox %T: %w", desired, err)
		}
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("get execution sandbox %T: %w", desired, err)
	}
	liveHash, err := executionSandboxManagedSpecHash(live)
	if err != nil {
		return false, err
	}
	desiredHash, err := executionSandboxManagedSpecHash(desired)
	if err != nil {
		return false, err
	}
	if liveHash == desiredHash {
		return false, nil
	}
	if desiredPolicy, ok := desired.(*networkingv1.NetworkPolicy); ok {
		livePolicy, liveOK := live.(*networkingv1.NetworkPolicy)
		if !liveOK {
			return false, fmt.Errorf("execution sandbox object %T has unexpected live type %T", desired, live)
		}
		desiredPolicy.ResourceVersion = livePolicy.ResourceVersion
		if err := r.Update(ctx, desiredPolicy); err != nil {
			return false, fmt.Errorf("update stale execution sandbox NetworkPolicy: %w", err)
		}
		return false, nil
	}
	if err := r.Delete(ctx, live); err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("delete stale execution sandbox %T: %w", desired, err)
	}
	return true, nil
}

func (r *ExecutionSandboxReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.ExecutionSandbox{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.NetworkPolicy{}).
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
	resources corev1.ResourceRequirements,
	emptyDirLimit apiresource.Quantity,
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
	workspaceSizeLimit := emptyDirLimit.DeepCopy()
	tmpSizeLimit := emptyDirLimit.DeepCopy()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            sandbox.Name,
			Namespace:       sandbox.Namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName:           "default",
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
				Name:            "runner",
				Image:           sandbox.Spec.Image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Resources:       resources,
				Ports: []corev1.ContainerPort{{
					Name:          "http",
					ContainerPort: executionSandboxRunnerPort,
					Protocol:      corev1.ProtocolTCP,
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
				{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &workspaceSizeLimit}}},
				{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &tmpSizeLimit}}},
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
				Protocol:   corev1.ProtocolTCP,
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
					v1beta1.LabelWorker:     sandbox.Spec.WorkerRef.Name,
					v1beta1.LabelController: controllerName,
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
	if len(allowedEgress) > 0 {
		// Explicit IP ceilings commonly target hostnames. Permit only cluster DNS
		// resolution; the resolved destination must still match a rule above.
		policy.Spec.Egress = append(policy.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
					"kubernetes.io/metadata.name": "kube-system",
				}},
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
					"k8s-app": "kube-dns",
				}},
			}},
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: protocolPointer(corev1.ProtocolUDP), Port: intstrPointer(intstr.FromInt32(53))},
				{Protocol: protocolPointer(corev1.ProtocolTCP), Port: intstrPointer(intstr.FromInt32(53))},
			},
		})
	}

	stampExecutionSandboxSpecHash(pod)
	stampExecutionSandboxSpecHash(service)
	stampExecutionSandboxSpecHash(policy)
	return pod, service, policy, nil
}

func stampExecutionSandboxSpecHash(object client.Object) {
	digest, err := executionSandboxManagedSpecHash(object)
	if err != nil {
		return
	}
	annotations := object.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[executionSandboxSpecHash] = digest
	object.SetAnnotations(annotations)
}

func executionSandboxManagedSpecHash(object client.Object) (string, error) {
	var managed any
	switch value := object.(type) {
	case *corev1.Pod:
		containers := make([]executionSandboxManagedContainer, 0, len(value.Spec.Containers))
		for _, container := range value.Spec.Containers {
			containers = append(containers, executionSandboxManagedContainer{
				Name:            container.Name,
				Image:           container.Image,
				ImagePullPolicy: container.ImagePullPolicy,
				Command:         container.Command,
				Args:            container.Args,
				WorkingDir:      container.WorkingDir,
				Ports:           container.Ports,
				Env:             container.Env,
				Resources:       container.Resources,
				VolumeMounts:    container.VolumeMounts,
				SecurityContext: container.SecurityContext,
			})
		}
		managed = struct {
			AutomountServiceAccountToken *bool                              `json:"automountServiceAccountToken,omitempty"`
			ServiceAccountName           string                             `json:"serviceAccountName,omitempty"`
			RestartPolicy                corev1.RestartPolicy               `json:"restartPolicy,omitempty"`
			SecurityContext              *corev1.PodSecurityContext         `json:"securityContext,omitempty"`
			Containers                   []executionSandboxManagedContainer `json:"containers"`
			Volumes                      []corev1.Volume                    `json:"volumes,omitempty"`
		}{
			AutomountServiceAccountToken: value.Spec.AutomountServiceAccountToken,
			ServiceAccountName:           value.Spec.ServiceAccountName,
			RestartPolicy:                value.Spec.RestartPolicy,
			SecurityContext:              value.Spec.SecurityContext,
			Containers:                   containers,
			Volumes:                      value.Spec.Volumes,
		}
	case *corev1.Service:
		managed = struct {
			Selector map[string]string    `json:"selector,omitempty"`
			Ports    []corev1.ServicePort `json:"ports,omitempty"`
		}{Selector: value.Spec.Selector, Ports: value.Spec.Ports}
	case *networkingv1.NetworkPolicy:
		managed = value.Spec
	default:
		return "", fmt.Errorf("unsupported execution sandbox object type %T", object)
	}
	managed = struct {
		Labels          map[string]string       `json:"labels,omitempty"`
		OwnerReferences []metav1.OwnerReference `json:"ownerReferences,omitempty"`
		Spec            any                     `json:"spec"`
	}{
		Labels:          executionSandboxManagedLabels(object.GetLabels()),
		OwnerReferences: object.GetOwnerReferences(),
		Spec:            managed,
	}
	payload, err := json.Marshal(managed)
	if err != nil {
		return "", fmt.Errorf("marshal execution sandbox managed spec for %T: %w", object, err)
	}
	digest := sha256.Sum256(payload)
	return fmt.Sprintf("%x", digest[:]), nil
}

func executionSandboxManagedLabels(labels map[string]string) map[string]string {
	managed := make(map[string]string, 4)
	for _, key := range []string{
		v1beta1.LabelController,
		v1beta1.LabelExecutionSandbox,
		v1beta1.LabelWorker,
		v1beta1.LabelRuntime,
	} {
		if value, ok := labels[key]; ok {
			managed[key] = value
		}
	}
	return managed
}

type executionSandboxManagedContainer struct {
	Name            string                      `json:"name"`
	Image           string                      `json:"image"`
	ImagePullPolicy corev1.PullPolicy           `json:"imagePullPolicy,omitempty"`
	Command         []string                    `json:"command,omitempty"`
	Args            []string                    `json:"args,omitempty"`
	WorkingDir      string                      `json:"workingDir,omitempty"`
	Ports           []corev1.ContainerPort      `json:"ports,omitempty"`
	Env             []corev1.EnvVar             `json:"env,omitempty"`
	Resources       corev1.ResourceRequirements `json:"resources,omitempty"`
	VolumeMounts    []corev1.VolumeMount        `json:"volumeMounts,omitempty"`
	SecurityContext *corev1.SecurityContext     `json:"securityContext,omitempty"`
}

func boolPointer(value bool) *bool { return &value }

func protocolPointer(value corev1.Protocol) *corev1.Protocol { return &value }

func intstrPointer(value intstr.IntOrString) *intstr.IntOrString { return &value }
