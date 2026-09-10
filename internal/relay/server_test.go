package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type handlerTransport struct{ h http.Handler }

func (tr handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	r.RemoteAddr = "127.0.0.1:12345"
	tr.h.ServeHTTP(w, r)
	return w.Result(), nil
}
func requestTest(t *testing.T, h http.Handler, method, path, token string, v any) (int, []byte) {
	t.Helper()
	var body io.Reader
	if v != nil {
		b, e := json.Marshal(v)
		must(t, e)
		body = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, "http://localhost"+path, body)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.Bytes()
}
func newTestClient(t *testing.T, s *Server, engine *Engine, name string) *Client {
	t.Helper()
	status, b := requestTest(t, s.Handler(), "POST", "/api/devices", s.Token, map[string]string{"name": name, "server": "http://localhost"})
	if status != 200 {
		t.Fatalf("device enrollment: %d %s", status, b)
	}
	var config ClientConfig
	must(t, json.Unmarshal(b, &config))
	engine.Config.DeviceID = config.DeviceID
	engine.Config.Token = config.Token
	engine.Config.Server = config.Server
	c, e := NewClient(engine.Config)
	must(t, e)
	c.Engine = engine
	c.HTTP.Transport = handlerTransport{s.Handler()}
	must(t, c.heartbeat(context.Background(), engine.Inventory()))
	return c
}
func queueTest(t *testing.T, s *Server, j Job) Job {
	t.Helper()
	status, b := requestTest(t, s.Handler(), "POST", "/api/jobs", s.Token, j)
	if status != 200 {
		t.Fatalf("queue: %d %s", status, b)
	}
	var out Job
	must(t, json.Unmarshal(b, &out))
	return out
}
func executeTest(t *testing.T, c *Client) Job {
	t.Helper()
	var j *Job
	ctx := context.Background()
	must(t, c.api(ctx, "POST", "/api/agent/claim", map[string]any{}, &j))
	if j == nil {
		t.Fatal("no queued job")
	}
	v, e := c.execute(ctx, *j)
	must(t, e)
	j.Result, e = json.Marshal(v)
	must(t, e)
	must(t, c.api(ctx, "POST", "/api/agent/result/"+j.ID, Completion{Result: j.Result}, j))
	if j.Status != "done" {
		t.Fatal("completion missing")
	}
	return *j
}
func TestRelayHTTPFullFlow(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			s, e := NewServer(t.TempDir(), "http://localhost", strings.Repeat("a", 64))
			must(t, e)
			a, b := testEngine(t), testEngine(t)
			fixture(t, a, provider)
			ca := newTestClient(t, s, a, "Mac")
			cb := newTestClient(t, s, b, "Windows target fixture")
			queueTest(t, s, Job{Kind: "export", DeviceID: ca.Config.DeviceID, Project: "my-app", Provider: provider})
			j := executeTest(t, ca)
			var exp struct {
				SnapshotID string `json:"snapshot_id"`
			}
			must(t, json.Unmarshal(j.Result, &exp))
			if exp.SnapshotID == "" {
				t.Fatal("missing uploaded snapshot")
			}
			queueTest(t, s, Job{Kind: "preview", DeviceID: cb.Config.DeviceID, Project: "my-app", SnapshotID: exp.SnapshotID})
			pre := executeTest(t, cb)
			var p Plan
			must(t, json.Unmarshal(pre.Result, &p))
			if !p.CanApply {
				t.Fatalf("plan blocked %+v", p)
			}
			queueTest(t, s, Job{Kind: "restore", DeviceID: cb.Config.DeviceID, PreviewID: pre.ID})
			restore := executeTest(t, cb)
			for _, c := range p.Changes {
				if mutates(c) {
					h, e := hashFile(c.Path)
					must(t, e)
					if h != c.After {
						t.Fatal("restored checksum mismatch")
					}
				}
			}
			queueTest(t, s, Job{Kind: "rollback", DeviceID: cb.Config.DeviceID, RestoreID: restore.ID})
			executeTest(t, cb)
			for _, c := range p.Changes {
				if mutates(c) {
					h, e := fileHash(c.Path)
					must(t, e)
					if h != c.Before {
						t.Fatal("rollback checksum mismatch")
					}
				}
			}
			s2, e := NewServer(s.Dir, "http://localhost", s.Token)
			must(t, e)
			if len(s2.state.Snapshots) != 1 || len(s2.state.Jobs) != 4 {
				t.Fatal("persistent state lost")
			}
		})
	}
}
func TestAuthAndDeviceIsolation(t *testing.T) {
	s, e := NewServer(t.TempDir(), "http://localhost", strings.Repeat("b", 64))
	must(t, e)
	h := s.Handler()
	for _, path := range []string{"/api/state", "/api/snapshots/bad"} {
		status, _ := requestTest(t, h, "GET", path, "", nil)
		if status != 401 {
			t.Fatal("unauthenticated read allowed")
		}
	}
	a := newTestClient(t, s, testEngine(t), "A")
	b := newTestClient(t, s, testEngine(t), "B")
	status, _ := requestTest(t, h, "POST", "/api/jobs", a.Config.Token, Job{Kind: "export", DeviceID: a.Config.DeviceID, Provider: "codex", Project: "my-app"})
	if status != 401 {
		t.Fatal("device can create admin jobs")
	}
	j := queueTest(t, s, Job{Kind: "export", DeviceID: a.Config.DeviceID, Provider: "codex", Project: "my-app"})
	status, _ = requestTest(t, h, "POST", "/api/agent/result/"+j.ID, b.Config.Token, Completion{})
	if status != 404 {
		t.Fatal("device can complete another device job")
	}
	status, data := requestTest(t, h, "GET", "/api/state", s.Token, nil)
	if status != 200 || strings.Contains(string(data), "token_hash") || strings.Contains(string(data), a.Config.Token) {
		t.Fatal("state exposed device token")
	}
	r := httptest.NewRequest("POST", "http://localhost/api/login", strings.NewReader(`{"token":"`+s.Token+`"}`))
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross-origin login accepted")
	}
	r = httptest.NewRequest("POST", "http://localhost/api/login", strings.NewReader(`{"token":"`+s.Token+`"}`))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	cookies := w.Result().Cookies()
	if w.Code != 200 || len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("cookie flags missing")
	}
	r = httptest.NewRequest("GET", "http://localhost/api/state", nil)
	r.AddCookie(cookies[0])
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("cookie login failed")
	}
}
func TestNoReplayAfterServerRestart(t *testing.T) {
	s, e := NewServer(t.TempDir(), "http://localhost", strings.Repeat("c", 64))
	must(t, e)
	c := newTestClient(t, s, testEngine(t), "A")
	j := queueTest(t, s, Job{Kind: "export", DeviceID: c.Config.DeviceID, Provider: "claude", Project: "my-app"})
	var claimed Job
	must(t, c.api(context.Background(), "POST", "/api/agent/claim", map[string]any{}, &claimed))
	s2, e := NewServer(s.Dir, "http://localhost", s.Token)
	must(t, e)
	if s2.state.Jobs[j.ID].Status != "failed" {
		t.Fatal("interrupted job replay risk")
	}
}
func TestLocalArchivesRemainAfterUpload(t *testing.T) {
	s, e := NewServer(t.TempDir(), "http://localhost", strings.Repeat("d", 64))
	must(t, e)
	a := testEngine(t)
	fixture(t, a, "claude")
	c := newTestClient(t, s, a, "A")
	j := queueTest(t, s, Job{Kind: "export", DeviceID: c.Config.DeviceID, Provider: "claude", Project: "my-app"})
	executeTest(t, c)
	if _, e = os.Stat(filepath.Join(a.Config.DataDir, "exports", j.ID+".relay.zip")); e != nil {
		t.Fatal("local fallback archive missing")
	}
}
