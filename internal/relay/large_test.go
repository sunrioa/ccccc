package relay

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestLargeSessionRoundTrip(t *testing.T) {
	if os.Getenv("RELAY_LARGE_TEST") != "1" {
		t.Skip("opt-in: writes 140 MiB sessions to isolated temp directories")
	}
	for _, mode := range []string{"codex", "claude", "codex-zstd"} {
		t.Run(mode, func(t *testing.T) {
			provider := strings.Split(mode, "-")[0]
			a, b := testEngine(t), testEngine(t)
			source := fixture(t, a, provider)
			f, err := os.OpenFile(source, os.O_APPEND|os.O_WRONLY, 0600)
			must(t, err)
			bw := bufio.NewWriterSize(f, 64<<10)
			line := []byte(`{"type":"event_msg","payload":{"type":"agent_message","message":"` + strings.Repeat("preserved-content-", 4096) + `"}}` + "\n")
			for n := 0; n < 140<<20; n += len(line) {
				_, err = bw.Write(line)
				must(t, err)
			}
			must(t, bw.Flush())
			must(t, f.Close())
			if mode == "codex-zstd" {
				in, err := os.Open(source)
				must(t, err)
				out, err := os.Create(source + ".zst")
				must(t, err)
				enc, err := zstd.NewWriter(out, zstd.WithEncoderConcurrency(1))
				must(t, err)
				_, err = io.Copy(enc, in)
				must(t, err)
				must(t, enc.Close())
				must(t, out.Close())
				must(t, in.Close())
				must(t, os.Remove(source))
				source += ".zst"
			}
			originalHash, err := hashFile(source)
			must(t, err)
			archive := filepath.Join(t.TempDir(), "large.relay.zip")
			_, err = a.Export(provider, "my-app", archive)
			must(t, err)
			plan, err := b.Preview(archive, "my-app", "large-preview")
			must(t, err)
			if !plan.CanApply || plan.ExpandedBytes < 140<<20 {
				t.Fatalf("large preview not supported: bytes=%d", plan.ExpandedBytes)
			}
			_, err = b.Apply(plan.ID, "large-restore")
			must(t, err)
			var target string
			for _, change := range plan.Changes {
				if strings.HasSuffix(change.Path, filepath.Base(source)) {
					target = change.Path
				}
			}
			if target == "" {
				t.Fatal("target missing")
			}
			if mode == "codex-zstd" {
				in, err := os.Open(target)
				must(t, err)
				dec, err := zstd.NewReader(in)
				must(t, err)
				plain := target + ".plain"
				out, err := os.Create(plain)
				must(t, err)
				_, err = io.Copy(out, dec)
				must(t, err)
				dec.Close()
				must(t, in.Close())
				must(t, out.Close())
				appendRow(t, plain, provider, b.Config.Projects[0].Path)
				in, err = os.Open(plain)
				must(t, err)
				out, err = os.Create(target)
				must(t, err)
				enc, err := zstd.NewWriter(out)
				must(t, err)
				_, err = io.Copy(enc, in)
				must(t, err)
				must(t, enc.Close())
				must(t, out.Close())
				must(t, in.Close())
				must(t, os.Remove(plain))
			} else {
				appendRow(t, target, provider, b.Config.Projects[0].Path)
			}
			reverse := filepath.Join(t.TempDir(), "reverse.relay.zip")
			_, err = b.Export(provider, "my-app", reverse)
			must(t, err)
			back, err := a.Preview(reverse, "my-app", "reverse-preview")
			must(t, err)
			if !back.CanApply {
				t.Fatal("reverse append blocked")
			}
			_, err = a.Apply(back.ID, "reverse-restore")
			must(t, err)
			_, err = a.Rollback("reverse-restore")
			must(t, err)
			restoredHash, err := hashFile(source)
			must(t, err)
			if restoredHash != originalHash {
				t.Fatal("rollback did not restore exact original bytes")
			}
			if _, err = b.Rollback("large-restore"); err == nil {
				t.Fatal("rollback overwrote new continuation")
			}
			t.Logf("140 MiB %s export/import/append/reverse/rollback passed", mode)
		})
	}
}
