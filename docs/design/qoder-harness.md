# Experimental Qoder Harness registration

The native adapter lives in [agentteams-group/agentteams-harness-qoder](https://github.com/agentteams-group/agentteams-harness-qoder). The candidate targets Leader and Worker roles through the Qoder Agent SDK. Manager support is not implemented.

Core recognizes `runtime: qoder`, projects the standard member configuration, routes public Task notifications, and retains the existing execution and Matrix recovery contracts. Docker, Kubernetes and Sandbox creation require an explicit `spec.image`; there is no fallback to another Harness's image or an unpublished default tag. Qoder reads projected model configuration at turn boundaries instead of using the QwenPaw console hot-apply endpoints.

Build the adapter against this Core candidate and an `agt`/role-assets image from the same source. The independent repository supplies its Dockerfile, build entrypoint, native tests and opt-in real SDK probes. Pin the actual Core/Harness commits and built image digest before deploying. The local installer and default shared release matrix are not changed by this experimental registration.

The adapter requires unencrypted Matrix rooms and routes model inference through Higress. Qoder authentication and model-gateway authentication are separate. A BYOK payload field in the SDK is not evidence that a particular account can reach the installation's Higress endpoint; verify the live provider catalog and a real inference request. Never substitute the AT gateway provider ID for a Qoder catalog provider ID or silently fall back to Qoder service inference. Ordinary Model Studio Qwen models can remain upstream of Higress; this does not bypass Qoder account authentication or endpoint authorization.

Registration, native SDK tests and image smoke are not admission certification. Run the [Harness integration and acceptance guide](harness-integration-guide.md) against the same frozen candidate before claiming supported role combinations, full Task collaboration, restart recovery or bidirectional switching. Keep failed gateway probes and other incomplete evidence in the adapter's acceptance record.
