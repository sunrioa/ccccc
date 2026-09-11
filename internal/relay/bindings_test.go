package relay

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBindingPersistenceAndConflicts(t *testing.T) {
	s, e := NewServer(t.TempDir(), "http://localhost", strings.Repeat("a", 64))
	must(t, e)
	a, b := newTestClient(t, s, testEngine(t), "Mac"), newTestClient(t, s, testEngine(t), "Windows")
	s.state.Devices[b.Config.DeviceID].Projects = append(s.state.Devices[b.Config.DeviceID].Projects, Project{Key: "other", Path: "/other"})
	req := bindingRequest{SourceDevice: a.Config.DeviceID, SourceProject: "my-app", TargetDevice: b.Config.DeviceID, TargetProject: "my-app"}
	status, data := requestTest(t, s.Handler(), "POST", "/api/bindings", s.Token, req)
	if status != 200 {
		t.Fatalf("bind: %d %s", status, data)
	}
	var binding ProjectBinding
	must(t, json.Unmarshal(data, &binding))
	if binding.A.Path == "" || binding.B.Path == "" {
		t.Fatal("missing persisted paths")
	}
	status, _ = requestTest(t, s.Handler(), "POST", "/api/bindings", s.Token, bindingRequest{SourceDevice: req.TargetDevice, SourceProject: req.TargetProject, TargetDevice: req.SourceDevice, TargetProject: req.SourceProject})
	if status != 200 || len(s.state.Bindings) != 1 {
		t.Fatal("reverse duplicate not idempotent")
	}
	req.TargetProject = "other"
	status, _ = requestTest(t, s.Handler(), "POST", "/api/bindings", s.Token, req)
	if status != 409 {
		t.Fatal("conflicting pair replaced")
	}
	req.TargetProject = "my-app"
	status, _ = requestTest(t, s.Handler(), "POST", "/api/bindings", a.Config.Token, req)
	if status != 401 {
		t.Fatal("agent can modify bindings")
	}
	req.TargetDevice = req.SourceDevice
	status, _ = requestTest(t, s.Handler(), "POST", "/api/bindings", s.Token, req)
	if status != 409 {
		t.Fatal("same-device binding accepted")
	}
	s2, e := NewServer(s.Dir, "http://localhost", s.Token)
	must(t, e)
	if len(s2.state.Bindings) != 1 {
		t.Fatal("binding lost on restart")
	}
	status, data = requestTest(t, s2.Handler(), "GET", "/api/state", s.Token, nil)
	if status != 200 || !strings.Contains(string(data), binding.ID) {
		t.Fatal("binding absent from UI state")
	}
	status, _ = requestTest(t, s2.Handler(), "DELETE", "/api/bindings/"+binding.ID, s2.Token, nil)
	if status != 200 {
		t.Fatal("unlink failed")
	}
	if len(s2.state.Bindings) != 0 {
		t.Fatal("unlink not persisted")
	}
	status, _ = requestTest(t, s2.Handler(), "POST", "/api/bindings", s2.Token, bindingRequest{SourceDevice: a.Config.DeviceID, SourceProject: "my-app", TargetDevice: b.Config.DeviceID, TargetProject: "absent"})
	if status != 400 {
		t.Fatal("unknown project accepted")
	}
}
func TestBindNewImportFromPreviewAfterEviction(t *testing.T) {
	s, e := NewServer(t.TempDir(), "http://localhost", strings.Repeat("a", 64))
	must(t, e)
	source, target := testEngine(t), testEngine(t)
	target.Config.ImportRoot = t.TempDir()
	fixture(t, source, "codex")
	sourceClient, targetClient := newTestClient(t, s, source, "source"), newTestClient(t, s, target, "target")
	queueTest(t, s, Job{Kind: "export", Provider: "codex", Project: "my-app", DeviceID: sourceClient.Config.DeviceID})
	exp := executeTest(t, sourceClient)
	var result struct {
		ID string `json:"snapshot_id"`
	}
	must(t, json.Unmarshal(exp.Result, &result))
	queueTest(t, s, Job{Kind: "preview", DeviceID: targetClient.Config.DeviceID, SnapshotID: result.ID, NewProject: true, Folder: "new-copy"})
	pre := executeTest(t, targetClient)
	if !s.state.Snapshots[result.ID].Purged {
		t.Fatal("fixture archive not evicted")
	}
	status, data := requestTest(t, s.Handler(), "POST", "/api/bindings", s.Token, bindingRequest{PreviewID: pre.ID})
	if status != 200 {
		t.Fatalf("preview binding: %d %s", status, data)
	}
	var binding ProjectBinding
	must(t, json.Unmarshal(data, &binding))
	if binding.B.Project != importKey("new-copy") || binding.B.Path != filepath.Join(canonicalPath(importRoot(target.Config)), "new-copy") {
		t.Fatal("new project mapping wrong")
	}
	if _, e = os.Stat(filepath.Join(binding.B.Path, ".git")); !os.IsNotExist(e) {
		t.Fatal("binding must not create repository contents")
	}
	status, data = requestTest(t, s.Handler(), "GET", "/api/state", s.Token, nil)
	if status != 200 || !strings.Contains(string(data), `"source"`) {
		t.Fatal("preview source not exposed after eviction")
	}
	// Binding itself queues no write; user still explicitly confirms restore.
	if len(s.state.Jobs) != 2 {
		t.Fatal("binding queued a mutation")
	}
	queueTest(t, s, Job{Kind: "restore", DeviceID: targetClient.Config.DeviceID, PreviewID: pre.ID})
	executeTest(t, targetClient)
}
