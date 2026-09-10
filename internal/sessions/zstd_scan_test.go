package sessions

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ahmojo/codex-claude-transfer/internal/zstdcli"
)

// writeCompressedRollout writes a real zstd-compressed rollout file named
// <name> (which must end in .jsonl.zst) under root/datePath, by compressing the
// given plain JSONL body with the zstd CLI.
func writeCompressedRollout(t *testing.T, root, datePath, name, body string) string {
	t.Helper()
	plain := filepath.Join(t.TempDir(), "plain.jsonl")
	if err := os.WriteFile(plain, []byte(body), 0o644); err != nil {
		t.Fatalf("write plain: %v", err)
	}
	dir := filepath.Join(root, datePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dst := filepath.Join(dir, name)
	out, err := zstdcli.Compress([]byte(body))
	if err != nil { t.Fatal(err) }
	if err = os.WriteFile(dst, out, 0600); err != nil { t.Fatal(err) }
	return dst
}

func TestScanDecompressesCompressedWhenRequested(t *testing.T) {
	if !zstdcli.Available() {
		t.Skip("zstd not installed; skipping compressed-metadata recovery test")
	}
	home := fakeHome(t)
	id := "dddd1111-2222-3333-4444-555566667777"
	cwd := "/Users/example/dev/zstproj"
	writeCompressedRollout(t, home.SessionsDir, "2026/06/13",
		"rollout-2026-06-13T18-22-01-"+id+".jsonl.zst",
		validRolloutLines(id, cwd))

	// Default scan: still detected as compressed, metadata not recovered.
	def, err := Scan(home, ScanOptions{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if def.Compressed != 1 || def.Valid != 0 {
		t.Fatalf("default scan: compressed=%d valid=%d", def.Compressed, def.Valid)
	}
	if def.Sessions[0].Parsed {
		t.Errorf("default scan should not parse compressed metadata")
	}

	// Opt-in scan: metadata recovered, but the file is still flagged compressed.
	got, err := Scan(home, ScanOptions{DecompressCompressed: true})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got.Valid != 1 {
		t.Fatalf("decompress scan: valid=%d, want 1", got.Valid)
	}
	s := got.Sessions[0]
	if !s.Compressed {
		t.Errorf("recovered session should still be marked Compressed")
	}
	if !s.Parsed {
		t.Errorf("expected Parsed=true after decompression")
	}
	if s.ThreadID != id {
		t.Errorf("thread id = %q, want %q", s.ThreadID, id)
	}
	if s.CWD != cwd {
		t.Errorf("cwd = %q, want %q", s.CWD, cwd)
	}
	if s.Preview == "" {
		t.Errorf("expected a recovered preview")
	}
}
