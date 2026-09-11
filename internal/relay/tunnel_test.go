package relay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSSHArgsUsePinnedHostAndLoopback(t *testing.T) {
	key := filepath.Join(t.TempDir(), "key with spaces")
	must(t, os.WriteFile(key, []byte("fixture"), 0600))
	c := ClientConfig{Server: "http://127.0.0.1:28787", SSH: &SSHConfig{Host: "example.com", User: "relay", IdentityFile: key}}
	args, e := c.sshArgs()
	must(t, e)
	joined := strings.Join(args, "|")
	for _, want := range []string{"StrictHostKeyChecking=yes", "BatchMode=yes", "127.0.0.1:28787:127.0.0.1:8787", key} {
		if !strings.Contains(joined, want) {
			t.Fatal(want)
		}
	}
	for _, host := range []string{"-oProxyCommand=x", "bad host", "x;touch /tmp/x"} {
		c.SSH.Host = host
		if _, e = c.sshArgs(); e == nil {
			t.Fatal("unsafe host accepted")
		}
	}
	c.SSH.Host = "example.com"
	c.Server = "http://0.0.0.0:28787"
	if _, e = c.sshArgs(); e == nil {
		t.Fatal("public listener accepted")
	}
}
