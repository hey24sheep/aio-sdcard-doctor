#!/usr/bin/env bash
# Builds the golden file-system images read by TestRealFSImages (./internal/fsread); the set in
# internal/fsread/testdata/realfs came from this script (CI job "fsimages", run by hand).
# Dev/CI only: the GPL tools used here (mtools, dosfstools, exfatprogs, ntfs-3g, e2fsprogs,
# xorriso) are never shipped or linked. Needs root for the exFAT/NTFS loop mounts.
# Usage: sudo scripts/gen-fs-images.sh OUTDIR
set -euo pipefail
out="${1:?usage: gen-fs-images.sh OUTDIR}"
mkdir -p "$out"
work="$(mktemp -d)"
trap 'umount -q "$work/mnt" 2>/dev/null || true; rm -rf "$work"' EXIT
mkdir -p "$work/mnt"

# fill FILE SIZE TAG — deterministic content, distinct per tag.
fill() { { yes "$3" || true; } | head -c "$2" > "$1"; }   # yes dies of SIGPIPE when head is done

# The tree every image starts with (same names in every FS).
mktree() {
  local d="$1" cs="$2"
  mkdir -p "$d/DCIM/Camera" "$d/a/b/c/d" "$d/Docs" "$d/हिंदी" "$d/emoji 😀"
  fill "$d/DCIM/Camera/IMG_0001.jpg" 5000 jpg1
  fill "$d/a/b/c/d/deep.txt" 300 deep
  : > "$d/Docs/empty.txt"
  fill "$d/Docs/cluster.bin" "$cs" cluster
  fill "$d/Docs/cluster1.bin" "$((cs + 1))" cluster1
  fill "$d/हिंदी/नमस्ते.txt" 100 hindi
  fill "$d/emoji 😀/smile.txt" 100 emoji
}

# FAT via mtools (no mount): live tree + fragmented, deleted, overwritten files.
fat() {
  local name="$1" fatbits="$2" sizekb="$3" cs=2048 img="$out/$1.img"
  rm -f "$img"; mkfs.vfat -C -F "$fatbits" -s 4 -S 512 -n "${name^^}" "$img" "$sizekb" >/dev/null
  export MTOOLS_SKIP_CHECK=1
  mktree "$work/tree-$name" "$cs"
  mcopy -s -i "$img" "$work/tree-$name"/* ::/
  fill "$work/a1" "$cs" a1; fill "$work/gap" "$cs" gap; fill "$work/a2" "$cs" a2
  mcopy -i "$img" "$work/a1" ::/A1.BIN; mcopy -i "$img" "$work/gap" ::/GAP.BIN; mcopy -i "$img" "$work/a2" ::/A2.BIN
  mdel -i "$img" ::/GAP.BIN
  fill "$work/frag" "$((cs * 3))" frag; mcopy -i "$img" "$work/frag" ::/FRAG.BIN          # fills the gap, continues after A2 → fragmented
  # Deleted files live in DEL/, where nothing is created afterwards: a new file takes the first
  # free directory slot, which would erase a deleted record in the same folder. DELETED.JPG is
  # deleted last, so NEWER.BIN can reuse only OVERWR.JPG's freed cluster.
  mmd -i "$img" ::/DEL
  fill "$work/del" "$((cs * 2))" deleted; mcopy -i "$img" "$work/del" ::/DEL/DELETED.JPG
  fill "$work/ow" "$cs" overwritten; mcopy -i "$img" "$work/ow" ::/DEL/OVERWR.JPG; mdel -i "$img" ::/DEL/OVERWR.JPG
  fill "$work/new" "$cs" newer; mcopy -i "$img" "$work/new" ::/NEWER.BIN
  mdel -i "$img" ::/DEL/DELETED.JPG
  gzip -9nf "$img"
}

fat fat12 12 1440
fat fat16 16 16384
fat fat32 32 34000

# exFAT and NTFS need a loop mount for realistic writes and deletes.
mounted() {
  local name="$1" mkfs="$2" sizemb="$3" cs=4096 img="$out/$1.img"
  rm -f "$img"; truncate -s "${sizemb}M" "$img"
  $mkfs "$img" >/dev/null
  mount -o loop "$img" "$work/mnt"
  mktree "$work/mnt" "$cs"
  fill "$work/mnt/A1.BIN" "$cs" a1; fill "$work/mnt/GAP.BIN" "$cs" gap; fill "$work/mnt/A2.BIN" "$cs" a2; sync
  rm "$work/mnt/GAP.BIN"; sync
  fill "$work/mnt/FRAG.BIN" "$((cs * 3))" frag; sync
  mkdir "$work/mnt/DEL"   # deleted records in their own folder (see fat above)
  fill "$work/mnt/DEL/DELETED.JPG" "$((cs * 2))" deleted; sync
  fill "$work/mnt/DEL/OVERWR.JPG" "$cs" overwritten; sync; rm "$work/mnt/DEL/OVERWR.JPG"; sync
  fill "$work/mnt/NEWER.BIN" "$cs" newer; sync
  rm "$work/mnt/DEL/DELETED.JPG"; sync
  umount "$work/mnt"
  gzip -9nf "$img"
}
mounted exfat "mkfs.exfat -L EXFAT" 8
mounted ntfs "mkntfs -F -Q -L NTFS" 8

# ext4: populated from a directory, no mount.
mktree "$work/tree-ext4" 4096
rm -f "$out/ext4.img"; mke2fs -q -t ext4 -L EXT4 -d "$work/tree-ext4" "$out/ext4.img" 4M && gzip -9nf "$out/ext4.img"

# ISO9660 with Rock Ridge + Joliet.
mktree "$work/tree-iso" 2048
xorriso -as mkisofs -quiet -R -J -V ISO -o "$out/iso9660.img" "$work/tree-iso" && gzip -9nf "$out/iso9660.img"

# MBR and GPT disks with a FAT32 and an exFAT partition.
for table in msdos gpt; do
  img="$out/disk-$table.img"; rm -f "$img"; truncate -s 80M "$img"
  parted -s "$img" mklabel "$table" mkpart primary fat32 1MiB 41MiB mkpart primary 41MiB 79MiB
  loop="$(losetup -Pf --show "$img")"
  mkfs.vfat -F 32 -n PART1 "${loop}p1" >/dev/null; mkfs.exfat -L PART2 "${loop}p2" >/dev/null
  mount -o utf8 "${loop}p1" "$work/mnt"; mktree "$work/mnt" 2048; umount "$work/mnt"   # utf8: else vfat mangles हिंदी/😀
  losetup -d "$loop"; gzip -9nf "$img"
done
ls -l "$out"
