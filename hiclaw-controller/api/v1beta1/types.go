// +k8s:deepcopy-gen=package

package v1beta1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

const (
	GroupName = "hiclaw.io"
	Version   = "v1beta1"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Worker represents an AI agent worker in HiClaw.
type Worker struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              WorkerSpec   `json:"spec"`
	Status            WorkerStatus `json:"status,omitempty"`
}

type WorkerSpec struct {
	Model      string   `json:"model"`
	Runtime    string   `json:"runtime,omitempty"` // openclaw | copaw (default: openclaw)
	Image      string   `json:"image,omitempty"`   // custom Docker image
	Identity   string   `json:"identity,omitempty"`
	Soul       string   `json:"soul,omitempty"`
	Agents     string   `json:"agents,omitempty"`
	Skills     []string `json:"skills,omitempty"`
	McpServers []string `json:"mcpServers,omitempty"`
	Package    string   `json:"package,omitempty"` // file://, http(s)://, or nacos:// URI
}

type WorkerStatus struct {
	Phase          string `json:"phase,omitempty"` // Pending/Running/Stopped/Failed
	MatrixUserID   string `json:"matrixUserID,omitempty"`
	RoomID         string `json:"roomID,omitempty"`
	ContainerState string `json:"containerState,omitempty"`
	LastHeartbeat  string `json:"lastHeartbeat,omitempty"`
	Message        string `json:"message,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type WorkerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Worker `json:"items"`
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Team represents a group of workers led by a Team Leader.
type Team struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              TeamSpec   `json:"spec"`
	Status            TeamStatus `json:"status,omitempty"`
}

type TeamSpec struct {
	Description string           `json:"description,omitempty"`
	Admin       *TeamAdminSpec   `json:"admin,omitempty"`
	Leader      LeaderSpec       `json:"leader"`
	Workers     []TeamWorkerSpec `json:"workers"`
}

type TeamAdminSpec struct {
	Name         string `json:"name"`
	MatrixUserID string `json:"matrixUserId,omitempty"`
}

type LeaderSpec struct {
	Name     string `json:"name"`
	Model    string `json:"model,omitempty"`
	Identity string `json:"identity,omitempty"`
	Soul     string `json:"soul,omitempty"`
	Agents   string `json:"agents,omitempty"`
	Package  string `json:"package,omitempty"`
}

type TeamWorkerSpec struct {
	Name       string   `json:"name"`
	Model      string   `json:"model,omitempty"`
	Runtime    string   `json:"runtime,omitempty"`
	Image      string   `json:"image,omitempty"`
	Identity   string   `json:"identity,omitempty"`
	Soul       string   `json:"soul,omitempty"`
	Agents     string   `json:"agents,omitempty"`
	Skills     []string `json:"skills,omitempty"`
	McpServers []string `json:"mcpServers,omitempty"`
	Package    string   `json:"package,omitempty"`
}

type TeamStatus struct {
	Phase        string `json:"phase,omitempty"` // Pending/Active/Degraded
	TeamRoomID   string `json:"teamRoomID,omitempty"`
	LeaderReady  bool   `json:"leaderReady,omitempty"`
	ReadyWorkers int    `json:"readyWorkers,omitempty"`
	TotalWorkers int    `json:"totalWorkers,omitempty"`
	Message      string `json:"message,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type TeamList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Team `json:"items"`
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Human represents a real human user with configurable access permissions.
type Human struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              HumanSpec   `json:"spec"`
	Status            HumanStatus `json:"status,omitempty"`
}

type HumanSpec struct {
	DisplayName       string   `json:"displayName"`
	Email             string   `json:"email,omitempty"`
	PermissionLevel   int      `json:"permissionLevel"` // 1=Admin, 2=Team, 3=Worker
	AccessibleTeams   []string `json:"accessibleTeams,omitempty"`
	AccessibleWorkers []string `json:"accessibleWorkers,omitempty"`
	Note              string   `json:"note,omitempty"`
}

type HumanStatus struct {
	Phase           string   `json:"phase,omitempty"` // Pending/Active/Failed
	MatrixUserID    string   `json:"matrixUserID,omitempty"`
	InitialPassword string   `json:"initialPassword,omitempty"` // Set on creation, shown once
	Rooms           []string `json:"rooms,omitempty"`
	EmailSent       bool     `json:"emailSent,omitempty"`
	Message      string   `json:"message,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

type HumanList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Human `json:"items"`
}
