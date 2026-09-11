package bundle

import (
	"archive/zip"
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ahmojo/codex-claude-transfer/internal/agent"
)

func TestStreamCanonicalMerge(t *testing.T) {
	cases := []struct {
		name, incoming, local string
		action                Action
		want                  string
	}{
		{"format-preserving append", "{\"a\":1,\"b\":2}\n{\"n\":3}\n", "{ \"b\": 2, \"a\": 1 }\r\n", ActionUpdate, "{ \"b\": 2, \"a\": 1 }\r\n{\"n\":3}\n"},
		{"missing final newline", "{\"a\":1}\n{\"n\":3}\n", "{\"a\":1}", ActionUpdate, "{\"a\":1}\n{\"n\":3}\n"},
		{"exact numbers", "{\"n\":9007199254740993}\n", "{\"n\":9007199254740992}\n", ActionConflict, ""},
		{"equal", "{\"a\":1}\n", "{\"a\":1}\n", ActionSkipIdentical, ""},
		{"local ahead", "{\"a\":1}\n", "{\"a\":1}\n{\"a\":2}\n", ActionSkipAhead, ""},
		{"fork", "{\"a\":1}\n{\"a\":3}\n", "{\"a\":1}\n{\"a\":2}\n", ActionConflict, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			action, _, err := mergeStreams(strings.NewReader(c.incoming), strings.NewReader(c.local), &out)
			if err != nil || action != c.action {
				t.Fatalf("%s %v", action, err)
			}
			if action == ActionUpdate && out.String() != c.want {
				t.Fatalf("changed local bytes: %q", out.String())
			}
		})
	}
}

func TestStreamMappingPreservesOpaqueContent(t *testing.T) {
	input := "{\"type\":\"session_meta\",\"payload\":{\"cwd\":\"/old\",\"id\":\"s\",\"n\":9007199254740993}}\r\n" +
		"{\"type\":\"response_item\",\"payload\":{\"content\":\"keep /old verbatim\",\"unknown\":[1,2,3]}}\r\n"
	var out bytes.Buffer
	_, changed, err := mapStream(strings.NewReader(input), &out, agent.Codex, &CWDMapping{Old: "/old", New: "/new"})
	if err != nil || !changed {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "9007199254740993") || !strings.HasSuffix(out.String(), strings.SplitAfterN(input, "\n", 2)[1]) {
		t.Fatal("altered non-cwd content")
	}
	if _, _, err = mapStream(strings.NewReader(input+"{broken"), io.Discard, agent.Codex, nil); err == nil {
		t.Fatal("accepted incomplete record")
	}
}

func TestStreamResourceGuards(t *testing.T) {
	if _, err := readRecordLimit(bufio.NewReader(strings.NewReader(strings.Repeat("x", 65))), 64); err == nil {
		t.Fatal("record bound ignored")
	}
	w := &limitWriter{w: io.Discard, max: 10}
	if _, err := w.Write(make([]byte, 11)); err == nil {
		t.Fatal("output bound ignored")
	}
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	f, _ := zw.Create("sessions/test")
	f.Write([]byte("hello"))
	zw.Close()
	zr, err := zip.NewReader(bytes.NewReader(b.Bytes()), int64(b.Len()))
	if err != nil {
		t.Fatal(err)
	}
	zr.File[0].UncompressedSize64 = uint64(StreamSessionBytes + 1)
	if err = verifyBundleWithLimits(zr, Checksums{"sessions/test": "unused"}, StreamSessionBytes, StreamTotalBytes); err == nil {
		t.Fatal("declared size guard ignored")
	}
	root := t.TempDir()
	os.Symlink(t.TempDir(), filepath.Join(root, "linked"))
	if err = streamSafeDestination(root, filepath.Join(root, "linked", "session.jsonl")); err == nil {
		t.Fatal("followed ancestor symlink")
	}
}
