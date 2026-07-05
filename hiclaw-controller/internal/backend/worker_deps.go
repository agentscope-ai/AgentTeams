package backend

const (
	sandboxWorkerDepsPVName              = "agentteams"
	sandboxWorkerDepsBaseSubPath         = "workers-deps"
	sandboxWorkerEnvMountPath            = "/mnt/agentteams/env"
	sandboxWorkerTokenMountPath          = "/var/run/secrets/agentteams"
	sandboxWorkerTokenCredentialProvider = "agentteams-token"
)

// BuildSandboxWorkerDeps carries the per-Worker runtime material to sandbox
// providers and adds the standard mounts consumed by the sandbox runtime.
func BuildSandboxWorkerDeps(name string, env map[string]string, authToken string, existing *WorkerDepsSpec) *WorkerDepsSpec {
	deps := cloneWorkerDeps(existing)
	if deps == nil {
		deps = &WorkerDepsSpec{}
	}
	if len(env) > 0 {
		if deps.Env == nil {
			deps.Env = cloneStringMap(env)
		} else {
			for k, v := range env {
				if _, ok := deps.Env[k]; !ok {
					deps.Env[k] = v
				}
			}
		}
		deps.DynamicVolumeMounts = appendMountIfMissing(deps.DynamicVolumeMounts, DynamicVolumeMount{
			PVName:    sandboxWorkerDepsPVName,
			MountPath: sandboxWorkerEnvMountPath,
			SubPath:   sandboxWorkerDepsBaseSubPath + "/" + name + "/env",
			ReadOnly:  true,
		})
	}
	if authToken != "" {
		if deps.AuthToken == "" {
			deps.AuthToken = authToken
		}
		deps.DynamicVolumeMounts = appendMountIfMissing(deps.DynamicVolumeMounts, DynamicVolumeMount{
			PVName:    sandboxWorkerDepsPVName,
			MountPath: sandboxWorkerTokenMountPath,
			SubPath:   sandboxWorkerDepsBaseSubPath + "/" + name + "/token",
			ReadOnly:  true,
			Attributes: map[string]string{
				"credentialProviderName": sandboxWorkerTokenCredentialProvider,
			},
		})
	}
	if deps.InplaceUpdateImage == "" && len(deps.Env) == 0 && deps.AuthToken == "" && len(deps.DynamicVolumeMounts) == 0 {
		return nil
	}
	return deps
}

func cloneWorkerDeps(in *WorkerDepsSpec) *WorkerDepsSpec {
	if in == nil {
		return nil
	}
	out := &WorkerDepsSpec{
		InplaceUpdateImage: in.InplaceUpdateImage,
		Env:                cloneStringMap(in.Env),
		AuthToken:          in.AuthToken,
	}
	if len(in.DynamicVolumeMounts) > 0 {
		out.DynamicVolumeMounts = make([]DynamicVolumeMount, 0, len(in.DynamicVolumeMounts))
		for _, mount := range in.DynamicVolumeMounts {
			out.DynamicVolumeMounts = append(out.DynamicVolumeMounts, DynamicVolumeMount{
				PVName:     mount.PVName,
				MountPath:  mount.MountPath,
				SubPath:    mount.SubPath,
				ReadOnly:   mount.ReadOnly,
				Attributes: cloneStringMap(mount.Attributes),
			})
		}
	}
	return out
}

func appendMountIfMissing(mounts []DynamicVolumeMount, mount DynamicVolumeMount) []DynamicVolumeMount {
	for _, existing := range mounts {
		if existing.MountPath == mount.MountPath {
			return mounts
		}
	}
	return append(mounts, mount)
}
