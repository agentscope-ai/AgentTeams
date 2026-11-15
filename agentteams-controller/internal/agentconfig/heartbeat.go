package agentconfig

// ResolveLLMHeartbeat gives the Worker override precedence over Team policy.
// Missing ordinary-Worker policy must not inherit OpenClaw's implicit 30m wake.
func ResolveLLMHeartbeat(override *bool, inherited *HeartbeatConfig) HeartbeatConfig {
	hb := HeartbeatConfig{Every: "0m"}
	if inherited != nil {
		hb = *inherited
	}
	if override != nil {
		hb.Enabled = *override
	}
	if !hb.Enabled {
		hb.Every = "0m"
	} else if hb.Every == "" || hb.Every == "0m" {
		hb.Every = "30m"
	}
	return hb
}
