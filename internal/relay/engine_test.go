package relay

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ahmojo/codex-claude-transfer/internal/claudehome"
)

const testSession = "aaaa1111-2222-4333-8444-555566667777"
const codexRel = "sessions/2026/09/10/rollout-2026-09-10T12-00-00-" + testSession + ".jsonl"

func testEngine(t *testing.T) *Engine {
	t.Helper()
	r := t.TempDir()
	p := filepath.Join(r, "code", "my-app")
	must(t, os.MkdirAll(p, 0700))
	return &Engine{Config: ClientConfig{DataDir: filepath.Join(r, "relay"), CodexHome: filepath.Join(r, "codex"), ClaudeHome: filepath.Join(r, "claude"), Projects: []Project{{Key: "my-app", Path: p}}}, guardOverride: func(string) error { return nil }, skipDiscovery: true}
}
func must(t *testing.T, e error) {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
}
func fixture(t *testing.T, e *Engine, provider string) string {
	t.Helper()
	cwd := e.Config.Projects[0].Path
	var path string
	var rows []any
	if provider == "codex" {
		path = filepath.Join(e.Config.CodexHome, filepath.FromSlash(codexRel))
		rows = []any{
			map[string]any{"timestamp": "2026-09-10T12:00:00Z", "type": "session_meta", "payload": map[string]any{"id": testSession, "timestamp": "2026-09-10T12:00:00Z", "originator": "codex_cli_rs", "cwd": cwd, "source": "cli", "model_provider": "openai", "cli_version": "0.144.6"}},
			map[string]any{"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": "保留图片和工具调用"}},
			map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call", "call_id": "call1", "name": "exec_command", "arguments": "{\"cmd\":\"echo ok\"}"}},
			map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call_output", "call_id": "call1", "output": strings.Repeat("tool output line\n", 100)}},
			map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,aGVsbG8="}}}},
		}
	} else {
		base := filepath.Join(e.Config.ClaudeHome, "projects", claudehome.EncodeCWD(cwd))
		path = filepath.Join(base, testSession+".jsonl")
		rows = []any{
			map[string]any{"type": "user", "uuid": "u1", "sessionId": testSession, "cwd": cwd, "timestamp": "2026-09-10T12:00:00Z", "message": map[string]any{"role": "user", "content": "hello Claude"}},
			map[string]any{"type": "assistant", "uuid": "a1", "parentUuid": "u1", "sessionId": testSession, "cwd": cwd, "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "t1", "name": "Read", "input": map[string]any{"file_path": "file.txt"}}}}},
		}
		must(t, atomicWrite(filepath.Join(base, testSession, "tool-results", "t1.txt"), []byte("完整外置结果")))
		child := map[string]any{"type": "assistant", "sessionId": testSession, "uuid": "child1", "isSidechain": true, "message": map[string]any{"role": "assistant", "content": "子代理记录（没有 cwd）"}}
		b, _ := json.Marshal(child)
		must(t, atomicWrite(filepath.Join(base, testSession, "subagents", "agent-test.jsonl"), append(b, '\n')))
		must(t, atomicWrite(filepath.Join(base, "memory", "MEMORY.md"), []byte("remember this project")))
	}
	var b bytes.Buffer
	for _, r := range rows {
		if provider == "codex" {
			r.(map[string]any)["timestamp"] = "2026-09-10T12:00:00Z"
		}
		must(t, json.NewEncoder(&b).Encode(r))
	}
	must(t, atomicWrite(path, b.Bytes()))
	return path
}
func appendRow(t *testing.T, path, provider, cwd string) {
	t.Helper()
	var row any
	if provider == "codex" {
		row = map[string]any{"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": "设备 B 上的新对话"}}
	} else {
		row = map[string]any{"type": "user", "uuid": "u2", "sessionId": testSession, "cwd": cwd, "message": map[string]any{"role": "user", "content": "设备 B 上的新对话"}}
	}
	f, e := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	must(t, e)
	must(t, json.NewEncoder(f).Encode(row))
	must(t, f.Close())
}
func TestRoundTripBothAgents(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			a, b := testEngine(t), testEngine(t)
			src := fixture(t, a, provider)
			original, _ := os.ReadFile(src)
			archive := filepath.Join(t.TempDir(), "out.zip")
			meta, err := a.Export(provider, "my-app", archive)
			must(t, err)
			if provider == "claude" && (meta.Sessions != 2 || len(meta.Extras) != 1) {
				t.Fatalf("lost child or tool result: %+v", meta)
			}
			plan, err := b.Preview(archive, "my-app", "preview1")
			must(t, err)
			if !plan.CanApply {
				t.Fatalf("plan blocked %+v", plan)
			}
			for _, c := range plan.Changes {
				if _, err = os.Stat(c.Path); !os.IsNotExist(err) {
					t.Fatal("preview wrote source data")
				}
			}
			_, err = b.Apply("preview1", "restore1")
			must(t, err)
			var dest string
			if provider == "codex" {
				dest = filepath.Join(b.Config.CodexHome, filepath.FromSlash(codexRel))
			} else {
				base := filepath.Join(b.Config.ClaudeHome, "projects", claudehome.EncodeCWD(b.Config.Projects[0].Path))
				dest = filepath.Join(base, testSession+".jsonl")
				for _, rel := range []string{testSession + "/subagents/agent-test.jsonl", testSession + "/tool-results/t1.txt", "memory/MEMORY.md"} {
					if _, err = os.Stat(filepath.Join(base, filepath.FromSlash(rel))); err != nil {
						t.Fatal("missing sidecar:", err)
					}
				}
			}
			data, err := os.ReadFile(dest)
			must(t, err)
			if !strings.Contains(string(data), "hello Claude") && provider == "claude" {
				t.Fatal("lost conversation")
			}
			if provider == "codex" && !bytes.Contains(data, []byte("data:image/png;base64,aGVsbG8=")) {
				t.Fatal("lost inline image")
			}
			appendRow(t, dest, provider, b.Config.Projects[0].Path)
			archive2 := filepath.Join(t.TempDir(), "return.zip")
			_, err = b.Export(provider, "my-app", archive2)
			must(t, err)
			plan2, err := a.Preview(archive2, "my-app", "preview2")
			must(t, err)
			if !plan2.CanApply {
				t.Fatalf("reverse sync conflicted: %+v", plan2)
			}
			_, err = a.Apply("preview2", "restore2")
			must(t, err)
			updated, _ := os.ReadFile(src)
			if !bytes.Contains(updated, []byte("设备 B 上的新对话")) {
				t.Fatal("reverse sync lost new turn")
			}
			_, err = a.Rollback("restore2")
			must(t, err)
			rolled, _ := os.ReadFile(src)
			if !bytes.Equal(rolled, original) {
				t.Fatal("rollback did not restore exact original")
			}
		})
	}
}
func TestChangedAfterPreviewAndRollbackRefuse(t *testing.T) {
	a, b := testEngine(t), testEngine(t)
	fixture(t, a, "claude")
	f := filepath.Join(t.TempDir(), "a.zip")
	_, err := a.Export("claude", "my-app", f)
	must(t, err)
	p, err := b.Preview(f, "my-app", "p1")
	must(t, err)
	must(t, atomicWrite(p.Changes[0].Path, []byte("newer local data")))
	if _, err = b.Apply("p1", "r1"); err == nil {
		t.Fatal("overwrote changed target")
	}
	must(t, os.Remove(p.Changes[0].Path))
	_, err = b.Apply("p1", "r2")
	must(t, err)
	must(t, atomicWrite(p.Changes[0].Path, []byte("new conversation after restore")))
	if _, err = b.Rollback("r2"); err == nil {
		t.Fatal("rollback overwrote newer conversation")
	}
}
func TestConflictBlocksWholeRestore(t *testing.T) {
	a, b := testEngine(t), testEngine(t)
	fixture(t, a, "claude")
	dst := fixture(t, b, "claude")
	must(t, atomicWrite(dst, []byte(`{"type":"user","sessionId":"`+testSession+`","cwd":"other","message":{"role":"user","content":"diverged"}}`+"\n")))
	f := filepath.Join(t.TempDir(), "a.zip")
	_, err := a.Export("claude", "my-app", f)
	must(t, err)
	p, err := b.Preview(f, "my-app", "conflict")
	must(t, err)
	if p.CanApply {
		t.Fatal("divergent transcript allowed")
	}
	if _, err = b.Apply("conflict", "bad"); err == nil {
		t.Fatal("applied conflict")
	}
}
func TestArchiveTamperingAndTraversal(t *testing.T) {
	a := testEngine(t)
	fixture(t, a, "claude")
	valid := filepath.Join(t.TempDir(), "valid.zip")
	_, err := a.Export("claude", "my-app", valid)
	must(t, err)
	for _, bad := range []string{"../escape", "extras/x/tool-results/NUL.txt", "tamper"} {
		t.Run(bad, func(t *testing.T) {
			z, err := zip.OpenReader(valid)
			must(t, err)
			defer z.Close()
			out := filepath.Join(t.TempDir(), "bad.zip")
			f, err := os.Create(out)
			must(t, err)
			w := zip.NewWriter(f)
			for _, entry := range z.File {
				r, err := entry.Open()
				must(t, err)
				data, err := io.ReadAll(r)
				r.Close()
				must(t, err)
				if bad == "tamper" && strings.HasPrefix(entry.Name, "extras/") {
					data = []byte("changed")
				}
				o, err := w.Create(entry.Name)
				must(t, err)
				_, err = o.Write(data)
				must(t, err)
			}
			if bad != "tamper" {
				o, err := w.Create(bad)
				must(t, err)
				_, err = o.Write([]byte("bad"))
				must(t, err)
			}
			must(t, w.Close())
			must(t, f.Close())
			if _, err = InspectArchive(out); err == nil {
				t.Fatal("accepted malicious archive")
			}
		})
	}
}
func TestSymlinkAndCrossToolMismatch(t *testing.T) {
	a, b := testEngine(t), testEngine(t)
	fixture(t, a, "claude")
	f := filepath.Join(t.TempDir(), "a.zip")
	_, err := a.Export("claude", "my-app", f)
	must(t, err)
	base := filepath.Join(b.Config.ClaudeHome, "projects", claudehome.EncodeCWD(b.Config.Projects[0].Path))
	must(t, os.MkdirAll(filepath.Dir(base), 0700))
	if err = os.Symlink(t.TempDir(), base); err != nil {
		t.Skip(err)
	}
	if _, err = b.Preview(f, "my-app", "symlink"); err == nil {
		t.Fatal("allowed symlink destination")
	}
}
func TestWindowsPortablePathRules(t *testing.T) {
	for _, p := range []string{"../x", "a/b:stream", "a/NUL.txt", "a/COM1", "a/file.", "a/../x", `a\b`} {
		if safeRel(p) == nil {
			t.Fatalf("accepted %s", p)
		}
	}
	if err := safeRel("session-id/tool-results/output.txt"); err != nil {
		t.Fatal(err)
	}
}
func TestNoWriteWhenAgentRunning(t *testing.T) {
	e := testEngine(t)
	fixture(t, e, "claude")
	e.guardOverride = func(string) error { return fmt.Errorf("agent busy") }
	out := filepath.Join(t.TempDir(), "out.zip")
	if _, err := e.Export("claude", "my-app", out); err == nil {
		t.Fatal("export should block")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("wrote archive while blocked")
	}
}

func TestNativeCodexDiscovery(t *testing.T) {
	if os.Getenv("RELAY_NATIVE_TEST") != "1" {
		t.Skip("opt-in: local Codex app-server against isolated fixture only")
	}
	a, b := testEngine(t), testEngine(t)
	fixture(t, a, "codex")
	f := filepath.Join(t.TempDir(), "native.zip")
	_, err := a.Export("codex", "my-app", f)
	must(t, err)
	_, err = b.Preview(f, "my-app", "native-preview")
	must(t, err)
	b.skipDiscovery = false
	result, err := b.Apply("native-preview", "native-restore")
	must(t, err)
	if warning, ok := result["discovery_warning"]; ok {
		t.Fatalf("native discovery failed: %v", warning)
	}
	if result["verified_threads"] != 1 {
		t.Fatalf("unexpected native verification: %+v", result)
	}
}
