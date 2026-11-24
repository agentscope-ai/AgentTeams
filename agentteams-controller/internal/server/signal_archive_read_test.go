package server

import (
	"context"
	"strings"
	"testing"

	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/oss/ossfake"
)

type archiveReadTracker struct {
	*mcLikeOSS
	archivesRead []string
}

func (s *archiveReadTracker) GetObject(ctx context.Context, key string) ([]byte, error) {
	if strings.Contains(key, "-archive-") {
		s.archivesRead = append(s.archivesRead, key)
	}
	return s.mcLikeOSS.GetObject(ctx, key)
}

func TestSignalEventsSkipArchivesBeforeDownload(t *testing.T) {
	store, h := seedSignalProject(t)
	dir := "teams/alpha-team/shared/events/alpha-team/"
	for _, name := range []string{"p1-archive-20261124.json", "p1-archive-20261124-0123456789abcdef.json"} {
		if err := store.PutObject(context.Background(), dir+name, []byte(`{"archive":true,"events":[]}`)); err != nil {
			t.Fatal(err)
		}
	}
	putSignalEvent(t, store, dir+"ev_live.json", signalTrackerEvent("ev_live", "t1", "shared/tasks/t1/meta.json", "SIG-VERDICT", "2026-11-24T00:00:00Z"))
	tracker := &archiveReadTracker{mcLikeOSS: &mcLikeOSS{Memory: (*ossfake.Memory)(store)}}
	h.oss = tracker
	response := decodeSignalEvents(t, getSignalEvents(t, h, "p1", ""))
	if len(response.Events) != 1 || response.Events[0].EventID != "ev_live" {
		t.Fatalf("unexpected live events: %+v", response.Events)
	}
	if len(tracker.archivesRead) != 0 {
		t.Fatalf("downloaded archives: %v", tracker.archivesRead)
	}
}
