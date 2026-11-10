package profile

import "strings"

// expected maps an extension to the content family its header must show.
var expected = func() map[string]string {
	m := map[string]string{}
	add := func(fam, exts string) {
		for _, e := range strings.Fields(exts) {
			m[e] = fam
		}
	}
	add("jpeg", "jpg jpeg jfif")
	add("png", "png")
	add("gif", "gif")
	add("bmp", "bmp")
	add("webp", "webp")
	add("tiff", "tif tiff dng cr2 nef nrw arw sr2 srf pef srw 3fr dcr kdc erf mos mrw orf rw2 iiq gpr")
	add("raf", "raf")
	add("isobmff", "heic heif avif mp4 mov m4v 3gp 3g2 m4a cr3 insv insp")
	add("ts", "ts mts m2ts")
	add("pdf", "pdf")
	add("zip", "zip docx xlsx pptx odt ods odp epub")
	add("ebml", "mkv webm")
	add("avi", "avi")
	add("wav", "wav bwf")
	add("flac", "flac")
	add("ogg", "ogg opus")
	add("7z", "7z")
	add("rar", "rar")
	add("gz", "gz tgz")
	return m
}()

// family names the content family shown by a file's first bytes ("" = unknown).
func family(b []byte) string {
	switch {
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "jpeg"
	case at(b, 0, "\x89PNG"):
		return "png"
	case at(b, 0, "GIF8"):
		return "gif"
	case at(b, 0, "BM") && len(b) >= 14:
		return "bmp"
	case at(b, 0, "RIFF") && at(b, 8, "WEBP"):
		return "webp"
	case at(b, 0, "RIFF") && at(b, 8, "AVI "):
		return "avi"
	case at(b, 0, "RIFF") && at(b, 8, "WAVE"), at(b, 0, "RF64"):
		return "wav"
	case at(b, 0, "II*\x00"), at(b, 0, "MM\x00*"), at(b, 0, "IIRO"), at(b, 0, "IIU\x00"):
		return "tiff"
	case at(b, 0, "FUJIFILMCCD-RAW"):
		return "raf"
	case at(b, 4, "ftyp"):
		return "isobmff"
	case tsKind(b) != "":
		return "ts"
	case at(b, 0, "%PDF"):
		return "pdf"
	case at(b, 0, "PK\x03\x04"), at(b, 0, "PK\x05\x06"):
		return "zip"
	case at(b, 0, "\x1a\x45\xdf\xa3"):
		return "ebml"
	case at(b, 0, "fLaC"):
		return "flac"
	case at(b, 0, "OggS"):
		return "ogg"
	case at(b, 0, "7z\xbc\xaf\x27\x1c"):
		return "7z"
	case at(b, 0, "Rar!"):
		return "rar"
	case len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b:
		return "gz"
	}
	return ""
}

// HeaderCheck compares a file's first bytes with its extension:
// "empty" (0 bytes), "zeroed" (all zero), "n/a" (no expected family for the extension),
// "ok" or "mismatch" (informational: e.g. a HEIC photo named .jpg).
func HeaderCheck(ext string, head []byte) string {
	if len(head) == 0 {
		return "empty"
	}
	if allZero(head) {
		return "zeroed"
	}
	want, ok := expected[normExt(ext)]
	if !ok {
		return "n/a"
	}
	if family(head) == want {
		return "ok"
	}
	return "mismatch"
}
