// Package report writes the CSV and plain-text summaries every action leaves next to its
// output. Reports never leave the machine.
package report

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/destfs"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/manifest"
)

// Stamp formats t as yyyyMMdd_HHmmss_fff (the reference script's report stamp).
func Stamp(t time.Time) string {
	return t.Format("20060102_150405") + fmt.Sprintf("_%03d", t.Nanosecond()/1e6)
}

// UniquePath returns p, or p with _2, _3 … before the extension if p exists (bounded).
func UniquePath(p string) string {
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 2; i < 10000; i++ {
		c := fmt.Sprintf("%s_%d%s", base, i, ext)
		if _, err := os.Stat(c); os.IsNotExist(err) {
			return c
		}
	}
	return fmt.Sprintf("%s_%d%s", base, time.Now().UnixNano(), ext)
}

// Header is the copy/recover CSV header.
var Header = []string{"Status", "Category", "Source", "Dest", "Bytes", "BadBytes", "Header", "SHA256", "Error", "MTime"}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// Cell makes card-supplied text safe in a spreadsheet: a name starting with = + - or @ would run
// as a formula when the report is opened in Excel or LibreOffice, so it gets a leading '.
func Cell(s string) string {
	if s != "" && strings.ContainsRune("=+-@", rune(s[0])) {
		return "'" + s
	}
	return s
}

// FromCell undoes Cell when reading a report back.
func FromCell(s string) string {
	if len(s) > 1 && s[0] == '\'' && strings.ContainsRune("=+-@", rune(s[1])) {
		return s[1:]
	}
	return s
}

// WriteCSV writes every entry each yields as a UTF-8 (with BOM) CSV, atomically.
func WriteCSV(out destfs.FS, path string, each func(func(manifest.Entry) error) error) error {
	var buf bytes.Buffer
	buf.WriteString("\xef\xbb\xbf")
	w := csv.NewWriter(&buf)
	w.Write(Header)
	err := each(func(e manifest.Entry) error {
		return w.Write([]string{string(e.Status), string(e.Category), Cell(e.Source), Cell(e.Dest),
			strconv.FormatInt(e.Bytes, 10), strconv.FormatInt(e.BadBytes, 10), e.Header, e.SHA256, Cell(e.Error), fmtTime(e.MTime)})
	})
	if err != nil {
		return err
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	return destfs.WriteFileAtomic(out, path, buf.Bytes())
}

// Summary builds the plain-text summary (mirrors the reference script's copy summary).
type Summary struct {
	Title, Source, Dest, Kind, CSV string
	Backup                         string // the user's backup choice
	Took                           time.Duration
	Profiles                       []string
	Excluded                       map[string]int // suggestion id → files the user chose to exclude
	Issues                         []string

	counts        map[manifest.Status]int
	headers       []string
	unrecoverable []string
}

// Add counts one entry.
// Remove takes back an Add (a file retried at the end of the run is counted again then).
func (s *Summary) Remove(e manifest.Entry) {
	if s.counts[e.Status] > 0 {
		s.counts[e.Status]--
	}
}

func (s *Summary) Add(e manifest.Entry) {
	if s.counts == nil {
		s.counts = map[manifest.Status]int{}
	}
	s.counts[e.Status]++
	switch e.Header {
	case "zeroed", "mismatch", "empty":
		if len(s.headers) < 80 {
			s.headers = append(s.headers, fmt.Sprintf("  [%s] %s", e.Header, e.Source))
		}
	}
	if e.Status == manifest.DELETED_UNRECOVERABLE {
		s.unrecoverable = append(s.unrecoverable, fmt.Sprintf("  %s — %s", e.Source, e.Error))
	}
}

// Count is the number of entries with status st.
func (s *Summary) Count(st manifest.Status) int { return s.counts[st] }

// String renders the summary.
func (s *Summary) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "SD card %s  %s   (took %d min)\n", strings.ToLower(s.Title), time.Now().Format("2006-01-02 15:04:05"), int(s.Took.Minutes()))
	fmt.Fprintf(&b, "Source: %s (read-only, %s)   ->   %s\n", s.Source, s.Kind, s.Dest)
	if len(s.Profiles) > 0 {
		fmt.Fprintf(&b, "Card layout: %s\n", strings.Join(s.Profiles, ", "))
	}
	if s.Backup != "" {
		fmt.Fprintf(&b, "Backup choice: %s\n", s.Backup)
	}
	b.WriteString("\n")
	var sts []string
	for st, n := range s.counts {
		if n > 0 { // a status emptied by a retry isn't shown
			sts = append(sts, string(st))
		}
	}
	sort.Strings(sts)
	for _, st := range sts {
		fmt.Fprintf(&b, "  %-22s %8d\n", st, s.counts[manifest.Status(st)])
	}
	if len(s.Excluded) > 0 {
		b.WriteString("\nExcluded because you chose to skip them:\n")
		var ids []string
		for id := range s.Excluded {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			fmt.Fprintf(&b, "  %-22s %8d\n", id, s.Excluded[id])
		}
	}
	if len(s.Issues) > 0 {
		b.WriteString("\nIssues raised during the run:\n")
		for _, is := range s.Issues {
			b.WriteString("  " + is + "\n")
		}
	}
	b.WriteString("\nHeader check - zeroed/empty = almost certainly corrupt; mismatch = content isn't the type its extension says (often still opens):\n")
	for _, h := range s.headers {
		b.WriteString(h + "\n")
	}
	if len(s.unrecoverable) > 0 {
		b.WriteString("\nDeleted files that could not be recovered (not written):\n")
		for _, u := range s.unrecoverable {
			b.WriteString(u + "\n")
		}
	}
	b.WriteString("\nPARTIAL = copied but some unreadable 4 KB blocks were zero-filled (photo may show grey band, video may glitch).\n")
	b.WriteString("Long paths were shortened (name~hash); the Source/Dest columns in the CSV map each one back.\n")
	b.WriteString("FAILED  = could not be opened/read at all. Run again to retry PARTIAL and FAILED files.\n")
	fmt.Fprintf(&b, "Full detail: %s\n", s.CSV)
	return b.String()
}

// Write saves the summary as UTF-8 with BOM, atomically.
func (s *Summary) Write(out destfs.FS, path string) error {
	return destfs.WriteFileAtomic(out, path, append([]byte("\xef\xbb\xbf"), s.String()...))
}
