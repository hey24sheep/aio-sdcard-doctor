package profile

import "strings"

var extTable = map[Category]string{
	Photos: "jpg jpeg jfif png gif bmp webp heic heif avif jxl dng cr2 cr3 crw nef nrw arw sr2 srf raf orf rw2 " +
		"pef srw x3f 3fr dcr kdc erf mos mrw rwl iiq gpr tif tiff insp hif mpo",
	Videos:    "mp4 mkv mov avi 3gp 3g2 webm m4v wmv flv mpg mpeg ts mts m2ts vob mod tod mxf insv braw r3d 360 264 h264 h265 hevc dav",
	Music:     "mp3 m4a aac wav bwf flac ogg opus amr wma mid midi aif aiff ape wv dsf dff 3ga m4b m4r oga spx",
	Documents: "pdf doc docx xls xlsx ppt pptx odt ods odp txt rtf csv vcf epub",
	Archives:  "zip 7z rar tar gz tgz bz2 xz",
	// WhatsApp encrypted chat backups, wherever they were copied to
	ChatBackups: "crypt crypt5 crypt7 crypt8 crypt12 crypt14 crypt15",
}

// sidecars take the category of the media file with the same folder and stem.
var sidecars = set("thm xmp srt lrv lrf xml aae")

var byExt = func() map[string]Category {
	m := map[string]Category{}
	for c, list := range extTable {
		for _, e := range strings.Fields(list) {
			m[e] = c
		}
	}
	return m
}()

func set(list string) map[string]bool {
	m := map[string]bool{}
	for _, e := range strings.Fields(list) {
		m[e] = true
	}
	return m
}

func normExt(ext string) string { return strings.ToLower(strings.TrimPrefix(ext, ".")) }

// ByExt maps a file extension (with or without the dot, any case) to its category.
func ByExt(ext string) (Category, bool) {
	c, ok := byExt[normExt(ext)]
	return c, ok
}

// IsSidecar reports extensions that follow their sibling media file.
func IsSidecar(ext string) bool { return sidecars[normExt(ext)] }
