package service

import (
	"context"
	"fmt"
	"time"

	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	authpkg "github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/auth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/backend"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// --- ServiceAccount Management ---

// EnsureServiceAccount creates a ServiceAccount for the worker if it doesn't exist.
func (p *Provisioner) EnsureServiceAccount(ctx context.Context, workerName string) error {
	if p.k8sClient == nil {
		return nil
	}
	saName := p.resourcePrefix.SAName(authpkg.RoleWorker, workerName)
	ns := p.namespace

	_, err := p.k8sClient.CoreV1().ServiceAccounts(ns).Get(ctx, saName, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get SA %s: %w", saName, err)
	}

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: ns,
			Labels: map[string]string{
				"app":                   p.resourcePrefix.WorkerAppLabel(),
				v1beta1.LabelWorker:     workerName,
				v1beta1.LabelController: p.controllerName,
			},
		},
	}
	if _, err := p.k8sClient.CoreV1().ServiceAccounts(ns).Create(ctx, sa, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create SA %s: %w", saName, err)
		}
	}

	return nil
}

// DeleteServiceAccount removes the ServiceAccount for the worker.
func (p *Provisioner) DeleteServiceAccount(ctx context.Context, workerName string) error {
	if p.k8sClient == nil {
		return nil
	}
	saName := p.resourcePrefix.SAName(authpkg.RoleWorker, workerName)
	ns := p.namespace

	err := p.k8sClient.CoreV1().ServiceAccounts(ns).Delete(ctx, saName, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// EnsureManagerServiceAccount creates a ServiceAccount for the Manager if it doesn't exist.
func (p *Provisioner) EnsureManagerServiceAccount(ctx context.Context, managerName string) error {
	if p.k8sClient == nil {
		return nil
	}
	saName := p.resourcePrefix.SAName(authpkg.RoleManager, managerName)
	ns := p.namespace

	_, err := p.k8sClient.CoreV1().ServiceAccounts(ns).Get(ctx, saName, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get SA %s: %w", saName, err)
	}

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: ns,
			Labels: map[string]string{
				"app":                   p.resourcePrefix.ManagerAppLabel(),
				v1beta1.LabelManager:    managerName,
				v1beta1.LabelController: p.controllerName,
			},
		},
	}
	if _, err := p.k8sClient.CoreV1().ServiceAccounts(ns).Create(ctx, sa, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create SA %s: %w", saName, err)
		}
	}

	return nil
}

// DeleteManagerServiceAccount removes the ServiceAccount for the Manager.
func (p *Provisioner) DeleteManagerServiceAccount(ctx context.Context, managerName string) error {
	if p.k8sClient == nil {
		return nil
	}
	saName := p.resourcePrefix.SAName(authpkg.RoleManager, managerName)
	ns := p.namespace

	err := p.k8sClient.CoreV1().ServiceAccounts(ns).Delete(ctx, saName, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// RequestManagerSAToken issues a short-lived SA token for Manager in non-K8s backends.
func (p *Provisioner) RequestManagerSAToken(ctx context.Context, managerName string) (string, error) {
	if p.k8sClient == nil {
		return "", nil
	}
	saName := p.resourcePrefix.SAName(authpkg.RoleManager, managerName)
	audience := p.authAudience
	if audience == "" {
		audience = authpkg.DefaultAudience
	}
	expSeconds := int64(315360000)

	tokenReq := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			Audiences:         []string{audience},
			ExpirationSeconds: &expSeconds,
		},
	}

	result, err := p.k8sClient.CoreV1().ServiceAccounts(p.namespace).CreateToken(
		ctx, saName, tokenReq, metav1.CreateOptions{},
	)
	if err != nil {
		return "", fmt.Errorf("request SA token for manager %s: %w", managerName, err)
	}
	return result.Status.Token, nil
}

// EnsureAdminServiceAccount creates the admin ServiceAccount if it doesn't exist.
//
// The admin SA exists primarily so the bundled `agt` CLI inside the
// agentteams-controller container (and any other operator tooling that runs in
// the same trust boundary, e.g. install-time `docker exec agentteams-controller
// hiclaw …`) can authenticate against the controller's HTTP API with
// admin-role privileges via the standard TokenReview path. Without it the
// only paths to call the API would be (a) reusing a Manager- or Worker-scoped
// SA token (wrong privilege scope, and not always available locally) or
// (b) using the embedded kube-apiserver's static admin token (a different
// token format that `prefix.ParseSAUsername` rejects with HTTP 401, since
// it surfaces as username `admin` rather than `system:serviceaccount:…`).
//
// Idempotent: returns nil if the SA already exists. Returns nil silently
// when k8sClient is nil (incluster-without-bootstrap or unit-test paths).
func (p *Provisioner) EnsureAdminServiceAccount(ctx context.Context) error {
	if p.k8sClient == nil {
		return nil
	}
	saName := p.resourcePrefix.AdminName()
	ns := p.namespace

	_, err := p.k8sClient.CoreV1().ServiceAccounts(ns).Get(ctx, saName, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get SA %s: %w", saName, err)
	}

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: ns,
			Labels: map[string]string{
				v1beta1.LabelRole: authpkg.RoleAdmin,
			},
		},
	}
	if _, err := p.k8sClient.CoreV1().ServiceAccounts(ns).Create(ctx, sa, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create SA %s: %w", saName, err)
		}
	}

	return nil
}

// RequestAdminSAToken issues a long-lived SA token for the admin SA.
//
// The token is written by the embedded-mode startup path to a known
// location (see `internal/app.bootstrapAdminCLIToken`) so that the bundled
// `agt` CLI can auto-discover it via the `AGENTTEAMS_AUTH_TOKEN_FILE`
// environment variable that the controller image sets by default.
//
// Expiration mirrors `RequestManagerSAToken` (~10 years) — the controller
// re-mints on every startup, so any reasonable lower bound would also
// work; we keep the long horizon to avoid surprise auth failures during
// long-running interactive shells when the operator forgets to re-source.
func (p *Provisioner) RequestAdminSAToken(ctx context.Context) (string, error) {
	if p.k8sClient == nil {
		return "", nil
	}
	saName := p.resourcePrefix.AdminName()
	audience := p.authAudience
	if audience == "" {
		audience = authpkg.DefaultAudience
	}
	expSeconds := int64(315360000)

	tokenReq := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			Audiences:         []string{audience},
			ExpirationSeconds: &expSeconds,
		},
	}

	result, err := p.k8sClient.CoreV1().ServiceAccounts(p.namespace).CreateToken(
		ctx, saName, tokenReq, metav1.CreateOptions{},
	)
	if err != nil {
		return "", fmt.Errorf("request SA token for admin: %w", err)
	}
	return result.Status.Token, nil
}

// RequestSAToken issues a short-lived SA token for non-K8s backends (Docker)
// and remote-managed Edge workers.
func (p *Provisioner) RequestSAToken(ctx context.Context, workerName string) (string, time.Time, error) {
	projection, err := p.ProjectSAToken(ctx, workerName, 3600)
	if err != nil || projection == nil {
		return "", time.Time{}, err
	}
	return projection.Token, projection.ExpirationTimestamp, nil
}

// RequestSATokenWithExpiration issues an SA token with a caller-controlled TTL.
func (p *Provisioner) RequestSATokenWithExpiration(ctx context.Context, workerName string, expirationSeconds int64) (string, error) {
	projection, err := p.ProjectSAToken(ctx, workerName, expirationSeconds)
	if err != nil || projection == nil {
		return "", err
	}
	return projection.Token, nil
}

// ProjectSAToken issues an SA token with a caller-controlled TTL and returns
// the actual expiration timestamp reported by the Kubernetes TokenRequest API.
func (p *Provisioner) ProjectSAToken(ctx context.Context, workerName string, expirationSeconds int64) (*SATokenProjection, error) {
	if p.k8sClient == nil {
		return &SATokenProjection{}, nil
	}
	saName := p.resourcePrefix.SAName(authpkg.RoleWorker, workerName)
	audience := p.authAudience
	if audience == "" {
		audience = authpkg.DefaultAudience
	}
	expirationSeconds = backend.NormalizeAuthTokenExpirationSeconds(expirationSeconds)
	issuedAt := time.Now()

	tokenReq := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			Audiences:         []string{audience},
			ExpirationSeconds: &expirationSeconds,
		},
	}

	result, err := p.k8sClient.CoreV1().ServiceAccounts(p.namespace).CreateToken(
		ctx, saName, tokenReq, metav1.CreateOptions{},
	)
	if err != nil {
		return nil, fmt.Errorf("request SA token for %s: %w", workerName, err)
	}
	return &SATokenProjection{
		Token:               result.Status.Token,
		IssuedAt:            issuedAt,
		ExpirationTimestamp: result.Status.ExpirationTimestamp.Time,
		ExpirationSeconds:   expirationSeconds,
	}, nil
}

// EnsureRemoteNamespace creates the target namespace on the remote cluster if
// it does not already exist.
func (p *Provisioner) EnsureRemoteNamespace(ctx context.Context, clusterID, namespace string) error {
	if p.remoteCache == nil {
		return fmt.Errorf("remote client provider not configured")
	}

	cli, err := p.remoteCache.ResolveClient(ctx, clusterID)
	if err != nil {
		return fmt.Errorf("resolve remote client for cluster %s: %w", clusterID, err)
	}

	return ensureRemoteNamespace(ctx, cli, clusterID, namespace)
}

func ensureRemoteNamespace(ctx context.Context, cli backend.K8sCoreClient, clusterID, namespace string) error {
	if _, err := cli.Namespaces().Get(ctx, namespace, metav1.GetOptions{}); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("check remote namespace %s in cluster %s: %w", namespace, clusterID, err)
		}
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		}
		if _, err := cli.Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create remote namespace %s in cluster %s: %w", namespace, clusterID, err)
		}
	}
	return nil
}

// EnsureRemoteServiceAccount creates the worker ServiceAccount on the remote
// cluster identified by clusterID. It is idempotent: if the SA already exists,
// it returns nil.
func (p *Provisioner) EnsureRemoteServiceAccount(
	ctx context.Context,
	workerName, clusterID, namespace string,
) error {
	if p.remoteCache == nil {
		return fmt.Errorf("remote client provider not configured")
	}

	cli, err := p.remoteCache.ResolveClient(ctx, clusterID)
	if err != nil {
		return fmt.Errorf("resolve remote client for cluster %s: %w", clusterID, err)
	}

	// Ensure the target namespace exists on the remote cluster before creating the SA.
	if err := ensureRemoteNamespace(ctx, cli, clusterID, namespace); err != nil {
		return err
	}

	saName := p.resourcePrefix.SAName(authpkg.RoleWorker, workerName)
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "agentteams-controller",
				v1beta1.LabelRole:              "worker",
				v1beta1.LabelWorker:            workerName,
			},
		},
	}

	if _, err := cli.ServiceAccounts(namespace).Create(ctx, sa, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("create remote SA %s in cluster %s namespace %s: %w", saName, clusterID, namespace, err)
	}
	return nil
}

// DeleteRemoteServiceAccount deletes the remote worker SA. NotFound is ignored.
func (p *Provisioner) DeleteRemoteServiceAccount(
	ctx context.Context,
	workerName, clusterID, namespace string,
) error {
	if p.remoteCache == nil {
		return fmt.Errorf("remote client provider not configured")
	}

	cli, err := p.remoteCache.ResolveClient(ctx, clusterID)
	if err != nil {
		return fmt.Errorf("resolve remote client for cluster %s: %w", clusterID, err)
	}

	saName := p.resourcePrefix.SAName(authpkg.RoleWorker, workerName)
	if err := cli.ServiceAccounts(namespace).Delete(ctx, saName, metav1.DeleteOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete remote SA %s in cluster %s namespace %s: %w", saName, clusterID, namespace, err)
	}
	return nil
}
