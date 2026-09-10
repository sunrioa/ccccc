package relay

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverWithoutConfiguredProjects(t *testing.T) {
	e := testEngine(t)
	fixture(t, e, "codex")
	fixture(t, e, "claude")
	path := e.Config.Projects[0].Path
	e.Config.Projects = nil
	inv := e.Inventory()
	if len(e.Config.Projects) != 1 || e.Config.Projects[0].Path != path || !e.Config.Projects[0].Discovered {
		t.Fatalf("discovery failed: %+v", e.Config.Projects)
	}
	key := e.Config.Projects[0].Key
	counts := map[string]int{}
	for _, x := range inv {
		counts[x.Provider] += x.Count
	}
	if counts["codex"] != 1 || counts["claude"] != 2 {
		t.Fatalf("missing sessions: %+v", inv)
	}
	e.Inventory()
	if len(e.Config.Projects) != 1 || e.Config.Projects[0].Key != key {
		t.Fatal("unstable project identity")
	}
	// Session history remains exportable after the original code folder is gone.
	must(t, os.RemoveAll(path))
	e.Inventory()
	if !e.Config.Projects[0].Missing {
		t.Fatal("missing directory not reported")
	}
	for _, provider := range []string{"codex", "claude"} {
		_, err := e.Export(provider, key, filepath.Join(t.TempDir(), provider+".zip"))
		must(t, err)
	}
	e.Config.Projects = nil
	e.Config.DisableAutoScan = true
	e.Inventory()
	if len(e.Config.Projects) != 0 {
		t.Fatal("opt-out ignored")
	}
}

func TestNewProjectHTTPRoundTripBothTools(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			a, b := testEngine(t), testEngine(t)
			fixture(t, a, provider)
			a.Config.Projects = nil
			a.Inventory()
			sourceKey := a.Config.Projects[0].Key
			b.Config.Projects = nil
			b.Config.ImportRoot = filepath.Join(t.TempDir(), "new-projects")
			s, err := NewServer(t.TempDir(), "", "0123456789abcdef0123456789abcdef")
			must(t, err)
			ca, cb := newTestClient(t, s, a, "source"), newTestClient(t, s, b, "empty target")
			queueTest(t, s, Job{Kind: "export", Provider: provider, Project: sourceKey, DeviceID: ca.Config.DeviceID})
			ex := executeTest(t, ca)
			var exported struct {
				SnapshotID string `json:"snapshot_id"`
			}
			must(t, json.Unmarshal(ex.Result, &exported))
			queueTest(t, s, Job{Kind: "preview", NewProject: true, Folder: "另一台电脑独有的项目", SnapshotID: exported.SnapshotID, DeviceID: cb.Config.DeviceID})
			preview := executeTest(t, cb)
			var plan Plan
			must(t, json.Unmarshal(preview.Result, &plan))
			if !plan.CanApply || plan.Target != canonicalPath(filepath.Join(b.Config.ImportRoot, "另一台电脑独有的项目")) {
				t.Fatalf("bad plan: %+v", plan)
			}
			for _, change := range plan.Changes {
				if _, err = os.Stat(change.Path); !os.IsNotExist(err) {
					t.Fatal("preview wrote session")
				}
			}
			queueTest(t, s, Job{Kind: "restore", DeviceID: cb.Config.DeviceID, PreviewID: preview.ID})
			restore := executeTest(t, cb)
			// Reopening without any hand-written projects recovers the mapping.
			config := b.Config
			config.Projects = nil
			reopened := &Engine{Config: config, guardOverride: b.guardOverride, skipDiscovery: true}
			reopened.Inventory()
			if len(reopened.Config.Projects) != 1 || reopened.Config.Projects[0].Key != plan.Project {
				t.Fatal("mapping not persisted")
			}
			var transcript string
			if provider == "codex" {
				transcript = filepath.Join(b.Config.CodexHome, filepath.FromSlash(codexRel))
			} else {
				scan, err := b.scan(provider)
				must(t, err)
				for _, session := range scan.Sessions {
					if filepath.Base(session.Path) == testSession+".jsonl" {
						transcript = session.Path
					}
				}
			}
			if transcript == "" {
				t.Fatal("main session not found")
			}
			appendRow(t, transcript, provider, plan.Target)
			inv := cb.refreshInventory()
			must(t, cb.heartbeat(context.Background(), inv))
			queueTest(t, s, Job{Kind: "export", DeviceID: cb.Config.DeviceID, Project: plan.Project, Provider: provider})
			reverse := executeTest(t, cb)
			must(t, json.Unmarshal(reverse.Result, &exported))
			queueTest(t, s, Job{Kind: "preview", DeviceID: ca.Config.DeviceID, Project: sourceKey, SnapshotID: exported.SnapshotID})
			back := executeTest(t, ca)
			must(t, json.Unmarshal(back.Result, &plan))
			if !plan.CanApply {
				t.Fatalf("reverse import blocked: %+v", plan)
			}
			queueTest(t, s, Job{Kind: "restore", DeviceID: ca.Config.DeviceID, PreviewID: back.ID})
			executeTest(t, ca)
			// Restore rollback must protect the new local continuation.
			if _, err = b.Rollback(restore.ID); err == nil {
				t.Fatal("rollback overwrote new continuation")
			}
		})
	}
}

func TestNewProjectRejectsUnsafeFolders(t *testing.T) {
	e := testEngine(t)
	e.Config.ImportRoot = filepath.Join(t.TempDir(), "imports")
	for _, folder := range []string{"../escape", "C:\\code", "/absolute", "nested/path", "CON", "bad.", "..", ""} {
		if _, err := e.PrepareImport(importKey(folder), folder); err == nil {
			t.Fatalf("allowed %q", folder)
		}
	}
	must(t, os.MkdirAll(e.Config.ImportRoot, 0700))
	must(t, os.Symlink(t.TempDir(), filepath.Join(e.Config.ImportRoot, "link")))
	if _, err := e.PrepareImport(importKey("link"), "link"); err == nil {
		t.Fatal("followed symlink")
	}
}

func TestNativeCodexNewProjectDiscovery(t *testing.T) {
	if os.Getenv("RELAY_NATIVE_TEST") != "1" {
		t.Skip("opt-in isolated native Codex test")
	}
	a, b := testEngine(t), testEngine(t)
	fixture(t, a, "codex")
	archive := filepath.Join(t.TempDir(), "new-native.zip")
	_, err := a.Export("codex", "my-app", archive)
	must(t, err)
	b.Config.Projects = nil
	b.Config.ImportRoot = filepath.Join(t.TempDir(), "imports")
	project, err := b.PrepareImport(importKey("win-only-project"), "win-only-project")
	must(t, err)
	_, err = b.Preview(archive, project.Key, "new-native-preview")
	must(t, err)
	b.skipDiscovery = false
	result, err := b.Apply("new-native-preview", "new-native-restore")
	must(t, err)
	if warning, ok := result["discovery_warning"]; ok {
		t.Fatalf("native discovery failed: %v", warning)
	}
	if result["verified_threads"] != 1 {
		t.Fatalf("native verification: %+v", result)
	}
}
