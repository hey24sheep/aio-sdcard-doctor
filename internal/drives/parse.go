package drives

// OS-independent parsers, so every CI job tests every OS's parsing.

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

type mount struct {
	dev, dir, fs string
	ro           bool
}

// unescapeMount decodes the octal escapes /proc/self/mounts uses for spaces etc.
func unescapeMount(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// parseProcMounts returns block-device mounts from /proc/self/mounts.
func parseProcMounts(r io.Reader) []mount {
	var out []mount
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 4 || !strings.HasPrefix(f[0], "/dev/") {
			continue
		}
		ro := false
		for _, o := range strings.Split(f[3], ",") {
			ro = ro || o == "ro"
		}
		out = append(out, mount{dev: unescapeMount(f[0]), dir: unescapeMount(f[1]), fs: f[2], ro: ro})
	}
	return out
}

func readTrim(fsys fs.FS, p string) string {
	b, err := fs.ReadFile(fsys, p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readInt(fsys fs.FS, p string) int64 {
	n, _ := strconv.ParseInt(readTrim(fsys, p), 10, 64)
	return n
}

// systemMount: a partition mounted here belongs to the running system.
var systemMount = map[string]bool{"/": true, "/boot": true, "/boot/efi": true, "/efi": true, "/usr": true, "/var": true, "/home": true}

// parseSysfs lists physical disks from a /sys tree. mounts is keyed by device path.
func parseSysfs(sys fs.FS, mounts map[string]mount) []Device {
	ents, _ := fs.ReadDir(sys, "block")
	var out []Device
	for _, e := range ents {
		name := e.Name()
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") || strings.HasPrefix(name, "zram") ||
			strings.HasPrefix(name, "dm-") || strings.HasPrefix(name, "sr") {
			continue
		}
		dir := path.Join("block", name)
		d := Device{Path: "/dev/" + name, PhysicalID: name, Size: readInt(sys, dir+"/size") * 512}
		d.ReadOnly = readTrim(sys, dir+"/ro") == "1"
		model := readTrim(sys, dir+"/device/model")
		if v := readTrim(sys, dir+"/device/vendor"); v != "" && model != "" {
			model = v + " " + model
		}
		if model == "" {
			model = readTrim(sys, dir+"/device/name")
		}
		d.Model = model
		d.Serial = readTrim(sys, dir+"/device/serial")
		mmc := strings.HasPrefix(name, "mmcblk")
		switch {
		case strings.HasPrefix(name, "nvme"):
			d.Bus = "nvme"
		case mmc:
			d.Bus = "sd"
		}
		t := readTrim(sys, dir+"/device/type")
		d.Removable = readTrim(sys, dir+"/removable") == "1" || mmc && (t == "SD" || t == "MMC")
		if m, ok := mounts[d.Path]; ok { // a card formatted without partitions (superfloppy)
			d.Mounted = true
			d.Volumes = append(d.Volumes, Drive{PhysicalID: name, Device: d.Path, Root: m.dir, FS: m.fs,
				WriteProtected: m.ro, System: systemMount[m.dir], Total: uint64(d.Size)})
		}
		if hs, _ := fs.ReadDir(sys, dir+"/holders"); len(hs) > 0 {
			d.InUse = "part of an LVM, RAID or encrypted volume (" + hs[0].Name() + ")"
		}
		parts, _ := fs.ReadDir(sys, dir)
		for _, p := range parts {
			pn := p.Name()
			if !p.IsDir() || !strings.HasPrefix(pn, name) || readTrim(sys, dir+"/"+pn+"/size") == "" {
				continue
			}
			v := Drive{PhysicalID: name, Device: "/dev/" + pn, Total: uint64(readInt(sys, dir+"/"+pn+"/size") * 512)}
			if m, ok := mounts["/dev/"+pn]; ok {
				v.Root, v.FS, v.WriteProtected, v.System = m.dir, m.fs, m.ro, systemMount[m.dir]
				d.Mounted = true
			}
			if hs, _ := fs.ReadDir(sys, dir+"/"+pn+"/holders"); len(hs) > 0 && d.InUse == "" {
				d.InUse = "part of an LVM, RAID or encrypted volume (" + hs[0].Name() + ")"
			}
			d.Volumes = append(d.Volumes, v)
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// plist is a decoded property-list dictionary (diskutil -plist output).
type plist map[string]any

func (p plist) str(k string) string { s, _ := p[k].(string); return s }
func (p plist) num(k string) int64  { n, _ := p[k].(int64); return n }
func (p plist) boolean(k string) bool {
	b, _ := p[k].(bool)
	return b
}
func (p plist) list(k string) []plist {
	a, _ := p[k].([]any)
	var out []plist
	for _, v := range a {
		if d, ok := v.(plist); ok {
			out = append(out, d)
		}
	}
	return out
}

// parsePlistDict decodes the top-level <dict> of an XML property list (bounded nesting).
func parsePlistDict(b []byte) (plist, error) {
	dec := xml.NewDecoder(bytes.NewReader(b))
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("plist: no dict: %w", err)
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "dict" {
			v, err := plistValue(dec, se, 0)
			if err != nil {
				return nil, err
			}
			return v.(plist), nil
		}
	}
}

func plistValue(dec *xml.Decoder, se xml.StartElement, depth int) (any, error) {
	if depth > 32 {
		return nil, fmt.Errorf("plist: too deep")
	}
	switch se.Name.Local {
	case "dict":
		d := plist{}
		key := ""
		for {
			tok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.EndElement:
				return d, nil
			case xml.StartElement:
				if t.Name.Local == "key" {
					var k string
					if err := dec.DecodeElement(&k, &t); err != nil {
						return nil, err
					}
					key = k
					continue
				}
				v, err := plistValue(dec, t, depth+1)
				if err != nil {
					return nil, err
				}
				d[key] = v
			}
		}
	case "array":
		var a []any
		for {
			tok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.EndElement:
				return a, nil
			case xml.StartElement:
				v, err := plistValue(dec, t, depth+1)
				if err != nil {
					return nil, err
				}
				a = append(a, v)
			}
		}
	case "true", "false":
		dec.Skip()
		return se.Name.Local == "true", nil
	default:
		var s string
		if err := dec.DecodeElement(&s, &se); err != nil {
			return nil, err
		}
		if se.Name.Local == "integer" {
			n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
			return n, err
		}
		return s, nil
	}
}
