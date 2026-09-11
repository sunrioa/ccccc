package bundle

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ahmojo/codex-claude-transfer/internal/agent"
	"github.com/ahmojo/codex-claude-transfer/internal/codexhome"
	"github.com/ahmojo/codex-claude-transfer/internal/safety"
	"github.com/klauspost/compress/zstd"
)

const StreamSessionBytes int64 = 2 << 30
const StreamTotalBytes int64 = 8 << 30
const StreamRecordBytes = 128 << 20

// A single physical JSONL record is bounded, but an entire session is never
// loaded into memory. Terminators and untouched records are kept verbatim.
func readRecord(r *bufio.Reader) ([]byte, error) { return readRecordLimit(r, StreamRecordBytes) }
func readRecordLimit(r *bufio.Reader, max int) ([]byte, error) {
	var out []byte
	for {
		part, err := r.ReadSlice('\n')
		if len(out)+len(part) > max {
			return nil, fmt.Errorf("单条 JSONL 记录超过 128 MiB，无法安全处理；会话文件总大小上限为 2 GiB")
		}
		out = append(out, part...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF && len(out) > 0 {
			return out, nil
		}
		return out, err
	}
}

type limitWriter struct {
	w      io.Writer
	n, max int64
}

func (w *limitWriter) Write(p []byte) (int, error) {
	if w.n+int64(len(p)) > w.max {
		return 0, fmt.Errorf("会话展开或合并后超过 2 GiB 上限")
	}
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}

func streamDecoder(r io.Reader) (*zstd.Decoder, error) {
	return zstd.NewReader(r, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(128<<20), zstd.WithDecoderMaxWindow(64<<20))
}

// mapStream writes one disk-backed plaintext session. Only the canonical cwd
// field is rewritten; JSON parsing is per-record rather than per-session.
func mapStream(r io.Reader, w io.Writer, kind agent.Kind, mapping *CWDMapping) (int64, bool, error) {
	input := &io.LimitedReader{R: r, N: StreamSessionBytes + 1}
	br := bufio.NewReaderSize(input, 64<<10)
	bw := bufio.NewWriterSize(w, 64<<10)
	lw := &limitWriter{w: bw, max: StreamSessionBytes}
	changed, metaSeen := false, false
	for {
		line, err := readRecord(br)
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, false, err
		}
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 && !json.Valid(trimmed) {
			return 0, false, fmt.Errorf("会话含有不完整或无效 JSON 记录，请关闭工具后重试")
		}
		mapped := line
		if mapping != nil {
			var did bool
			if kind == agent.Claude {
				mapped, did, err = rewriteClaudeCWD(line, mapping.Old, mapping.New)
				if err == nil && did {
					err = validateClaudeMappedCWD(line, mapped, mapping.Old, mapping.New)
				}
			} else if !metaSeen {
				var wrapper struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(line, &wrapper) == nil && wrapper.Type == "session_meta" {
					metaSeen = true
					mapped, did, err = rewriteSessionMetaCWD(line, mapping.Old, mapping.New)
					if err == nil && did {
						err = validateMappedJSONL(line, mapped, mapping.New)
					}
				}
			}
			if err != nil {
				return 0, false, err
			}
			changed = changed || did
		}
		if _, err = lw.Write(mapped); err != nil {
			return 0, false, err
		}
	}
	if input.N <= 0 {
		return 0, false, fmt.Errorf("会话展开后超过 2 GiB 上限")
	}
	if err := bw.Flush(); err != nil {
		return 0, false, err
	}
	consumed := StreamSessionBytes + 1 - input.N
	if consumed > lw.n {
		return consumed, changed, nil
	}
	return lw.n, changed, nil
}

// mergeStreams retains LOCAL bytes for every shared semantic record and appends
// only the incoming suffix. It rejects any actual divergence, including exact
// numeric-literal differences. Only one record from each side is resident.
func mergeStreams(incoming, local io.Reader, out io.Writer) (Action, int, error) {
	a, b := bufio.NewReaderSize(incoming, 64<<10), bufio.NewReaderSize(local, 64<<10)
	bw := bufio.NewWriterSize(out, 64<<10)
	w := &limitWriter{w: bw, max: StreamSessionBytes}
	lastNewline, haveLocal, added := true, false, 0
	for {
		x, xe := readRecord(a)
		y, ye := readRecord(b)
		if xe != nil && xe != io.EOF {
			return "", 0, xe
		}
		if ye != nil && ye != io.EOF {
			return "", 0, ye
		}
		if xe == io.EOF {
			if ye == io.EOF {
				return ActionSkipIdentical, 0, nil
			}
			return ActionSkipAhead, 0, nil
		}
		if ye == io.EOF {
			if haveLocal && !lastNewline {
				if _, err := w.Write([]byte{'\n'}); err != nil {
					return "", 0, err
				}
			}
			for {
				if _, err := w.Write(x); err != nil {
					return "", 0, err
				}
				added++
				x, xe = readRecord(a)
				if xe == io.EOF {
					break
				}
				if xe != nil {
					return "", 0, xe
				}
			}
			if err := bw.Flush(); err != nil {
				return "", 0, err
			}
			return ActionUpdate, added, nil
		}
		xText := bytes.TrimSuffix(x, []byte{'\n'})
		yText := bytes.TrimSuffix(y, []byte{'\n'})
		if !bytes.Equal(xText, yText) && !bytes.Equal(canonicalJSONLLine(xText), canonicalJSONLLine(yText)) {
			return ActionConflict, 0, nil
		}
		if _, err := w.Write(y); err != nil {
			return "", 0, err
		}
		haveLocal, lastNewline = true, len(y) > 0 && y[len(y)-1] == '\n'
	}
}

func newStage(dir string) (*os.File, error) { return os.CreateTemp(dir, "payload-*") }

// PlanStreaming is the Relay import planner. It validates the entire native
// bundle first, then prepares payloads on disk; it never writes the agent home.
// The caller owns stagingDir and removes it when the preview is discarded.
func PlanStreaming(home codexhome.Home, opts ImportOptions, stagingDir string, progress func(int, int)) (ImportResult, int64, error) {
	result := ImportResult{DryRun: true, Warnings: []string{}}
	if !opts.DryRun {
		return result, 0, fmt.Errorf("stream planner requires DryRun")
	}
	z, err := zip.OpenReader(opts.BundlePath)
	if err != nil {
		return result, 0, err
	}
	defer z.Close()
	manifest, sums, err := readMeta(&z.Reader)
	if err != nil {
		return result, 0, err
	}
	if err = validateManifest(manifest); err != nil {
		return result, 0, err
	}
	kind := agent.Normalize(agent.Kind(manifest.Tool))
	if kind != agent.Codex && kind != agent.Claude {
		return result, 0, fmt.Errorf("不支持的工具类型")
	}
	if err = verifyBundleWithLimits(&z.Reader, sums, StreamSessionBytes, StreamTotalBytes); err != nil {
		return result, 0, err
	}
	if err = verifyManifestBinding(&z.Reader, manifest, sums, kind); err != nil {
		return result, 0, err
	}
	result.Manifest = manifest
	if err = os.MkdirAll(stagingDir, 0700); err != nil {
		return result, 0, err
	}
	declared := map[string]bool{ManifestName: true, ChecksumsName: true}
	var expanded int64
	for index, session := range manifest.Sessions {
		rel := session.BundlePath
		declared[rel] = true
		if !isImportableEntryForImport(kind, rel, opts.IncludeArchived) {
			return result, expanded, fmt.Errorf("不支持的会话路径：%s", rel)
		}
		entry, err := openByName(&z.Reader, rel)
		if err != nil {
			return result, expanded, err
		}
		item := ImportItem{BundlePath: rel, OriginalCWD: session.OriginalCWD}
		mapping := matchMapping(item.OriginalCWD, opts.MapCWD)
		destRel := rel
		if kind == agent.Claude && mapping != nil {
			destRel = claudeDestRelForCWD(rel, mapping.New)
		}
		item.DestPath, err = safety.DestPath(home.Root, destRel)
		if err != nil {
			return result, expanded, err
		}
		if err = streamSafeDestination(home.Root, item.DestPath); err != nil {
			return result, expanded, err
		}
		// Reject symlinks/special files before opening a local session for merging.
		if st, x := os.Lstat(item.DestPath); x == nil && !st.Mode().IsRegular() {
			return result, expanded, fmt.Errorf("目标不是普通会话文件：%s", item.DestPath)
		} else if x != nil && !os.IsNotExist(x) {
			return result, expanded, x
		}
		plain, err := newStage(stagingDir)
		if err != nil {
			return result, expanded, err
		}
		plainPath := plain.Name()
		raw, err := entry.Open()
		if err != nil {
			plain.Close()
			return result, expanded, err
		}
		var reader io.Reader = raw
		compressed := strings.HasSuffix(rel, compressedSessionSuffix)
		var decoder *zstd.Decoder
		if compressed {
			decoder, err = streamDecoder(raw)
			if err != nil {
				raw.Close()
				plain.Close()
				return result, expanded, err
			}
			reader = decoder
		}
		n, changed, err := mapStream(reader, plain, kind, mapping)
		if decoder != nil {
			decoder.Close()
		}
		raw.Close()
		ce := plain.Close()
		if err == nil {
			err = ce
		}
		if err != nil {
			return result, expanded, fmt.Errorf("处理 %s：%w", rel, err)
		}
		expanded += n
		if expanded > StreamTotalBytes {
			return result, expanded, fmt.Errorf("会话总展开量超过 8 GiB")
		}
		item.Mapped = changed
		item.Action = ActionImport
		if st, statErr := os.Stat(item.DestPath); statErr == nil {
			if st.Size() > StreamSessionBytes {
				return result, expanded, fmt.Errorf("本机已有会话超过 2 GiB：%s", item.DestPath)
			}
			incoming, err := os.Open(plainPath)
			if err != nil {
				return result, expanded, err
			}
			local, err := os.Open(item.DestPath)
			if err != nil {
				incoming.Close()
				return result, expanded, err
			}
			var localReader io.Reader = local
			var localDecoder *zstd.Decoder
			if compressed {
				localDecoder, err = streamDecoder(local)
				if err != nil {
					incoming.Close()
					local.Close()
					return result, expanded, err
				}
				localReader = localDecoder
			}
			localLimited := &io.LimitedReader{R: localReader, N: StreamSessionBytes + 1}
			merged, err := newStage(stagingDir)
			if err != nil {
				incoming.Close()
				local.Close()
				if localDecoder != nil {
					localDecoder.Close()
				}
				return result, expanded, err
			}
			item.Action, item.LinesAdded, err = mergeStreams(incoming, localLimited, merged)
			if localLimited.N <= 0 {
				err = fmt.Errorf("本机会话展开后超过 2 GiB")
			}
			incoming.Close()
			if localDecoder != nil {
				localDecoder.Close()
			}
			local.Close()
			ce = merged.Close()
			if err == nil {
				err = ce
			}
			if err != nil {
				return result, expanded, err
			}
			if item.Action == ActionUpdate {
				os.Remove(plainPath)
				plainPath = merged.Name()
			} else {
				os.Remove(merged.Name())
			}
		} else if !os.IsNotExist(statErr) {
			return result, expanded, statErr
		}
		if item.Action == ActionImport || item.Action == ActionUpdate {
			if compressed {
				encoded, err := newStage(stagingDir)
				if err != nil {
					return result, expanded, err
				}
				in, err := os.Open(plainPath)
				if err != nil {
					encoded.Close()
					return result, expanded, err
				}
				encoder, err := zstd.NewWriter(encoded, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(zstd.SpeedDefault))
				if err != nil {
					in.Close()
					encoded.Close()
					return result, expanded, err
				}
				_, err = io.Copy(encoder, in)
				in.Close()
				ce = encoder.Close()
				if err == nil {
					err = ce
				}
				ce = encoded.Close()
				if err == nil {
					err = ce
				}
				if err != nil {
					return result, expanded, err
				}
				os.Remove(plainPath)
				plainPath = encoded.Name()
			}
			item.StagedPath = plainPath
		} else {
			os.Remove(plainPath)
		}
		switch item.Action {
		case ActionImport:
			result.Imported++
		case ActionUpdate:
			result.Updated++
			result.LinesAdded += item.LinesAdded
		case ActionSkipAhead:
			result.AlreadyAhead++
		case ActionSkipIdentical:
			result.SkippedIdentical++
		case ActionConflict:
			result.Conflicts++
		}
		if changed {
			result.Mapped++
		}
		result.Items = append(result.Items, item)
		if progress != nil {
			progress(index+1, len(manifest.Sessions))
		}
	}
	memories := map[string]ManifestMemory{}
	for _, memory := range manifest.Memory {
		memories[memory.BundlePath] = memory
		declared[memory.BundlePath] = true
		expanded += memory.SizeBytes
	}
	if expanded > StreamTotalBytes {
		return result, expanded, fmt.Errorf("展开总量超过 8 GiB")
	}
	for _, memory := range manifest.Memory {
		entry, err := openByName(&z.Reader, memory.BundlePath)
		if err != nil {
			return result, expanded, err
		}
		if sums[memory.BundlePath] != memory.SHA256 || entry.UncompressedSize64 != uint64(memory.SizeBytes) {
			return result, expanded, fmt.Errorf("记忆文件元数据与内容不匹配")
		}
		cwd := memory.ProjectCWD
		if m := matchMapping(cwd, opts.MapCWD); m != nil {
			cwd = m.New
		}
		if err := streamSafeDestination(home.Root, filepath.Join(home.Root, filepath.FromSlash(memoryDestRelForCWD(memory.Rel, cwd)))); err != nil {
			return result, expanded, err
		}
		if err := importMemoryEntry(&z.Reader, home, memory.BundlePath, memories, opts.MapCWD, opts, &result); err != nil {
			return result, expanded, err
		}
	}
	for _, entry := range z.File {
		if !declared[entry.Name] {
			return result, expanded, fmt.Errorf("原生包包含未登记条目：%s", entry.Name)
		}
	}
	return result, expanded, nil
}

func streamSafeDestination(root, dest string) error {
	rel, err := filepath.Rel(root, dest)
	if err != nil {
		return err
	}
	if _, err = safety.CleanRelPath(filepath.ToSlash(rel)); err != nil {
		return err
	}
	current := root
	for i, part := range strings.Split(filepath.ToSlash(rel), "/") {
		current = filepath.Join(current, part)
		st, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("目标路径包含符号链接：%s", current)
		}
		if i < len(strings.Split(filepath.ToSlash(rel), "/"))-1 && !st.IsDir() {
			return fmt.Errorf("父路径不是目录")
		}
	}
	return nil
}
