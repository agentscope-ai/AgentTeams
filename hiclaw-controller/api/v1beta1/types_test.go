package v1beta1

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// strPtr / boolPtr are tiny helpers used by the cross-cluster deployment
// serialization tests below. Kept package-private to avoid leaking generic
// helpers from the API package.
func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

// TestWorkerSpec_DeployFieldsJSONTags verifies the new cross-cluster
// deployment fields (DeployMode, TargetCluster, ServiceEnabled) marshal
// with stable, lowerCamelCase JSON keys and omit cleanly when nil.
func TestWorkerSpec_DeployFieldsJSONTags(t *testing.T) {
	cases := []struct {
		name    string
		spec    WorkerSpec
		wantSub []string // substrings expected in JSON
		absent  []string // substrings that must NOT appear in JSON
	}{
		{
			name: "local_with_target",
			spec: WorkerSpec{
				Model:      "m",
				DeployMode: strPtr("Local"),
				TargetCluster: &TargetClusterSpec{
					ID:        "c-123",
					Namespace: "agents",
				},
				ServiceEnabled: boolPtr(true),
			},
			wantSub: []string{`"deployMode":"Local"`, `"id":"c-123"`, `"namespace":"agents"`, `"serviceEnabled":true`},
		},
		{
			name: "remote_with_target",
			spec: WorkerSpec{
				Model:      "m",
				DeployMode: strPtr("Remote"),
				TargetCluster: &TargetClusterSpec{
					ID:        "c-remote",
					Namespace: "team-a",
				},
			},
			wantSub: []string{`"deployMode":"Remote"`, `"id":"c-remote"`, `"namespace":"team-a"`},
			absent:  []string{`"serviceEnabled"`},
		},
		{
			name:   "all_nil_omitted",
			spec:   WorkerSpec{Model: "m"},
			absent: []string{`"deployMode"`, `"targetCluster"`, `"serviceEnabled"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.spec)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			got := string(data)
			for _, sub := range tc.wantSub {
				if !strings.Contains(got, sub) {
					t.Errorf("JSON missing %q: %s", sub, got)
				}
			}
			for _, sub := range tc.absent {
				if strings.Contains(got, sub) {
					t.Errorf("JSON should omit %q: %s", sub, got)
				}
			}
		})
	}
}

// TestWorkerSpec_DeployFieldsRoundTrip verifies the new fields survive a
// JSON marshal/unmarshal cycle without value drift.
func TestWorkerSpec_DeployFieldsRoundTrip(t *testing.T) {
	orig := WorkerSpec{
		Model:      "m",
		DeployMode: strPtr("Remote"),
		TargetCluster: &TargetClusterSpec{
			ID:        "c-xyz",
			Namespace: "prod",
		},
		ServiceEnabled: boolPtr(false),
	}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got WorkerSpec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.DeployMode == nil || *got.DeployMode != "Remote" {
		t.Fatalf("DeployMode = %v, want *Remote", got.DeployMode)
	}
	if got.TargetCluster == nil || got.TargetCluster.ID != "c-xyz" || got.TargetCluster.Namespace != "prod" {
		t.Fatalf("TargetCluster = %+v", got.TargetCluster)
	}
	if got.ServiceEnabled == nil || *got.ServiceEnabled != false {
		t.Fatalf("ServiceEnabled = %v, want *false", got.ServiceEnabled)
	}
}

// TestWorkerSpec_BackwardCompatOldJSON verifies that JSON payloads written
// before the cross-cluster fields existed deserialize cleanly with all
// new pointer fields left nil.
func TestWorkerSpec_BackwardCompatOldJSON(t *testing.T) {
	old := []byte(`{"model":"m","runtime":"openclaw"}`)
	var got WorkerSpec
	if err := json.Unmarshal(old, &got); err != nil {
		t.Fatalf("Unmarshal old payload: %v", err)
	}
	if got.DeployMode != nil {
		t.Errorf("DeployMode should default to nil, got %v", *got.DeployMode)
	}
	if got.TargetCluster != nil {
		t.Errorf("TargetCluster should default to nil, got %+v", got.TargetCluster)
	}
	if got.ServiceEnabled != nil {
		t.Errorf("ServiceEnabled should default to nil, got %v", *got.ServiceEnabled)
	}
	if got.Channels != nil {
		t.Errorf("Channels should default to nil, got %+v", got.Channels)
	}
}

func TestWorkerSpec_DingTalkChannelsJSONRoundTrip(t *testing.T) {
	enabled := false
	orig := WorkerSpec{
		Model: "m",
		Channels: &ChannelsSpec{
			DingTalk: &DingTalkChannelSpec{
				Enabled:          &enabled,
				ClientID:         "demo-client-id",
				ClientSecret:     "test-client-secret",
				RobotCode:        "demo-robot-code",
				ShowThinking:     true,
				ShowToolCalls:    false,
				StreamingEnabled: true,
				MessageType:      "card",
				CardTemplateID:   "card-template-1",
			},
		},
	}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	gotJSON := string(data)
	for _, sub := range []string{
		`"channels"`,
		`"dingtalk"`,
		`"enabled":false`,
		`"clientId":"demo-client-id"`,
		`"clientSecret":"test-client-secret"`,
		`"robotCode":"demo-robot-code"`,
		`"showThinking":true`,
		`"streamingEnabled":true`,
		`"messageType":"card"`,
		`"cardTemplateId":"card-template-1"`,
	} {
		if !strings.Contains(gotJSON, sub) {
			t.Fatalf("JSON missing %q: %s", sub, gotJSON)
		}
	}

	var got WorkerSpec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Channels == nil || got.Channels.DingTalk == nil {
		t.Fatalf("channels.dingtalk missing after round-trip: %+v", got.Channels)
	}
	if got.Channels.DingTalk.Enabled == nil || *got.Channels.DingTalk.Enabled {
		t.Fatalf("enabled false was not preserved: %+v", got.Channels.DingTalk.Enabled)
	}
	if !reflect.DeepEqual(orig.Channels, got.Channels) {
		t.Fatalf("channels round-trip = %+v, want %+v", got.Channels, orig.Channels)
	}
}

// TestTargetClusterSpec_JSONTags pins down the TargetClusterSpec field tags
// so a future rename does not silently break stored CRs (the apiserver
// stores the JSON form in etcd via the structural CRD schema).
func TestTargetClusterSpec_JSONTags(t *testing.T) {
	spec := TargetClusterSpec{ID: "c-1", Namespace: "ns-1"}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"id":"c-1","namespace":"ns-1"}`
	if string(data) != want {
		t.Fatalf("Marshal = %s, want %s", data, want)
	}

	var back TargetClusterSpec
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back != spec {
		t.Fatalf("round-trip = %+v, want %+v", back, spec)
	}
}

// TestLeaderSpec_DeployFieldsRoundTrip verifies the same set of
// cross-cluster fields on LeaderSpec — the Leader path is exercised
// separately because Team admission paths embed LeaderSpec, not
// WorkerSpec.
func TestLeaderSpec_DeployFieldsRoundTrip(t *testing.T) {
	orig := LeaderSpec{
		Name:       "ld",
		Runtime:    "qwenpaw",
		Image:      "agentteams/qwenpaw-worker:v1",
		DeployMode: strPtr("Remote"),
		TargetCluster: &TargetClusterSpec{
			ID:        "c-leader",
			Namespace: "leaders",
		},
		ServiceEnabled: boolPtr(true),
	}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got LeaderSpec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Image != "agentteams/qwenpaw-worker:v1" {
		t.Fatalf("image = %q", got.Image)
	}
	if got.Runtime != "qwenpaw" {
		t.Fatalf("runtime = %q", got.Runtime)
	}
	if got.DeployMode == nil || *got.DeployMode != "Remote" {
		t.Fatalf("DeployMode = %v", got.DeployMode)
	}
	if got.TargetCluster == nil || got.TargetCluster.ID != "c-leader" {
		t.Fatalf("TargetCluster = %+v", got.TargetCluster)
	}
	if got.ServiceEnabled == nil || *got.ServiceEnabled != true {
		t.Fatalf("ServiceEnabled = %v", got.ServiceEnabled)
	}
}

// TestTeamWorkerSpec_DeployFieldsRoundTrip mirrors the round-trip
// assertion for TeamWorkerSpec, the third struct that carries the
// cross-cluster deployment triple.
func TestTeamWorkerSpec_DeployFieldsRoundTrip(t *testing.T) {
	orig := TeamWorkerSpec{
		Name:       "w1",
		DeployMode: strPtr("Local"),
	}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), `"deployMode":"Local"`) {
		t.Fatalf("Marshal = %s", data)
	}
	var got TeamWorkerSpec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.DeployMode == nil || *got.DeployMode != "Local" {
		t.Fatalf("DeployMode = %v", got.DeployMode)
	}
	if got.TargetCluster != nil {
		t.Errorf("TargetCluster should be nil when omitted, got %+v", got.TargetCluster)
	}
}

func TestWorkerSpec_CredentialContractRoundTrip(t *testing.T) {
	orig := WorkerSpec{
		Model: "m",
		AgentIdentity: &AgentIdentitySpec{
			WorkloadIdentityName: "wi-worker-a",
		},
		CredentialBindings: []CredentialBinding{{
			CredentialRef: CredentialRef{
				TokenVaultName:               "default",
				APIKeyCredentialProviderName: "GITHUB_TOKEN",
			},
			ToolWhitelist: []string{"gh"},
		}},
	}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	gotJSON := string(data)
	for _, want := range []string{
		`"agentIdentity":{"workloadIdentityName":"wi-worker-a"}`,
		`"credentialBindings":[{"credentialRef":{"tokenVaultName":"default","apiKeyCredentialProviderName":"GITHUB_TOKEN"},"toolWhitelist":["gh"]}]`,
	} {
		if !strings.Contains(gotJSON, want) {
			t.Fatalf("JSON missing %s: %s", want, gotJSON)
		}
	}
	var got WorkerSpec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.AgentIdentity == nil || got.AgentIdentity.WorkloadIdentityName != "wi-worker-a" {
		t.Fatalf("agentIdentity = %#v", got.AgentIdentity)
	}
	if len(got.CredentialBindings) != 1 {
		t.Fatalf("credentialBindings len=%d", len(got.CredentialBindings))
	}
	ref := got.CredentialBindings[0].CredentialRef
	if ref.TokenVaultName != "default" || ref.APIKeyCredentialProviderName != "GITHUB_TOKEN" {
		t.Fatalf("credentialRef = %#v", ref)
	}
	if got.CredentialBindings[0].ToolWhitelist[0] != "gh" {
		t.Fatalf("toolWhitelist = %#v", got.CredentialBindings[0].ToolWhitelist)
	}
}

func TestWorkerSpec_DeepCopyCredentialContract(t *testing.T) {
	src := WorkerSpec{
		Model: "m",
		AgentIdentity: &AgentIdentitySpec{
			WorkloadIdentityName: "wi-worker-a",
		},
		CredentialBindings: []CredentialBinding{{
			CredentialRef: CredentialRef{
				TokenVaultName:               "default",
				APIKeyCredentialProviderName: "GITHUB_TOKEN",
			},
			ToolWhitelist: []string{"gh"},
		}},
	}
	cp := *src.DeepCopy()

	src.AgentIdentity.WorkloadIdentityName = "mutated"
	src.CredentialBindings[0].CredentialRef.APIKeyCredentialProviderName = "MUTATED_TOKEN"
	src.CredentialBindings[0].ToolWhitelist[0] = "mutated-tool"

	if cp.AgentIdentity == nil || cp.AgentIdentity.WorkloadIdentityName != "wi-worker-a" {
		t.Fatalf("DeepCopy aliased AgentIdentity: %#v", cp.AgentIdentity)
	}
	if cp.CredentialBindings[0].CredentialRef.APIKeyCredentialProviderName != "GITHUB_TOKEN" {
		t.Fatalf("DeepCopy aliased CredentialBindings: %#v", cp.CredentialBindings)
	}
	if cp.CredentialBindings[0].ToolWhitelist[0] != "gh" {
		t.Fatalf("DeepCopy aliased ToolWhitelist: %#v", cp.CredentialBindings)
	}
}

// TestWorkerSpec_DeepCopyLabels verifies WorkerSpec.Labels is deep-copied:
// mutating the source map after DeepCopy must not mutate the copy. Covers
// nil, empty-but-non-nil, and populated variants because our hand-edited
// zz_generated.deepcopy.go has no code-gen safety net.
func TestWorkerSpec_DeepCopyLabels(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]string
	}{
		{name: "nil", in: nil},
		{name: "empty", in: map[string]string{}},
		{name: "populated", in: map[string]string{"owner": "alice", "env": "prod"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := WorkerSpec{Model: "m", Labels: tc.in}
			cp := *src.DeepCopy()

			if !reflect.DeepEqual(cp.Labels, src.Labels) {
				t.Fatalf("copy labels=%v want %v", cp.Labels, src.Labels)
			}
			if tc.in != nil {
				src.Labels["mutated"] = "x"
				if _, ok := cp.Labels["mutated"]; ok {
					t.Fatalf("DeepCopy did not isolate Labels: %v", cp.Labels)
				}
			}
		})
	}
}

func TestWorkerSpec_DeepCopyVolumesAndMounts(t *testing.T) {
	src := WorkerSpec{
		Model: "m",
		Volumes: []WorkerVolumeSpec{{
			Name: "worker-deps",
			Type: WorkerVolumeTypeOSS,
			OSS: &WorkerOSSVolumeSpec{
				Bucket:   "bucket-a",
				Endpoint: "https://oss.example.com",
				Auth: WorkerOSSAuthSpec{
					Type: "RRSA",
					RRSA: &WorkerOSSRRSASpec{RoleName: "role-a"},
				},
			},
		}},
		Mounts: []WorkerMountSpec{{
			Name:      "token",
			VolumeRef: "worker-deps",
			SubPath:   "instances/alice/token",
			MountPath: "/var/run/secrets/agentteams",
			ReadOnly:  true,
		}},
	}
	cp := *src.DeepCopy()
	src.Volumes[0].OSS.Auth.RRSA.RoleName = "mutated"
	src.Mounts[0].SubPath = "mutated"
	if cp.Volumes[0].OSS.Auth.RRSA.RoleName != "role-a" {
		t.Fatalf("DeepCopy aliased Volumes: %+v", cp.Volumes)
	}
	if cp.Mounts[0].SubPath != "instances/alice/token" {
		t.Fatalf("DeepCopy aliased Mounts: %+v", cp.Mounts)
	}
}

// TestManagerSpec_DeepCopyLabels mirrors the WorkerSpec assertion for
// ManagerSpec.Labels.
func TestManagerSpec_DeepCopyLabels(t *testing.T) {
	src := ManagerSpec{Model: "m", Labels: map[string]string{"tier": "ctrl"}}
	cp := *src.DeepCopy()
	if !reflect.DeepEqual(cp.Labels, src.Labels) {
		t.Fatalf("copy labels=%v want %v", cp.Labels, src.Labels)
	}
	src.Labels["mutated"] = "x"
	if _, ok := cp.Labels["mutated"]; ok {
		t.Fatalf("DeepCopy did not isolate ManagerSpec.Labels: %v", cp.Labels)
	}
	// Nil branch — ensure DeepCopy does not allocate an empty map for nil
	// input (preserves JSON omitempty round-trip stability).
	srcNil := ManagerSpec{Model: "m"}
	cpNil := *srcNil.DeepCopy()
	if cpNil.Labels != nil {
		t.Fatalf("expected nil Labels on deep-copy of nil source, got %v", cpNil.Labels)
	}
}

// TestLeaderSpec_DeepCopyLabels verifies LeaderSpec.Labels survives DeepCopy.
func TestLeaderSpec_DeepCopyLabels(t *testing.T) {
	src := LeaderSpec{Name: "ld", Labels: map[string]string{"role-hint": "planner"}}
	cp := *src.DeepCopy()
	if !reflect.DeepEqual(cp.Labels, src.Labels) {
		t.Fatalf("copy labels=%v want %v", cp.Labels, src.Labels)
	}
	src.Labels["role-hint"] = "mutated"
	if cp.Labels["role-hint"] != "planner" {
		t.Fatalf("DeepCopy aliased LeaderSpec.Labels: %v", cp.Labels)
	}
}

// TestTeamWorkerSpec_DeepCopyLabels verifies TeamWorkerSpec.Labels survives DeepCopy.
func TestTeamWorkerSpec_DeepCopyLabels(t *testing.T) {
	src := TeamWorkerSpec{Name: "w1", Labels: map[string]string{"skill": "rust"}}
	cp := *src.DeepCopy()
	if !reflect.DeepEqual(cp.Labels, src.Labels) {
		t.Fatalf("copy labels=%v want %v", cp.Labels, src.Labels)
	}
	src.Labels["skill"] = "mutated"
	if cp.Labels["skill"] != "rust" {
		t.Fatalf("DeepCopy aliased TeamWorkerSpec.Labels: %v", cp.Labels)
	}
}
