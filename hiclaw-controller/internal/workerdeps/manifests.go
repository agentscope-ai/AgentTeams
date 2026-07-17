package workerdeps

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	v1beta1 "github.com/hiclaw/hiclaw-controller/api/v1beta1"
	"github.com/hiclaw/hiclaw-controller/internal/backend"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

const (
	MountToken = "token"
	MountEnv   = "env"
	MountData  = "data"
)

const (
	defaultStorageCapacity      = "1Pi"
	accessKeyPVCapacity         = "50Gi"
	accessKeyStorageClass       = "test"
	accessKeyOtherOpts          = "-o umask=022 -o allow_other"
	ackOSSCSIProvisioner        = "ossplugin.csi.alibabacloud.com"
	authTypeRRSA                = "RRSA"
	authTypeAccessKey           = "AccessKey"
)

var (
	storageClassGVR       = schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}
	pvGVR                 = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "persistentvolumes"}
	pvcGVR                = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "persistentvolumeclaims"}
	credentialProviderGVR = schema.GroupVersionResource{Group: "agentidentity.alibabacloud.com", Version: "v1alpha1", Resource: "credentialproviders"}
	agentIdentityGVR      = schema.GroupVersionResource{Group: "agentidentity.alibabacloud.com", Version: "v1alpha1", Resource: "agentidentities"}
	agentRoleGVR          = schema.GroupVersionResource{Group: "agentidentity.alibabacloud.com", Version: "v1alpha1", Resource: "agentroles"}
	agentRoleBindingGVR   = schema.GroupVersionResource{Group: "agentidentity.alibabacloud.com", Version: "v1alpha1", Resource: "agentrolebindings"}
)

// Exported GVR aliases for controller tests and dynamic client wiring.
var (
	StorageClassGVR       = storageClassGVR
	PersistentVolumeGVR   = pvGVR
	PersistentVolumeClaimGVR = pvcGVR
	CredentialProviderGVR = credentialProviderGVR
	AgentIdentityGVR      = agentIdentityGVR
	AgentRoleGVR          = agentRoleGVR
	AgentRoleBindingGVR   = agentRoleBindingGVR
)

// CSIProvisioner is the ACK OSS CSI driver used for worker-deps volumes.
const CSIProvisioner = ackOSSCSIProvisioner

func MountResourceObjects(volume v1beta1.WorkerVolumeSpec, namespace string, builtIn bool) []*unstructured.Unstructured {
	if volume.OSS != nil && volume.OSS.Auth.Type == authTypeAccessKey {
		return []*unstructured.Unstructured{BuildAccessKeyPersistentVolume(volume, namespace)}
	}
	if builtIn && volume.OSS != nil && volume.OSS.Auth.Type == authTypeRRSA {
		return []*unstructured.Unstructured{
			BuildRRSAPersistentVolume(volume),
			BuildCredentialProvider(volume, namespace, MountEnv),
			BuildCredentialProvider(volume, namespace, MountToken),
			BuildCredentialProvider(volume, namespace, MountData),
			BuildAgentIdentity(namespace),
			BuildAgentRole(namespace),
			BuildAgentRoleBinding(namespace),
		}
	}
	return []*unstructured.Unstructured{
		BuildStorageClass(volume, namespace),
		BuildPersistentVolume(volume, namespace),
		BuildPersistentVolumeClaim(volume, namespace),
	}
}

func CreateObjectIfMissing(ctx context.Context, dynClient dynamic.Interface, obj *unstructured.Unstructured) error {
	gvr := ObjectGVR(obj)
	name := obj.GetName()
	ns := obj.GetNamespace()
	if ns != "" {
		res := dynClient.Resource(gvr).Namespace(ns)
		existing, err := res.Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			return UpdateObjectIfNeeded(ctx, res, existing, obj)
		}
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get workers-deps %s %s/%s: %w", obj.GetKind(), ns, name, err)
		}
		if _, err := res.Create(ctx, obj, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create workers-deps %s %s/%s: %w", obj.GetKind(), ns, name, err)
		}
		return nil
	}
	res := dynClient.Resource(gvr)
	existing, err := res.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return UpdateObjectIfNeeded(ctx, res, existing, obj)
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get workers-deps %s %s/%s: %w", obj.GetKind(), ns, name, err)
	}
	if _, err := res.Create(ctx, obj, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create workers-deps %s %s/%s: %w", obj.GetKind(), ns, name, err)
	}
	return nil
}

func UpdateObjectIfNeeded(ctx context.Context, res dynamic.ResourceInterface, existing, desired *unstructured.Unstructured) error {
	switch desired.GetKind() {
	case "CredentialProvider", "AgentIdentity", "AgentRole", "AgentRoleBinding":
	default:
		return nil
	}

	labels := map[string]string{}
	for k, v := range existing.GetLabels() {
		labels[k] = v
	}
	for k, v := range desired.GetLabels() {
		labels[k] = v
	}
	desiredSpec, ok, err := unstructured.NestedMap(desired.Object, "spec")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("desired %s spec is missing", desired.GetKind())
	}
	existingSpec, _, err := unstructured.NestedMap(existing.Object, "spec")
	if err != nil {
		return err
	}
	if reflect.DeepEqual(existing.GetLabels(), labels) && reflect.DeepEqual(existingSpec, desiredSpec) {
		return nil
	}

	updated := existing.DeepCopy()
	updated.SetLabels(labels)
	updated.Object["spec"] = desiredSpec
	if _, err := res.Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update workers-deps %s %s: %w", desired.GetKind(), desired.GetName(), err)
	}
	return nil
}

func ObjectGVR(obj *unstructured.Unstructured) schema.GroupVersionResource {
	switch obj.GetKind() {
	case "StorageClass":
		return storageClassGVR
	case "PersistentVolume":
		return pvGVR
	case "CredentialProvider":
		return credentialProviderGVR
	case "AgentIdentity":
		return agentIdentityGVR
	case "AgentRole":
		return agentRoleGVR
	case "AgentRoleBinding":
		return agentRoleBindingGVR
	default:
		return pvcGVR
	}
}

func BuildStorageClass(volume v1beta1.WorkerVolumeSpec, namespace string) *unstructured.Unstructured {
	provisioner := ackOSSCSIProvisioner
	parameters := map[string]interface{}{}
	if volume.OSS != nil {
		parameters["bucket"] = volume.OSS.Bucket
		parameters["url"] = volume.OSS.Endpoint
		parameters["path"] = "/"
		if volume.OSS.Auth.Type == "RRSA" && volume.OSS.Auth.RRSA != nil {
			parameters["authType"] = "rrsa"
			if volume.OSS.Auth.RRSA.RoleName != "" {
				parameters["roleName"] = volume.OSS.Auth.RRSA.RoleName
			}
			if volume.OSS.Auth.RRSA.RoleARN != "" {
				parameters["roleArn"] = volume.OSS.Auth.RRSA.RoleARN
			}
		}
		if volume.OSS.Auth.Type == "AccessKey" && volume.OSS.Auth.AccessKey != nil {
			parameters["authType"] = "ak"
			parameters["csi.storage.k8s.io/node-publish-secret-name"] = volume.OSS.Auth.AccessKey.SecretRef.Name
			parameters["csi.storage.k8s.io/node-publish-secret-namespace"] = defaultSecretNamespace(volume.OSS.Auth.AccessKey.SecretRef.Namespace, namespace)
		}
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "storage.k8s.io/v1",
		"kind":       "StorageClass",
		"metadata": map[string]interface{}{
			"name":   volume.Name,
			"labels": objectLabels(volume),
		},
		"provisioner":       provisioner,
		"reclaimPolicy":     "Retain",
		"volumeBindingMode": "Immediate",
		"parameters":        parameters,
	}}
}

func BuildPersistentVolume(volume v1beta1.WorkerVolumeSpec, namespace string) *unstructured.Unstructured {
	driver := ackOSSCSIProvisioner
	attrs := map[string]interface{}{}
	var secretRef map[string]interface{}
	if volume.OSS != nil {
		attrs["bucket"] = volume.OSS.Bucket
		attrs["url"] = volume.OSS.Endpoint
		attrs["path"] = "/"
		if volume.OSS.Auth.Type == "RRSA" && volume.OSS.Auth.RRSA != nil {
			attrs["authType"] = "rrsa"
			if volume.OSS.Auth.RRSA.RoleName != "" {
				attrs["roleName"] = volume.OSS.Auth.RRSA.RoleName
			}
			if volume.OSS.Auth.RRSA.RoleARN != "" {
				attrs["roleArn"] = volume.OSS.Auth.RRSA.RoleARN
			}
		}
		if volume.OSS.Auth.Type == "AccessKey" && volume.OSS.Auth.AccessKey != nil {
			secretRef = map[string]interface{}{
				"name":      volume.OSS.Auth.AccessKey.SecretRef.Name,
				"namespace": defaultSecretNamespace(volume.OSS.Auth.AccessKey.SecretRef.Namespace, namespace),
			}
		}
	}
	csi := map[string]interface{}{
		"driver":           driver,
		"volumeHandle":     volume.Name,
		"volumeAttributes": attrs,
	}
	if secretRef != nil && secretRef["name"] != "" {
		csi["nodePublishSecretRef"] = secretRef
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolume",
		"metadata": map[string]interface{}{
			"name":   volume.Name,
			"labels": objectLabels(volume),
		},
		"spec": map[string]interface{}{
			"capacity": map[string]interface{}{
				"storage": defaultStorageCapacity,
			},
			"accessModes":                   []interface{}{"ReadWriteMany"},
			"persistentVolumeReclaimPolicy": "Retain",
			"storageClassName":              volume.Name,
			"csi":                           csi,
		},
	}}
}

func BuildAccessKeyPersistentVolume(volume v1beta1.WorkerVolumeSpec, namespace string) *unstructured.Unstructured {
	attrs := map[string]interface{}{}
	secretRef := map[string]interface{}{}
	if volume.OSS != nil {
		attrs["bucket"] = volume.OSS.Bucket
		attrs["url"] = volume.OSS.Endpoint
		attrs["otherOpts"] = accessKeyOtherOpts
		if volume.OSS.Auth.AccessKey != nil {
			secretRef = map[string]interface{}{
				"name":      volume.OSS.Auth.AccessKey.SecretRef.Name,
				"namespace": namespace,
			}
		}
	}
	labels := objectLabels(volume)
	labels["alicloud-pvname"] = volume.Name
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolume",
		"metadata": map[string]interface{}{
			"name":   volume.Name,
			"labels": labels,
		},
		"spec": map[string]interface{}{
			"capacity": map[string]interface{}{
				"storage": accessKeyPVCapacity,
			},
			"accessModes":                   []interface{}{"ReadWriteMany"},
			"persistentVolumeReclaimPolicy": "Retain",
			"storageClassName":              accessKeyStorageClass,
			"volumeMode":                    "Filesystem",
			"csi": map[string]interface{}{
				"driver":               ackOSSCSIProvisioner,
				"nodePublishSecretRef": secretRef,
				"volumeAttributes":     attrs,
				"volumeHandle":         volume.Name,
			},
		},
	}}
}

func BuildRRSAPersistentVolume(volume v1beta1.WorkerVolumeSpec) *unstructured.Unstructured {
	attrs := map[string]interface{}{}
	if volume.OSS != nil {
		attrs["authType"] = "agent-identity"
		attrs["bucket"] = volume.OSS.Bucket
		attrs["url"] = volume.OSS.Endpoint
		attrs["otherOpts"] = accessKeyOtherOpts
	}
	labels := objectLabels(volume)
	labels["alicloud-pvname"] = volume.Name
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolume",
		"metadata": map[string]interface{}{
			"name":   volume.Name,
			"labels": labels,
		},
		"spec": map[string]interface{}{
			"capacity": map[string]interface{}{
				"storage": accessKeyPVCapacity,
			},
			"accessModes":                   []interface{}{"ReadWriteMany"},
			"persistentVolumeReclaimPolicy": "Retain",
			"storageClassName":              accessKeyStorageClass,
			"volumeMode":                    "Filesystem",
			"csi": map[string]interface{}{
				"driver":           ackOSSCSIProvisioner,
				"volumeAttributes": attrs,
				"volumeHandle":     volume.Name,
			},
		},
	}}
}

func CredentialProviderName(mountName string) string {
	return backend.BuiltinSandboxInstanceName + "-" + mountName
}

func BuildCredentialProvider(volume v1beta1.WorkerVolumeSpec, namespace, mountName string) *unstructured.Unstructured {
	roleName := ""
	if volume.OSS != nil && volume.OSS.Auth.RRSA != nil {
		roleName = volume.OSS.Auth.RRSA.RoleName
	}
	labels := objectLabels(volume)
	labels["agentteams.io/sandboxset"] = backend.BuiltinSandboxInstanceName
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "agentidentity.alibabacloud.com/v1alpha1",
		"kind":       "CredentialProvider",
		"metadata": map[string]interface{}{
			"name":      CredentialProviderName(mountName),
			"namespace": namespace,
			"labels":    labels,
		},
		"spec": map[string]interface{}{
			"type": "RAM",
			"ram": map[string]interface{}{
				"source": map[string]interface{}{
					"provider": "RRSA",
					"rrsa": map[string]interface{}{
						"roleName": roleName,
						"policy":   credentialProviderPolicy(),
					},
				},
			},
		},
	}}
}

func BuildAgentIdentity(namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "agentidentity.alibabacloud.com/v1alpha1",
		"kind":       "AgentIdentity",
		"metadata": map[string]interface{}{
			"name":      backend.BuiltinSandboxInstanceName,
			"namespace": namespace,
			"labels":    fixedObjectLabels(),
		},
		"spec": map[string]interface{}{
			"description": "this is for agentteams",
		},
	}}
}

func BuildAgentRole(namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "agentidentity.alibabacloud.com/v1alpha1",
		"kind":       "AgentRole",
		"metadata": map[string]interface{}{
			"name":      backend.BuiltinSandboxInstanceName,
			"namespace": namespace,
			"labels":    fixedObjectLabels(),
		},
		"spec": map[string]interface{}{
			"rules": []interface{}{
				agentRoleRule(CredentialProviderName(MountEnv)),
				agentRoleRule(CredentialProviderName(MountToken)),
				agentRoleRule(CredentialProviderName(MountData)),
			},
		},
	}}
}

func agentRoleRule(resource string) map[string]interface{} {
	return map[string]interface{}{
		"effect":   "Allow",
		"action":   "GetResourceCredential",
		"resource": "CredentialProvider/" + resource,
	}
}

func BuildAgentRoleBinding(namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "agentidentity.alibabacloud.com/v1alpha1",
		"kind":       "AgentRoleBinding",
		"metadata": map[string]interface{}{
			"name":      backend.BuiltinSandboxInstanceName,
			"namespace": namespace,
			"labels":    fixedObjectLabels(),
		},
		"spec": map[string]interface{}{
			"agentRoleRef": map[string]interface{}{
				"apiGroup": "agentidentity.alibabacloud.com",
				"kind":     "AgentRole",
				"name":     backend.BuiltinSandboxInstanceName,
			},
			"subjects": []interface{}{
				map[string]interface{}{
					"authorizationType": "Agent",
					"agentAuthorizationConfiguration": map[string]interface{}{
						"agentName": backend.BuiltinSandboxInstanceName,
					},
				},
			},
		},
	}}
}

func credentialProviderPolicy() string {
	policy := map[string]interface{}{
		"Version": "1",
		"Statement": []map[string]interface{}{
			{
				"Action": []string{
					"oss:GetObject",
					"oss:PutObject",
					"oss:DeleteObject",
					"oss:AbortMultipartUpload",
					"oss:ListMultipartUploads",
				},
				"Effect": "Allow",
				"Resource": []string{
					"acs:oss:*:*:${ack:agent-identity/storage-auth/bucket-name}/${ack:agent-identity/storage-auth/sub-path}",
					"acs:oss:*:*:${ack:agent-identity/storage-auth/bucket-name}/${ack:agent-identity/storage-auth/sub-path}/*",
				},
			},
			{
				"Action": []string{
					"oss:ListObjects",
				},
				"Effect": "Allow",
				"Resource": []string{
					"acs:oss:*:*:${ack:agent-identity/storage-auth/bucket-name}",
				},
				"Condition": map[string]interface{}{
					"StringLike": map[string]interface{}{
						"oss:Prefix": []string{
							"${ack:agent-identity/storage-auth/sub-path}/*",
						},
					},
				},
			},
		},
	}
	out, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		return ""
	}
	return string(out)
}

func defaultSecretNamespace(secretNamespace, fallback string) string {
	if secretNamespace != "" {
		return secretNamespace
	}
	return fallback
}

func BuildPersistentVolumeClaim(volume v1beta1.WorkerVolumeSpec, namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]interface{}{
			"name":      volume.Name,
			"namespace": namespace,
			"labels":    objectLabels(volume),
		},
		"spec": map[string]interface{}{
			"accessModes":      []interface{}{"ReadWriteMany"},
			"storageClassName": volume.Name,
			"volumeName":       volume.Name,
			"resources": map[string]interface{}{
				"requests": map[string]interface{}{
					"storage": defaultStorageCapacity,
				},
			},
		},
	}}
}

func objectLabels(volume v1beta1.WorkerVolumeSpec) map[string]interface{} {
	labels := fixedObjectLabels()
	labels["agentteams.io/mount-provider"] = strings.ToLower(volume.Type)
	return labels
}

func fixedObjectLabels() map[string]interface{} {
	return map[string]interface{}{
		"agentteams.io/managed-by":   "agentteams-controller",
		"agentteams.io/workers-deps": "true",
	}
}
