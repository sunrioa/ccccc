//go:build !windows

package relay

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Uses only the health endpoint, never device credentials or session content.
func TestRealSSHTunnelReconnect(t *testing.T) {
	if os.Getenv("RELAY_SSH_TEST_HOST") == "" {
		t.Skip("opt-in live SSH")
	}
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	must(t, e)
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	c := &Client{Config: ClientConfig{Server: "http://127.0.0.1:" + strconv.Itoa(port), SSH: &SSHConfig{Host: os.Getenv("RELAY_SSH_TEST_HOST"), User: "root", IdentityFile: os.Getenv("RELAY_SSH_TEST_KEY"), KnownHostsFile: os.Getenv("RELAY_SSH_TEST_KNOWN")}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop, e := c.startTunnel(ctx)
	must(t, e)
	defer stop()
	wait := func() {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if tunnelHealthy(ctx, c.Config.Server) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("SSH did not become healthy")
	}
	wait()
	children, e := exec.Command("pgrep", "-P", strconv.Itoa(os.Getpid())).Output()
	must(t, e)
	fields := strings.Fields(string(children))
	if len(fields) != 1 {
		t.Fatalf("expected exactly one owned SSH child, got %d", len(fields))
	}
	pid, e := strconv.Atoi(fields[0])
	must(t, e)
	proc, e := os.FindProcess(pid)
	must(t, e)
	must(t, proc.Kill())
	time.Sleep(200 * time.Millisecond)
	if tunnelHealthy(ctx, c.Config.Server) {
		t.Fatal("test failed to disconnect")
	}
	wait()
	stop()
	if tunnelHealthy(ctx, c.Config.Server) {
		t.Fatal("owned SSH process left behind")
	}
}
