package relay

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestProgressOwnedByRunningDevice(t *testing.T) {
	s, err := NewServer(t.TempDir(), "", strings.Repeat("p", 64))
	must(t, err)
	a, b := newTestClient(t, s, testEngine(t), "A"), newTestClient(t, s, testEngine(t), "B")
	j := queueTest(t, s, Job{Kind: "scan", DeviceID: a.Config.DeviceID})
	var claimed Job
	must(t, a.api(context.Background(), "POST", "/api/agent/claim", map[string]any{}, &claimed))
	path := "/api/agent/progress/" + j.ID
	status, _ := requestTest(t, s.Handler(), "POST", path, b.Config.Token, Progress{Stage: "upload", Done: 1, Total: 2})
	if status != 409 {
		t.Fatalf("other device changed progress: %d", status)
	}
	status, _ = requestTest(t, s.Handler(), "POST", path, a.Config.Token, Progress{Stage: "upload", Done: 3, Total: 2})
	if status != 400 {
		t.Fatal("invalid progress accepted")
	}
	status, _ = requestTest(t, s.Handler(), "POST", path, a.Config.Token, Progress{Stage: "upload", Done: 1, Total: 2})
	if status != 200 || s.state.Jobs[j.ID].Progress.Done != 1 {
		t.Fatal("progress not recorded")
	}
}

func TestNewSnapshotRejectsOldTargetClient(t *testing.T) {
	s, err := NewServer(t.TempDir(), "", strings.Repeat("v", 64))
	must(t, err)
	c := newTestClient(t, s, testEngine(t), "old target")
	s.state.Devices[c.Config.DeviceID].Version = "0.2.0"
	s.state.Snapshots["sample"] = &Snapshot{ID: "sample", Provider: "codex", MinClientVersion: "0.3.0"}
	status, b := requestTest(t, s.Handler(), "POST", "/api/jobs", s.Token, Job{Kind: "preview", DeviceID: c.Config.DeviceID, Project: "my-app", SnapshotID: "sample"})
	if status != 400 {
		t.Fatalf("old target accepted: %d %s", status, b)
	}
	var message map[string]string
	must(t, json.Unmarshal(b, &message))
	if !strings.Contains(message["error"], "升级") {
		t.Fatal("missing upgrade guidance")
	}
}
