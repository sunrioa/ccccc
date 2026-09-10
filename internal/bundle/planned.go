package bundle

import "archive/zip"

// PlannedBytes exposes the already validated/mapped bytes for the relay's
// transaction layer. The caller must use an item from a successful dry run.
func PlannedBytes(file string, item ImportItem) ([]byte, error) {
	if item.content != nil {
		return append([]byte(nil), item.content...), nil
	}
	z, e := zip.OpenReader(file)
	if e != nil {
		return nil, e
	}
	defer z.Close()
	return readEntryBytes(&z.Reader, item.BundlePath)
}
