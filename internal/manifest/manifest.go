// Package manifest is the record of every file an action handled. It lives in the run's
// Reports/state.db (the source of truth for resume) and is exported as manifest.json.
package manifest

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/destfs"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/profile"
)

// Status of one file.
type Status string

// Statuses written to the manifest and reports.
const (
	OK                    Status = "OK"
	PARTIAL               Status = "PARTIAL"
	FAILED                Status = "FAILED"
	STALLED               Status = "STALLED"
	LOCKED                Status = "LOCKED"
	EXCLUDED              Status = "EXCLUDED"
	ALREADY_COPIED        Status = "ALREADY-COPIED"
	DUPLICATE             Status = "DUPLICATE"
	DELETED_RECOVERED     Status = "DELETED-RECOVERED"
	DELETED_UNRECOVERABLE Status = "DELETED-UNRECOVERABLE"
	CARVED                Status = "CARVED"
	CARVED_DAMAGED        Status = "CARVED-DAMAGED"
	MISSING               Status = "MISSING"
	WRONG_SIZE            Status = "WRONG-SIZE"
	CORRUPT               Status = "CORRUPT"
	UNREADABLE            Status = "UNREADABLE"
)

// Version of the manifest format.
const Version = 1

const (
	bucketEntries = "entries"
	bucketMeta    = "meta"
	keyMeta       = "manifest"
)

// Entry is one file's outcome.
type Entry struct {
	Source, Dest    string
	Category        profile.Category
	Bytes, BadBytes int64
	Header, SHA256  string
	Status          Status
	Error           string
	MTime, SrcMTime time.Time
}

// Manifest is the run's metadata plus access to its entries.
type Manifest struct {
	Version    int
	Volume     drives.Identity
	SourceKind string
	Profiles   []string
	Excluded   []string
	// The user's choice for this destination, offered again on the next run.
	Backup    string
	Types     []profile.Category
	Apps      []string
	SkipEmpty bool
	Compress  bool
	s         *job.Store
}

// Open loads the manifest stored in s (empty if new).
func Open(s *job.Store) (*Manifest, error) {
	m, ok, err := job.Get[Manifest](s, bucketMeta, keyMeta)
	if err != nil {
		return nil, err
	}
	if !ok {
		m = Manifest{Version: Version}
	}
	m.s = s
	return &m, nil
}

// SaveMeta stores the metadata fields.
func (m *Manifest) SaveMeta() error { return job.Put(m.s, bucketMeta, keyMeta, *m) }

// Put records e under its Source path (one fsync'd transaction).
func (m *Manifest) Put(e Entry) error { return job.Put(m.s, bucketEntries, e.Source, e) }

// Get returns the entry for a source path.
func (m *Manifest) Get(src string) (Entry, bool, error) {
	return job.Get[Entry](m.s, bucketEntries, src)
}

// Each visits every entry in source-path order.
func (m *Manifest) Each(fn func(Entry) error) error {
	return job.ForEach(m.s, bucketEntries, func(_ string, e Entry) error { return fn(e) })
}

// Count is the number of entries.
func (m *Manifest) Count() (int, error) {
	n := 0
	err := m.Each(func(Entry) error { n++; return nil })
	return n, err
}

// ExportJSON writes manifest.json atomically (the spec's report file).
func (m *Manifest) ExportJSON(out destfs.FS, path string) error {
	var buf bytes.Buffer
	head, err := json.Marshal(struct {
		Version    int
		Volume     drives.Identity
		SourceKind string
		Profiles   []string
		Excluded   []string
	}{m.Version, m.Volume, m.SourceKind, m.Profiles, m.Excluded})
	if err != nil {
		return err
	}
	buf.Write(head[:len(head)-1])
	buf.WriteString(`,"Entries":[`)
	first := true
	err = m.Each(func(e Entry) error {
		if !first {
			buf.WriteByte(',')
		}
		first = false
		b, err := json.Marshal(e)
		buf.Write(b)
		return err
	})
	if err != nil {
		return err
	}
	buf.WriteString("]}\n")
	return destfs.WriteFileAtomic(out, path, buf.Bytes())
}

func rank(s Status) int {
	switch s {
	case OK, ALREADY_COPIED:
		return 3
	case PARTIAL:
		return 2
	case LOCKED, STALLED, FAILED:
		return 1
	}
	return 0
}

// Better reports whether new should replace old: OK > PARTIAL (fewer bad bytes first) >
// LOCKED/STALLED/FAILED > nothing. A good copy is never replaced by a worse one.
func Better(old, new Entry) bool {
	ro, rn := rank(old.Status), rank(new.Status)
	if rn != ro {
		return rn > ro
	}
	return new.Status == PARTIAL && new.BadBytes < old.BadBytes
}
