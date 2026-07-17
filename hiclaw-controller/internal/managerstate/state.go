// Package managerstate implements atomic read/write helpers for the OpenClaw
// Manager task-tracking state.json file. Semantics match
// manager/agent/skills/task-management/scripts/manage-state.sh.
package managerstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Task is one entry in state.json active_tasks.
type Task map[string]interface{}

// State is the on-disk Manager task board.
type State struct {
	AdminDMRoomID    interface{}              `json:"admin_dm_room_id"`
	ActiveTasks      []Task                   `json:"active_tasks"`
	CancelledTasks   []map[string]interface{} `json:"cancelled_tasks"`
	LastDigestSentAt interface{}              `json:"last_digest_sent_at"`
	UpdatedAt        string                   `json:"updated_at"`
}

// DefaultPath returns the Manager state.json path used by manage-state.sh.
// Prefer AGENTTEAMS_MANAGER_STATE_FILE, then $HOME (bash-compatible), then
// the OS user home directory. Checking $HOME first matters on Windows where
// os.UserHomeDir ignores HOME.
func DefaultPath() string {
	if p := strings.TrimSpace(os.Getenv("AGENTTEAMS_MANAGER_STATE_FILE")); p != "" {
		return p
	}
	if home := strings.TrimSpace(os.Getenv("HOME")); home != "" {
		return filepath.Join(home, "state.json")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "state.json"
	}
	return filepath.Join(home, "state.json")
}

// Store performs atomic state.json operations at path.
type Store struct {
	Path string
}

func (s *Store) path() string {
	if s.Path != "" {
		return s.Path
	}
	return DefaultPath()
}

func utcNow() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}

func (s *Store) load() (*State, error) {
	path := s.path()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		st := &State{
			AdminDMRoomID:    nil,
			ActiveTasks:      []Task{},
			CancelledTasks:   []map[string]interface{}{},
			LastDigestSentAt: nil,
			UpdatedAt:        utcNow(),
		}
		return st, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	if st.ActiveTasks == nil {
		st.ActiveTasks = []Task{}
	}
	if st.CancelledTasks == nil {
		st.CancelledTasks = []map[string]interface{}{}
	}
	if _, ok := st.AdminDMRoomID.(interface{}); !ok && st.AdminDMRoomID == nil {
		st.AdminDMRoomID = nil
	}
	return &st, nil
}

func (s *Store) save(st *State) error {
	st.UpdatedAt = utcNow()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := s.path()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Init ensures state.json exists with the expected shape.
func (s *Store) Init() (string, error) {
	st, err := s.load()
	if err != nil {
		return "", err
	}
	if err := s.save(st); err != nil {
		return "", err
	}
	return fmt.Sprintf("OK: state.json ready at %s", s.path()), nil
}

func taskString(t Task, key string) string {
	v, ok := t[key]
	if !ok || v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	default:
		return fmt.Sprint(x)
	}
}

func tasksMatch(a, b Task) bool {
	return taskString(a, "task_id") == taskString(b, "task_id") &&
		taskString(a, "title") == taskString(b, "title") &&
		taskString(a, "assigned_to") == taskString(b, "assigned_to")
}

func countTaskID(st *State, id string) int {
	n := 0
	for _, t := range st.ActiveTasks {
		if taskString(t, "task_id") == id {
			n++
		}
	}
	return n
}

func nextSuffixedID(st *State, base string) string {
	for suffix := 2; ; suffix++ {
		candidate := fmt.Sprintf("%s-%d", base, suffix)
		if countTaskID(st, candidate) == 0 {
			return candidate
		}
	}
}

// AddFinite registers a finite task.
func (s *Store) AddFinite(taskID, title, assignedTo, roomID, projectRoomID, delegatedToTeam string) (string, error) {
	st, err := s.load()
	if err != nil {
		return "", err
	}
	candidate := Task{
		"task_id":      taskID,
		"title":        title,
		"assigned_to":  assignedTo,
	}
	for _, t := range st.ActiveTasks {
		if tasksMatch(t, candidate) {
			return fmt.Sprintf("SKIP: task %s already in active_tasks", taskID), nil
		}
	}
	finalID := taskID
	if countTaskID(st, taskID) > 0 {
		finalID = nextSuffixedID(st, taskID)
	}
	entry := Task{
		"task_id":     finalID,
		"title":       title,
		"type":        "finite",
		"assigned_to": assignedTo,
		"room_id":     roomID,
	}
	if projectRoomID != "" {
		entry["project_room_id"] = projectRoomID
	}
	if delegatedToTeam != "" {
		entry["delegated_to_team"] = delegatedToTeam
	}
	st.ActiveTasks = append(st.ActiveTasks, entry)
	if err := s.save(st); err != nil {
		return "", err
	}
	if finalID != taskID {
		return fmt.Sprintf("OK: added finite task %s \"%s\" (assigned to %s) [id suffixed from %s due to collision]", finalID, title, assignedTo, taskID), nil
	}
	return fmt.Sprintf("OK: added finite task %s \"%s\" (assigned to %s)", finalID, title, assignedTo), nil
}

// AddInfinite registers a recurring task.
func (s *Store) AddInfinite(taskID, title, assignedTo, roomID, schedule, timezone, nextScheduledAt string) (string, error) {
	st, err := s.load()
	if err != nil {
		return "", err
	}
	if countTaskID(st, taskID) > 0 {
		return fmt.Sprintf("SKIP: task %s already in active_tasks", taskID), nil
	}
	st.ActiveTasks = append(st.ActiveTasks, Task{
		"task_id":           taskID,
		"title":             title,
		"type":              "infinite",
		"assigned_to":       assignedTo,
		"room_id":           roomID,
		"schedule":          schedule,
		"timezone":          timezone,
		"last_executed_at":  nil,
		"next_scheduled_at": nextScheduledAt,
	})
	if err := s.save(st); err != nil {
		return "", err
	}
	return fmt.Sprintf("OK: added infinite task %s \"%s\" (assigned to %s, next: %s)", taskID, title, assignedTo, nextScheduledAt), nil
}

// Complete removes a finite task from active_tasks.
func (s *Store) Complete(taskID string) (string, error) {
	st, err := s.load()
	if err != nil {
		return "", err
	}
	if countTaskID(st, taskID) == 0 {
		return fmt.Sprintf("SKIP: task %s not found in active_tasks", taskID), nil
	}
	filtered := make([]Task, 0, len(st.ActiveTasks))
	for _, t := range st.ActiveTasks {
		if taskString(t, "task_id") != taskID {
			filtered = append(filtered, t)
		}
	}
	st.ActiveTasks = filtered
	if err := s.save(st); err != nil {
		return "", err
	}
	return fmt.Sprintf("OK: removed task %s from active_tasks", taskID), nil
}

// Executed updates an infinite task after a run.
func (s *Store) Executed(taskID, nextScheduledAt string) (string, error) {
	st, err := s.load()
	if err != nil {
		return "", err
	}
	found := false
	for _, t := range st.ActiveTasks {
		if taskString(t, "task_id") == taskID && taskString(t, "type") == "infinite" {
			found = true
			break
		}
	}
	if !found {
		return fmt.Sprintf("WARN: infinite task %s not found in active_tasks (may be a legacy task not yet registered). Skipping update.", taskID), nil
	}
	now := utcNow()
	for i, t := range st.ActiveTasks {
		if taskString(t, "task_id") == taskID {
			t["last_executed_at"] = now
			t["next_scheduled_at"] = nextScheduledAt
			st.ActiveTasks[i] = t
			break
		}
	}
	if err := s.save(st); err != nil {
		return "", err
	}
	return fmt.Sprintf("OK: updated infinite task %s (last_executed_at=%s, next_scheduled_at=%s)", taskID, now, nextScheduledAt), nil
}

// SetAdminDM caches the admin DM room id.
func (s *Store) SetAdminDM(roomID string) (string, error) {
	st, err := s.load()
	if err != nil {
		return "", err
	}
	st.AdminDMRoomID = roomID
	if err := s.save(st); err != nil {
		return "", err
	}
	return fmt.Sprintf("OK: admin_dm_room_id set to %s", roomID), nil
}

// MarkBlocked marks a task blocked.
func (s *Store) MarkBlocked(taskID, reason string) (string, error) {
	st, err := s.load()
	if err != nil {
		return "", err
	}
	if countTaskID(st, taskID) == 0 {
		return fmt.Sprintf("SKIP: task %s not found in active_tasks", taskID), nil
	}
	now := utcNow()
	for i, t := range st.ActiveTasks {
		if taskString(t, "task_id") == taskID {
			t["status"] = "blocked"
			t["blocked_since"] = now
			if reason != "" {
				t["blocked_reason"] = reason
			}
			st.ActiveTasks[i] = t
			break
		}
	}
	if err := s.save(st); err != nil {
		return "", err
	}
	if reason != "" {
		return fmt.Sprintf("OK: task %s marked blocked (reason: %s)", taskID, reason), nil
	}
	return fmt.Sprintf("OK: task %s marked blocked (reason: none)", taskID), nil
}

// Unblock clears blocked status.
func (s *Store) Unblock(taskID string) (string, error) {
	st, err := s.load()
	if err != nil {
		return "", err
	}
	if countTaskID(st, taskID) == 0 {
		return fmt.Sprintf("SKIP: task %s not found in active_tasks", taskID), nil
	}
	for i, t := range st.ActiveTasks {
		if taskString(t, "task_id") == taskID {
			delete(t, "status")
			delete(t, "blocked_since")
			delete(t, "blocked_reason")
			st.ActiveTasks[i] = t
			break
		}
	}
	if err := s.save(st); err != nil {
		return "", err
	}
	return fmt.Sprintf("OK: task %s unblocked", taskID), nil
}

// Cancel removes a task and records it in cancelled_tasks.
func (s *Store) Cancel(taskID, reason string) (string, error) {
	st, err := s.load()
	if err != nil {
		return "", err
	}
	var removed Task
	found := false
	filtered := make([]Task, 0, len(st.ActiveTasks))
	for _, t := range st.ActiveTasks {
		if taskString(t, "task_id") == taskID {
			removed = t
			found = true
			continue
		}
		filtered = append(filtered, t)
	}
	if !found {
		return fmt.Sprintf("SKIP: task %s not found in active_tasks", taskID), nil
	}
	now := utcNow()
	record := map[string]interface{}{}
	for k, v := range removed {
		record[k] = v
	}
	record["cancelled_at"] = now
	record["cancel_reason"] = reason
	st.CancelledTasks = append(st.CancelledTasks, record)
	st.ActiveTasks = filtered
	if err := s.save(st); err != nil {
		return "", err
	}
	if reason != "" {
		return fmt.Sprintf("OK: cancelled task %s (reason: %s)", taskID, reason), nil
	}
	return fmt.Sprintf("OK: cancelled task %s (reason: none)", taskID), nil
}

// Reassign swaps assignee and room for a task.
func (s *Store) Reassign(taskID, assignedTo, roomID string) (string, error) {
	st, err := s.load()
	if err != nil {
		return "", err
	}
	if countTaskID(st, taskID) == 0 {
		return fmt.Sprintf("SKIP: task %s not found in active_tasks", taskID), nil
	}
	for i, t := range st.ActiveTasks {
		if taskString(t, "task_id") == taskID {
			t["assigned_to"] = assignedTo
			t["room_id"] = roomID
			st.ActiveTasks[i] = t
			break
		}
	}
	if err := s.save(st); err != nil {
		return "", err
	}
	return fmt.Sprintf("OK: reassigned task %s to %s (room %s)", taskID, assignedTo, roomID), nil
}

func formatAdminDM(v interface{}) string {
	if v == nil {
		return "null"
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// LastDigestGet returns last_digest_sent_at as plain text.
func (s *Store) LastDigestGet() (string, error) {
	st, err := s.load()
	if err != nil {
		return "", err
	}
	return formatAdminDM(st.LastDigestSentAt), nil
}

// LastDigestSet writes last_digest_sent_at.
func (s *Store) LastDigestSet(at string) (string, error) {
	if strings.TrimSpace(at) == "" {
		return "", errors.New("ERROR: 'last-digest set' requires --at ISO")
	}
	st, err := s.load()
	if err != nil {
		return "", err
	}
	st.LastDigestSentAt = at
	if err := s.save(st); err != nil {
		return "", err
	}
	return fmt.Sprintf("OK: last_digest_sent_at set to %s", at), nil
}

// List prints active tasks in the manage-state.sh human format.
func (s *Store) List() (string, error) {
	st, err := s.load()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Admin DM room: %s\n", formatAdminDM(st.AdminDMRoomID))
	if len(st.ActiveTasks) == 0 {
		b.WriteString("No active tasks.")
		return b.String(), nil
	}
	for _, t := range st.ActiveTasks {
		status := taskString(t, "status")
		if status == "" {
			status = "-"
		}
		title := taskString(t, "title")
		if title == "" {
			title = "-"
		}
		blockedSince := taskString(t, "blocked_since")
		if blockedSince == "" {
			blockedSince = "-"
		}
		if status == "blocked" {
			fmt.Fprintf(&b, "  [BLOCKED since %s] %s  type=%s  worker=%s  title=\"%s\"\n",
				blockedSince, taskString(t, "task_id"), taskString(t, "type"), taskString(t, "assigned_to"), title)
		} else {
			fmt.Fprintf(&b, "  %s  type=%s  worker=%s  title=\"%s\"\n",
				taskString(t, "task_id"), taskString(t, "type"), taskString(t, "assigned_to"), title)
		}
	}
	fmt.Fprintf(&b, "Total: %d active task(s). Updated: %s", len(st.ActiveTasks), st.UpdatedAt)
	return b.String(), nil
}

// Args mirrors manage-state.sh flag parsing.
type Args struct {
	Action          string
	TaskID          string
	Title           string
	AssignedTo      string
	RoomID          string
	ProjectRoomID   string
	DelegatedToTeam string
	TaskType        string
	Schedule        string
	Timezone        string
	NextScheduledAt string
	Reason          string
	SubAction       string
	At              string
}

// Run executes one manage-state action and returns stdout text.
func Run(store *Store, args Args) (string, error) {
	action := args.Action
	if action == "add" {
		switch strings.TrimSpace(args.TaskType) {
		case "", "finite":
			action = "add-finite"
		case "infinite":
			action = "add-infinite"
		default:
			return "", fmt.Errorf("ERROR: Unknown task type '%s' for legacy add action. Use: finite, infinite", args.TaskType)
		}
	}
	switch action {
	case "init":
		return store.Init()
	case "add-finite":
		if err := requireFields(map[string]string{
			"task-id": args.TaskID, "title": args.Title, "assigned-to": args.AssignedTo, "room-id": args.RoomID,
		}); err != nil {
			return "", err
		}
		return store.AddFinite(args.TaskID, args.Title, args.AssignedTo, args.RoomID, args.ProjectRoomID, args.DelegatedToTeam)
	case "add-infinite":
		if err := requireFields(map[string]string{
			"task-id": args.TaskID, "title": args.Title, "assigned-to": args.AssignedTo, "room-id": args.RoomID,
			"schedule": args.Schedule, "timezone": args.Timezone, "next-scheduled-at": args.NextScheduledAt,
		}); err != nil {
			return "", err
		}
		return store.AddInfinite(args.TaskID, args.Title, args.AssignedTo, args.RoomID, args.Schedule, args.Timezone, args.NextScheduledAt)
	case "complete":
		if err := requireFields(map[string]string{"task-id": args.TaskID}); err != nil {
			return "", err
		}
		return store.Complete(args.TaskID)
	case "executed":
		if err := requireFields(map[string]string{"task-id": args.TaskID, "next-scheduled-at": args.NextScheduledAt}); err != nil {
			return "", err
		}
		return store.Executed(args.TaskID, args.NextScheduledAt)
	case "set-admin-dm":
		if err := requireFields(map[string]string{"room-id": args.RoomID}); err != nil {
			return "", err
		}
		return store.SetAdminDM(args.RoomID)
	case "list":
		return store.List()
	case "mark-blocked":
		if err := requireFields(map[string]string{"task-id": args.TaskID}); err != nil {
			return "", err
		}
		return store.MarkBlocked(args.TaskID, args.Reason)
	case "unblock":
		if err := requireFields(map[string]string{"task-id": args.TaskID}); err != nil {
			return "", err
		}
		return store.Unblock(args.TaskID)
	case "cancel":
		if err := requireFields(map[string]string{"task-id": args.TaskID}); err != nil {
			return "", err
		}
		return store.Cancel(args.TaskID, args.Reason)
	case "reassign":
		if err := requireFields(map[string]string{"task-id": args.TaskID, "assigned-to": args.AssignedTo, "room-id": args.RoomID}); err != nil {
			return "", err
		}
		return store.Reassign(args.TaskID, args.AssignedTo, args.RoomID)
	case "last-digest":
		switch strings.TrimSpace(args.SubAction) {
		case "", "get":
			return store.LastDigestGet()
		case "set":
			return store.LastDigestSet(args.At)
		default:
			return "", fmt.Errorf("ERROR: Unknown last-digest subaction '%s'. Use: get, set", args.SubAction)
		}
	case "":
		return "", errors.New("usage: hiclaw manager-state --action <init|add-finite|add-infinite|complete|executed|set-admin-dm|list|mark-blocked|unblock|cancel|reassign|last-digest> [options]")
	default:
		return "", fmt.Errorf("ERROR: Unknown action '%s'. Use: init, add-finite, add-infinite, complete, executed, set-admin-dm, list, mark-blocked, unblock, cancel, reassign, last-digest", action)
	}
}

func requireFields(fields map[string]string) error {
	var missing []string
	for flag, val := range fields {
		if strings.TrimSpace(val) == "" {
			missing = append(missing, "--"+flag)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("ERROR: missing required arguments: %s", strings.Join(missing, " "))
}

// ParseArgs parses argv in manage-state.sh flag order.
func ParseArgs(argv []string) (Args, error) {
	var out Args
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		switch arg {
		case "--action":
			i++
			if i < len(argv) {
				out.Action = argv[i]
			}
		case "--task-id":
			i++
			if i < len(argv) {
				out.TaskID = argv[i]
			}
		case "--title", "--task-title":
			i++
			if i < len(argv) {
				out.Title = argv[i]
			}
		case "--assigned-to":
			i++
			if i < len(argv) {
				out.AssignedTo = argv[i]
			}
		case "--room-id":
			i++
			if i < len(argv) {
				out.RoomID = argv[i]
			}
		case "--project-room-id":
			i++
			if i < len(argv) {
				out.ProjectRoomID = argv[i]
			}
		case "--delegated-to-team":
			i++
			if i < len(argv) {
				out.DelegatedToTeam = argv[i]
			}
		case "--type":
			i++
			if i < len(argv) {
				out.TaskType = argv[i]
			}
		case "--schedule":
			i++
			if i < len(argv) {
				out.Schedule = argv[i]
			}
		case "--timezone":
			i++
			if i < len(argv) {
				out.Timezone = argv[i]
			}
		case "--next-scheduled-at":
			i++
			if i < len(argv) {
				out.NextScheduledAt = argv[i]
			}
		case "--reason":
			i++
			if i < len(argv) {
				out.Reason = argv[i]
			}
		case "--at":
			i++
			if i < len(argv) {
				out.At = argv[i]
			}
		case "get", "set":
			out.SubAction = arg
			if arg == "set" && i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "--") {
				i++
				out.At = argv[i]
			}
		default:
			return out, fmt.Errorf("Unknown argument: %s", arg)
		}
	}
	return out, nil
}
