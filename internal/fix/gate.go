package fix

import (
	"context"
	"strings"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/preflight"
)

// GateInput is what Gate needs besides the target.
type GateInput struct {
	Op         string
	Ctx        context.Context // Stop ends the look for a rescue (nil: none)
	Verified   bool            // a verified rescue (manifest + ALL FILES MATCH) of this card exists
	AllowFixed bool            // --allow-fixed
	Admin      bool            // running as administrator/root
	BinaryDisk string          // PhysicalID of the disk this program runs from
	HasRescue  func(root string) (found, complete bool)
}

// uncheckedWarning starts the warning for a disk too big to look through for a rescue.
const uncheckedWarning = "There are too many folders on "

// fixedBuses are internal-disk buses: never a card in a reader.
var fixedBuses = map[string]bool{"nvme": true, "sata": true, "sas": true, "raid": true, "ata": true}

// Gate decides whether an operation may touch the target at all. It checks every volume on the
// physical disk, not only the one named (R6). warnings need a second confirmation.
func Gate(t Target, in GateInput) (warnings []string, err error) {
	vols := append([]drives.Drive{t.Vol}, t.Disk.Volumes...) // the named volume always counts
	has := in.HasRescue
	if has == nil {
		ctx := in.Ctx
		if ctx == nil {
			ctx = context.Background()
		}
		has = func(root string) (bool, bool) { return hasRescue(ctx, root) }
	}
	for _, v := range vols {
		if v.System {
			return nil, &Refused{"this disk holds your system (" + v.Root + ")"}
		}
	}
	if t.Disk.InUse != "" {
		return nil, &Refused{"the operating system uses this whole disk: " + t.Disk.InUse}
	}
	if in.BinaryDisk != "" && in.BinaryDisk == t.Disk.PhysicalID {
		return nil, &Refused{"this disk holds this program — run it from another disk"}
	}
	var unchecked []string // volumes too big to look through completely
	walked := map[string]bool{}
	for _, v := range vols {
		// a check only reads, so a rescue on the disk can't be hurt (SA-8); the named volume is in
		// the disk's list too, and one walk of a dying card is enough (OA7)
		if in.Op == OpCheck || walked[v.Root] {
			continue
		}
		walked[v.Root] = true
		found, complete := has(v.Root)
		if found {
			return nil, &Refused{"this disk holds a rescue (" + v.Root + ") — fixing it could destroy your copy"}
		}
		if !complete {
			unchecked = append(unchecked, v.Root)
		}
	}
	if !in.AllowFixed {
		switch {
		case !t.Disk.Removable:
			return nil, &Refused{"this is a fixed disk, not a card or USB stick — add --allow-fixed if you mean it"}
		case fixedBuses[strings.ToLower(t.Disk.Bus)]:
			return nil, &Refused{"this is an internal " + strings.ToUpper(t.Disk.Bus) + " disk — add --allow-fixed if you mean it"}
		case t.Size() > 2<<40:
			return nil, &Refused{"this disk is larger than 2 TB, so not a card — add --allow-fixed if you mean it"}
		}
	}
	writes := in.Op != OpCheck
	if writes && (t.Disk.ReadOnly || t.Vol.WriteProtected) {
		return nil, &Refused{"the card is write-protected — slide the lock switch on the card"}
	}
	for _, v := range vols {
		if v.Locked {
			return nil, &Refused{"the card is locked (encrypted) — unlock it in Explorer/Disk Utility first"}
		}
	}
	if !in.Admin {
		return nil, ErrNeedsAdmin
	}
	if writes && len(unchecked) > 0 {
		warnings = append(warnings, uncheckedWarning+strings.Join(unchecked, ", ")+" to check them all for a rescue or a card image. "+
			"If one you need is on this card, "+strings.ToLower(opWord(in.Op))+" will erase it: make sure it isn't.")
	}
	if writes && !in.Verified {
		warnings = append(warnings, "There is no verified rescue of this card. Rescue (or Image) and Verify it first: "+
			strings.ToLower(opWord(in.Op))+" can erase what is still on it.")
	}
	return warnings, nil
}

func opWord(op string) string {
	switch op {
	case OpRepair:
		return "A repair"
	case OpFormat:
		return "Formatting"
	case OpHealth:
		return "The health test"
	}
	return "A check"
}

// SizeText is the card's size exactly as it is shown and must be typed.
func SizeText(t Target) string { return preflight.GB(t.Size()) }

// ConfirmLabel is what the user types as the label: the label itself (case-sensitive), or the
// drive letter / mount point when the card has none.
func ConfirmLabel(t Target) string {
	if t.Vol.Label != "" {
		return t.Vol.Label
	}
	if r := strings.TrimRight(t.Vol.Root, `\/`); r != "" {
		return r
	}
	return t.Disk.PhysicalID
}

// Confirm checks what the user typed: the label and the size, both as shown.
func Confirm(t Target, label, size string) error {
	norm := func(s string) string { return strings.ToLower(strings.ReplaceAll(s, " ", "")) }
	switch {
	case label != ConfirmLabel(t):
		return &Refused{"the label typed doesn't match — type " + ConfirmLabel(t) + " exactly"}
	case size == "" || norm(size) != norm(SizeText(t)):
		return &Refused{"the size typed doesn't match — type " + SizeText(t) + " as shown"}
	}
	return nil
}
