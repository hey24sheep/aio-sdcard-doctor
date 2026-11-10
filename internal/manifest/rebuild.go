package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
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
// to the recorded SHA-256. It reads the destination only — never the card. Entries without a
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
	m.Version, m.Volume, m.SourceKind = doc.Version, doc.Volume, doc.SourceKind
	m.Profiles, m.Excluded = doc.Profiles, doc.Excluded
	if err := m.SaveMeta(); err != nil {
		return 0, err
	}
	for _, e := range doc.Entries {
		switch {
		case e.Status == EXCLUDED || e.Status == DELETED_UNRECOVERABLE:
		case e.Dest == "" || e.SHA256 == "":
			continue
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
