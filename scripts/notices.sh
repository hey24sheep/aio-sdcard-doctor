#!/bin/sh
# Fails if any module built into the release binaries has a licence outside the permissive list,
# then writes THIRD_PARTY_NOTICES.txt: the full licence text of every such module (the union over
# all target OSes, since some dependencies are OS-specific), plus the Go standard library's,
# which every Go binary contains. Run by goreleaser before a release and by CI on every push.
set -eu
allowed=MIT,BSD-2-Clause,BSD-3-Clause,Apache-2.0,ISC,Zlib,0BSD,Unlicense
# MIT per its README; it ships no licence file (text in scripts/licenses)
ignored=github.com/mattn/go-localereader
cd "$(dirname "$0")/.."
tmp=$(mktemp -d)
trap 'rm -r "$tmp"' EXIT
# built for this machine once; GOOS below only selects which dependencies are reported
gobin=$tmp
if command -v cygpath >/dev/null 2>&1; then # Cygwin/MSYS sh: go needs a Windows path in GOBIN
	gobin=$(cygpath -m "$tmp")
fi
GOBIN="$gobin" GOOS= GOARCH= go install github.com/google/go-licenses@v1.6.0
lic=$(ls "$tmp"/go-licenses*)
for os in windows darwin linux; do
	GOOS=$os "$lic" check ./... --allowed_licenses="$allowed" --ignore "$ignored"
	GOOS=$os "$lic" report ./... --template scripts/notices.tpl >"$tmp/$os.txt" 2>"$tmp/$os.err" || {
		cat "$tmp/$os.err" >&2
		exit 1
	}
done
{
	echo "Third-party software included in aio-sdcard-doctor"
	echo
	echo "================================================================"
	echo "Go standard library and runtime (BSD-3-Clause)"
	echo "https://go.dev/LICENSE"
	echo
	cat "$(go env GOROOT)/LICENSE"
	echo
	# one block per module (blocks start with "@@@@ <module>"), first copy wins; modules that ship
	# no licence file are taken from scripts/licenses instead
	cat "$tmp"/*.txt | awk -v ignored="$ignored" '
		/^@@@@ / { name = substr($0, 6); keep = !(name in seen) && name != ignored
		           seen[name] = 1; if (keep) order[++n] = name; next }
		keep { body[name] = body[name] $0 "\n" }
		END { for (i = 1; i <= n; i++) printf "%s", body[order[i]] }'
	cat scripts/licenses/*.txt
	# Apache-2.0 §4(d): a module's NOTICE file travels with it
	for os in windows darwin linux; do
		GOOS=$os go list -deps -f '{{with .Module}}{{.Path}} {{.Dir}}{{end}}' ./...
	done | sort -u | while read -r path dir; do
		for f in "$dir"/NOTICE*; do
			[ -f "$f" ] || continue
			printf '================================================================\n%s (NOTICE)\n\n' "$path"
			cat "$f"
			echo
		done
	done
} >THIRD_PARTY_NOTICES.txt
