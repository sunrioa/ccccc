package bundle

import (
	"archive/zip"
	"io"
	"os"
)

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

// WritePlanned streams validated staged content without holding a session in RAM.
func WritePlanned(file string, item ImportItem, w io.Writer) error {
	if item.StagedPath != "" {
		r, err := os.Open(item.StagedPath)
		if err != nil {
			return err
		}
		defer r.Close()
		_, err = io.Copy(w, r)
		return err
	}
	if item.content != nil {
		_, err := w.Write(item.content)
		return err
	}
	z, err := zip.OpenReader(file)
	if err != nil {
		return err
	}
	defer z.Close()
	entry, err := openByName(&z.Reader, item.BundlePath)
	if err != nil {
		return err
	}
	r, err := entry.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	_, err = io.Copy(w, r)
	return err
}
