package fsread

import (
	"context"
	"strings"

	"github.com/aoiflux/libxfat"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
)

// deleted reports exFAT's deleted file records (libxfat keeps them; descends deleted directories).
func (d *exfatDriver) deleted(ctx context.Context, emit func(source.Deleted) error) error {
	cs := int64(d.x.ClusterSize())
	opts := libxfat.WalkOptions{IncludeDeleted: true, DescendDeletedDirectories: true}
	return d.x.WalkWithOptions(ctx, opts, func(p string, _ uint32, e libxfat.Entry) error {
		if !e.IsDeleted() || !e.IsFile() || e.IsDir() {
			return nil
		}
		del := source.Deleted{Path: strings.TrimPrefix(p, "/"), Size: int64(e.Size()), MTime: e.ModifiedTime(), State: "metadata-damaged"}
		fc := e.FirstCluster()
		if del.Size == 0 || fc < 2 || e.NameChecksumMismatch() {
			return emit(del)
		}
		res, err := d.x.FragmentOffsetsWithOptions(e, libxfat.FragmentOptions{AssumeContiguous: true})
		if err != nil || res == nil || len(res.Ranges) == 0 {
			return emit(del)
		}
		for _, r := range res.Ranges {
			del.Extents = append(del.Extents, [2]int64{r.StartByte, r.Length})
		}
		n := uint32((del.Size + cs - 1) / cs)
		used := uint32(0)
		for c := fc; c < fc+n; c++ {
			a, err := d.x.IsClusterAllocated(c)
			if err != nil {
				del.State, del.Extents = "metadata-damaged", nil
				return emit(del)
			}
			if a {
				used++
			}
		}
		switch {
		case used == 0:
			del.State = "recoverable"
		case used == n:
			del.State = "overwritten"
		default:
			del.State = "fragmented-unknown"
		}
		return emit(del)
	})
}

// live reports where every live exFAT file's data sits.
func (d *exfatDriver) live(ctx context.Context, emit func(path string, off, n int64) error) error {
	return d.x.Walk(ctx, func(p string, _ uint32, e libxfat.Entry) error {
		if !live(e) || e.IsDir() || e.Size() == 0 {
			return nil
		}
		rs, err := d.x.FragmentOffsets(e)
		if err != nil {
			return nil
		}
		for _, r := range rs {
			if err := emit(strings.TrimPrefix(p, "/"), r.StartByte, r.Length); err != nil {
				return err
			}
		}
		return nil
	})
}
