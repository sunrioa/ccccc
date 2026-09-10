package relay

import (
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestClientPollingOverRealHTTP(t *testing.T) {
	if os.Getenv("RELAY_NETWORK_TEST") != "1" {
		t.Skip("opt-in localhost networking test")
	}
	s, e := NewServer(t.TempDir(), "", strings.Repeat("z", 64))
	must(t, e)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	engine := testEngine(t)
	fixture(t, engine, "claude")
	c := newTestClient(t, s, engine, "loopback client")
	c.Config.Server = ts.URL
	c.HTTP.Transport = nil
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case e := <-done:
			must(t, e)
		case <-time.After(10 * time.Second):
			t.Error("client did not stop")
		}
	}()
	job := queueTest(t, s, Job{Kind: "export", DeviceID: c.Config.DeviceID, Provider: "claude", Project: "my-app"})
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		status := s.state.Jobs[job.ID].Status
		message := s.state.Jobs[job.ID].Error
		s.mu.Unlock()
		if status == "done" {
			return
		}
		if status == "failed" {
			t.Fatal(message)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("polling/upload/completion did not finish")
}
