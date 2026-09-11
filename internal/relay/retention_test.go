package relay

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTemporaryArchiveEvictionKeepsLocalRestore(t *testing.T) {
	for _, keep := range []bool{false, true} {
		t.Run(map[bool]string{false: "temporary", true: "retained"}[keep], func(t *testing.T) {
			s, e := NewServer(t.TempDir(), "http://localhost", strings.Repeat("a", 64))
			must(t, e)
			a, b := testEngine(t), testEngine(t)
			fixture(t, a, "codex")
			ca, cb := newTestClient(t, s, a, "source"), newTestClient(t, s, b, "target")
			queueTest(t, s, Job{Kind: "export", Provider: "codex", Project: "my-app", DeviceID: ca.Config.DeviceID, KeepSnapshot: keep})
			exp := executeTest(t, ca)
			var result struct {
				ID string `json:"snapshot_id"`
			}
			must(t, json.Unmarshal(exp.Result, &result))
			snap := s.state.Snapshots[result.ID]
			if snap.Temporary == keep {
				t.Fatal("retention preference lost")
			}
			queueTest(t, s, Job{Kind: "preview", Project: "my-app", DeviceID: cb.Config.DeviceID, SnapshotID: snap.ID})
			pre := executeTest(t, cb)
			if snap.Purged == keep {
				t.Fatal("unexpected eviction")
			}
			_, err := os.Stat(filepath.Join(s.Dir, "bundles", snap.ID+".zip"))
			if !keep && !os.IsNotExist(err) {
				t.Fatal("temporary payload remains")
			}
			// Restart server with metadata only; applying the staged local plan still works.
			s2, e := NewServer(s.Dir, "http://localhost", s.Token)
			must(t, e)
			cb.HTTP.Transport = handlerTransport{s2.Handler()}
			queueTest(t, s2, Job{Kind: "restore", DeviceID: cb.Config.DeviceID, PreviewID: pre.ID})
			restore := executeTest(t, cb)
			queueTest(t, s2, Job{Kind: "rollback", DeviceID: cb.Config.DeviceID, RestoreID: restore.ID})
			executeTest(t, cb)
			if !keep {
				if e := cb.api(context.Background(), "GET", "/api/agent/bundles/"+snap.ID, nil, nil); e == nil {
					t.Fatal("evicted bundle downloadable")
				}
			}
		})
	}
}
func TestExpiryLegacyAndActiveTransfers(t *testing.T) {
	s, e := NewServer(t.TempDir(), "", strings.Repeat("a", 64))
	must(t, e)
	now := time.Now()
	for _, id := range []string{"legacy", "expired", "busy", "conflict", "queued"} {
		b := &Snapshot{ID: id, Temporary: id != "legacy", Expires: now.Add(-time.Hour)}
		if id == "conflict" {
			b.Expires = now.Add(time.Hour)
		}
		s.state.Snapshots[id] = b
		must(t, os.WriteFile(filepath.Join(s.Dir, "bundles", id+".zip"), []byte("payload"), 0600))
	}
	s.state.Jobs["busy"] = &Job{SnapshotID: "busy", Kind: "preview", Status: "running"}
	s.state.Jobs["queued"] = &Job{SnapshotID: "queued", Kind: "preview", Status: "queued"}
	s.state.Jobs["conflict"] = &Job{SnapshotID: "conflict", Kind: "preview", Status: "done", Result: json.RawMessage(`{"can_apply":false}`)}
	must(t, s.Cleanup(now))
	for _, id := range []string{"legacy", "busy", "conflict"} {
		if s.state.Snapshots[id].Purged {
			t.Fatalf("premature eviction: %s", id)
		}
	}
	for _, id := range []string{"expired", "queued"} {
		if !s.state.Snapshots[id].Purged {
			t.Fatalf("not expired: %s", id)
		}
	}
	if s.state.Jobs["queued"].Status != "failed" {
		t.Fatal("expired queue left active")
	}
	s.state.Jobs["busy"].Status = "failed"
	must(t, s.Cleanup(now))
	if !s.state.Snapshots["busy"].Purged {
		t.Fatal("failed transfer not expired")
	}
}
