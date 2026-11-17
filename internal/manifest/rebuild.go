package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/archive"
)

// HashFile returns the uppercase hex SHA-256 of a file (the reference reports' format).
func HashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", n, err
	}
	return strings.ToUpper(hex.EncodeToString(h.Sum(nil))), n, nil
}

// Rebuild refills a fresh store after the old one was found damaged (R18): it reads
// manifest.json (or its .bak) and keeps only entries whose files on the destination still hash
// to the recorded SHA-256 (a file in a ZIP part is checked inside the part, and the part's index
// restored). It reads the destination only — never the card. Entries without a
// file (excluded, failed) are restored as they were, except failures, which simply run again.
func Rebuild(m *Manifest, jsonPath, destRoot string) (restored int, err error) {
	b, err := os.ReadFile(jsonPath)
	if err != nil {
		if b, err = os.ReadFile(jsonPath + ".bak"); err != nil {
			return 0, err
		}
	}
	var doc struct {
		Manifest
		Entries []Entry
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return 0, err
	}
	m.Version, m.Volume, m.SourceKind, m.SourceRoot = doc.Version, doc.Volume, doc.SourceKind, doc.SourceRoot
	m.Profiles, m.Excluded = doc.Profiles, doc.Excluded
	m.Backup, m.Types, m.Apps, m.SkipEmpty, m.Compress = doc.Backup, doc.Types, doc.Apps, doc.SkipEmpty, doc.Compress
	if err := m.SaveMeta(); err != nil {
		return 0, err
	}
	zips := archive.NewEntryHasher(destRoot)
	defer zips.Close()
	for _, e := range doc.Entries {
		_, _, inZip := archive.ZipPart(e.Dest)
		switch {
		case e.Status == EXCLUDED || e.Status == DELETED_UNRECOVERABLE:
		case e.Dest == "" || e.SHA256 == "":
			continue
		case inZip: // checked inside its part, and the part's index restored with it
			sum, n, err := zips.Hash(e.Dest)
			if err != nil || n != e.Bytes || !strings.EqualFold(sum, e.SHA256) {
				continue
			}
			if err := archive.IndexEntry(m.s, e.Dest, n); err != nil {
				return restored, err
			}
		default:
			sum, n, err := HashFile(filepath.Join(destRoot, filepath.FromSlash(e.Dest)))
			if err != nil || n != e.Bytes || !strings.EqualFold(sum, e.SHA256) {
				continue
			}
		}
		if err := m.Put(e); err != nil {
			return restored, err
		}
		restored++
	}
	return restored, nil
}
