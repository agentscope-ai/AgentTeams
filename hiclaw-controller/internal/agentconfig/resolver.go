package agentconfig

import (
	"context"
	"fmt"
	"strings"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/oss"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Resolver loads agent configuration files (SOUL.md, AGENTS.md) from external
// sources referenced by AgentConfigRef. It supports three source types:
//   - configmap: reads from a Kubernetes ConfigMap
//   - minio: reads from MinIO/object storage
//   - nacos: reserved for future implementation (returns empty for now)
type Resolver struct {
	k8sClient    kubernetes.Interface
	ossClient    oss.StorageClient
}

// NewResolver creates a new agent config resolver.
// k8sClient is required for configmap source; ossClient is required for minio source.
// Either may be nil if the corresponding source type is not used.
func NewResolver(k8sClient kubernetes.Interface, ossClient oss.StorageClient) *Resolver {
	return &Resolver{
		k8sClient: k8sClient,
		ossClient: ossClient,
	}
}

// Resolve loads content for the given fileName (e.g., "SOUL.md", "AGENTS.md")
// from the external source specified in ref.
// Returns ("", nil) if ref is nil.
// Returns an error if the source is unsupported or the content cannot be loaded.
func (r *Resolver) Resolve(ctx context.Context, ref *v1beta1.AgentConfigRef, fileName string) (string, error) {
	if ref == nil {
		return "", nil
	}

	switch ref.Source {
	case "configmap":
		return r.resolveFromConfigMap(ctx, ref.URI, fileName)
	case "minio":
		return r.resolveFromMinIO(ctx, ref.URI, fileName)
	case "nacos":
		// TODO: implement Nacos AgentSpec resource extraction.
		// The existing executor.NacosAIClient.GetAgentSpec() downloads all
		// resources to disk; a single-file read API should be added there first.
		return "", fmt.Errorf("nacos source not yet implemented for agentConfigRef")
	default:
		return "", fmt.Errorf("unknown agentConfigRef source: %q", ref.Source)
	}
}

// resolveFromConfigMap reads a key from a ConfigMap.
// URI format: "namespace/name"
func (r *Resolver) resolveFromConfigMap(ctx context.Context, uri, fileName string) (string, error) {
	if r.k8sClient == nil {
		return "", fmt.Errorf("kubernetes client not available for configmap source")
	}

	parts := strings.SplitN(uri, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("invalid configmap URI %q: expected namespace/name", uri)
	}
	namespace, name := parts[0], parts[1]

	cm, err := r.k8sClient.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get configmap %s/%s: %w", namespace, name, err)
	}

	content, ok := cm.Data[fileName]
	if !ok {
		// Also check binary data
		if binData, binOk := cm.BinaryData[fileName]; binOk {
			return string(binData), nil
		}
		return "", fmt.Errorf("key %q not found in configmap %s/%s (available keys: %s)",
			fileName, namespace, name, availableKeys(cm))
	}

	return content, nil
}

// resolveFromMinIO reads an object from MinIO/object storage.
// URI format: "key-prefix" (the full key becomes "key-prefix/fileName")
func (r *Resolver) resolveFromMinIO(ctx context.Context, uri, fileName string) (string, error) {
	if r.ossClient == nil {
		return "", fmt.Errorf("OSS client not available for minio source")
	}

	key := strings.TrimSuffix(uri, "/") + "/" + fileName
	data, err := r.ossClient.GetObject(ctx, key)
	if err != nil {
		return "", fmt.Errorf("get object %q: %w", key, err)
	}

	return string(data), nil
}

// availableKeys returns a comma-separated list of keys in a ConfigMap for error messages.
func availableKeys(cm *corev1.ConfigMap) string {
	keys := make([]string, 0, len(cm.Data)+len(cm.BinaryData))
	for k := range cm.Data {
		keys = append(keys, k)
	}
	for k := range cm.BinaryData {
		keys = append(keys, k+" (binary)")
	}
	if len(keys) == 0 {
		return "<empty>"
	}
	return strings.Join(keys, ", ")
}
