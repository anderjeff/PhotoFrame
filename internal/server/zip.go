package server

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

// maxZipEntries caps how many files one archive may contain.
const maxZipEntries = 20000

// isZip reports whether an upload is a zip archive (e.g. a Google Photos
// download of several photos).
func isZip(head []byte, name string) bool {
	return bytes.HasPrefix(head, []byte("PK\x03\x04")) ||
		(bytes.HasPrefix(head, []byte("PK\x05\x06")) && strings.HasSuffix(strings.ToLower(name), ".zip")) // empty archive
}

// saveZip spools the archive to disk (zip needs random access to its central
// directory at the end), then adds every photo inside it. Other files, such as
// videos or Google Takeout's .json sidecars, are skipped. HEIC photos are
// reported as errors so the user knows they weren't added.
func (a *App) saveZip(name string, r io.Reader) []uploadResult {
	fail := func(msg string) []uploadResult {
		a.Log.Warn("zip upload failed", "name", name, "reason", msg)
		return []uploadResult{{Name: name, Zip: name, Error: msg}}
	}
	start := time.Now()
	tmp, err := os.CreateTemp(a.Store.TmpDir(), "zip-*")
	if err != nil {
		return fail(err.Error())
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	limit := a.Cfg.MaxZipMB << 20
	size, err := io.Copy(tmp, io.LimitReader(r, limit+1))
	if err != nil {
		return fail("upload failed: " + err.Error())
	}
	if size > limit {
		return fail(fmt.Sprintf("zip is larger than %d MB (MAX_ZIP_MB)", a.Cfg.MaxZipMB))
	}

	zr, err := zip.NewReader(tmp, size)
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) { // entry paths are never used as file paths here
		return fail("not a valid zip file: " + err.Error())
	}
	if len(zr.File) > maxZipEntries {
		return fail(fmt.Sprintf("zip has more than %d files", maxZipEntries))
	}

	var results []uploadResult
	skipped := 0
	for _, f := range zr.File {
		base := path.Base(strings.ReplaceAll(f.Name, `\`, "/"))
		if f.FileInfo().IsDir() || strings.HasPrefix(base, ".") || strings.HasPrefix(f.Name, "__MACOSX/") {
			continue
		}
		res := a.saveZipEntry(f, base)
		if res == nil {
			skipped++
			continue
		}
		res.Zip = name
		results = append(results, *res)
	}
	if len(results) == 0 {
		return fail("no photos found in the zip")
	}
	added, dups, failed := 0, 0, 0
	for _, r := range results {
		switch {
		case r.Error != "":
			failed++
		case r.Duplicate:
			dups++
		default:
			added++
		}
	}
	a.Log.Info("zip unpacked", "name", name, "bytes", size, "added", added, "duplicates", dups,
		"failed", failed, "skipped", skipped, "dur", time.Since(start).Round(time.Millisecond))
	return results
}

// saveZipEntry adds one archive entry. It returns nil for files that aren't
// photos and should be skipped silently.
func (a *App) saveZipEntry(f *zip.File, name string) *uploadResult {
	rc, err := f.Open()
	if err != nil {
		return &uploadResult{Name: name, Error: "cannot read from zip: " + err.Error()}
	}
	defer rc.Close()
	head, err := readHead(rc)
	if err != nil {
		if errors.Is(err, errEmptyFile) {
			return nil
		}
		return &uploadResult{Name: name, Error: err.Error()}
	}
	if _, ok := allowedTypes[http.DetectContentType(head)]; !ok {
		if lower := strings.ToLower(name); strings.HasSuffix(lower, ".heic") || strings.HasSuffix(lower, ".heif") {
			return &uploadResult{Name: name, Error: unsupportedReason(name)}
		}
		return nil
	}
	res := a.savePhoto(name, head, rc)
	return &res
}
