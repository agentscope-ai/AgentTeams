package controller

import (
	"context"
	"fmt"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/hiclaw/hiclaw-controller/internal/backend"
)

// ReconcileMemberService ensures a ClusterIP Service exists (or is removed)
// based on ServiceEnabled. This phase runs after ReconcileMemberContainer so
// the Pod selector labels are guaranteed to be present on the target pod.
func ReconcileMemberService(ctx context.Context, mc *MemberContext, deps *MemberDeps) error {
	if !mc.ServiceEnabled {
		return ensureServiceDeleted(ctx, mc, deps)
	}
	return ensureServiceExists(ctx, mc, deps)
}

// ensureServiceExists creates or updates a ClusterIP Service for the member pod.
func ensureServiceExists(ctx context.Context, mc *MemberContext, deps *MemberDeps) error {
	logger := log.FromContext(ctx)

	// A Service without ports is useless; delete any stale Service from a
	// previous expose config.
	if len(mc.Spec.Expose) == 0 {
		logger.V(1).Info("serviceEnabled is true but no ports declared in spec.expose, deleting stale Service if present", "name", mc.Name)
		return ensureServiceDeleted(ctx, mc, deps)
	}

	svcClient, ns, err := resolveServiceClient(ctx, mc, deps)
	if err != nil {
		return err
	}

	svcName := serviceName(deps, mc)
	selector := serviceSelector(mc)
	desiredPorts := buildServicePorts(mc)

	existing, err := svcClient.Get(ctx, svcName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get service %s/%s: %w", ns, svcName, err)
	}

	if apierrors.IsNotFound(err) {
		// Create
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      svcName,
				Namespace: ns,
				Labels: map[string]string{
					"agentteams.io/worker": mc.Name,
					"app":                  deps.ResourcePrefix.WorkerAppLabel(),
				},
			},
			Spec: corev1.ServiceSpec{
				Type:     corev1.ServiceTypeClusterIP,
				Selector: selector,
				Ports:    desiredPorts,
			},
		}
		if _, err := svcClient.Create(ctx, svc, metav1.CreateOptions{}); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// Lost the race; will reconcile on next pass.
				return nil
			}
			return fmt.Errorf("create service %s/%s: %w", ns, svcName, err)
		}
		logger.Info("created ClusterIP Service for member", "name", mc.Name, "service", svcName, "namespace", ns)
		return nil
	}

	// Update if selector or ports differ.
	needsUpdate := !reflect.DeepEqual(existing.Spec.Selector, selector) ||
		!reflect.DeepEqual(existing.Spec.Ports, desiredPorts)
	if !needsUpdate {
		return nil
	}

	existing.Spec.Selector = selector
	existing.Spec.Ports = desiredPorts
	if _, err := svcClient.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update service %s/%s: %w", ns, svcName, err)
	}
	logger.Info("updated ClusterIP Service for member", "name", mc.Name, "service", svcName, "namespace", ns)
	return nil
}

// ensureServiceDeleted removes the ClusterIP Service for the member pod.
func ensureServiceDeleted(ctx context.Context, mc *MemberContext, deps *MemberDeps) error {
	svcClient, ns, err := resolveServiceClient(ctx, mc, deps)
	if err != nil {
		// If the backend doesn't support services (e.g. Docker), nothing to delete.
		return nil
	}

	svcName := serviceName(deps, mc)

	// Get first: skip deletion if Service does not exist.
	_, err = svcClient.Get(ctx, svcName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check service %s/%s: %w", ns, svcName, err)
	}

	// Service exists — delete it.
	if err := svcClient.Delete(ctx, svcName, metav1.DeleteOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete service %s/%s: %w", ns, svcName, err)
	}
	log.FromContext(ctx).Info("deleted ClusterIP Service for member", "name", mc.Name, "service", svcName, "namespace", ns)
	return nil
}

// resolveServiceClient obtains a K8sServiceClient from the detected backend.
// Returns an error if no backend supports Service management.
func resolveServiceClient(ctx context.Context, mc *MemberContext, deps *MemberDeps) (backend.K8sServiceClient, string, error) {
	if deps.Backend == nil {
		return nil, "", fmt.Errorf("no backend registry available")
	}
	sb := deps.Backend.FindServiceBackend(ctx, mc.DeployMode, mc.TargetClusterID, mc.TargetNamespace)
	if sb == nil {
		return nil, "", fmt.Errorf("no worker backend supports Service management")
	}
	return sb.ServiceClient(ctx, mc.DeployMode, mc.TargetClusterID, mc.TargetNamespace)
}

// serviceName returns the K8s Service name for the member, using the same
// prefix + name convention as Pod names (e.g. "hiclaw-worker-alice").
func serviceName(deps *MemberDeps, mc *MemberContext) string {
	return deps.ResourcePrefix.WorkerNamePrefix() + mc.Name
}

// serviceSelector builds the label selector that matches the member Pod.
// Uses the identity label stamped by createMemberContainer.
func serviceSelector(mc *MemberContext) map[string]string {
	return map[string]string{
		"agentteams.io/worker": mc.Name,
	}
}

// buildServicePorts converts spec.Expose entries into Kubernetes ServicePorts.
func buildServicePorts(mc *MemberContext) []corev1.ServicePort {
	ports := make([]corev1.ServicePort, 0, len(mc.Spec.Expose))
	for _, ep := range mc.Spec.Expose {
		proto := corev1.ProtocolTCP
		name := fmt.Sprintf("port-%d", ep.Port)
		ports = append(ports, corev1.ServicePort{
			Name:       name,
			Port:       int32(ep.Port),
			TargetPort: intstr.FromInt(ep.Port),
			Protocol:   proto,
		})
	}
	return ports
}
