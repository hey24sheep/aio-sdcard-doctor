package worker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// mbrImage is an 8 MiB disk image with parts primary partitions, every byte outside the
// partition table set to fill (so two images are told apart by content).
func mbrImage(parts int, fill byte) []byte {
	b := bytes.Repeat([]byte{fill}, 8<<20)
	copy(b[:512], make([]byte, 512))
	for i := 0; i < parts; i++ {
		e := b[446+16*i:]
		e[4] = 0x0C
		binary.LittleEndian.PutUint32(e[8:], uint32(2048+i*4096))
		binary.LittleEndian.PutUint32(e[12:], 4096)
	}
	b[510], b[511] = 0x55, 0xAA
	return b
}

// A respawn whose open fails (the card was briefly away) must not leave a worker with nothing
// open behind: once the card is back, reads work again.
func TestFailedReopenRetriesWhenCardIsBack(t *testing.T) {
	d := folder(t)
	c := spawn(t, Spec{From: d}, 5*time.Second)
	if _, err := c.ReadDir("."); err != nil {
		t.Fatal(err)
	}
	away := d + "_away"
	if err := os.Rename(d, away); err != nil {
		t.Fatal(err)
	}
	c.hook("Crash")
	if _, err := c.ReadDir("."); err == nil {
		t.Fatal("read with the card away succeeded")
	}
	if err := os.Rename(away, d); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadDir("."); err != nil {
		t.Fatalf("read after the card is back: %v", err)
	}
}

// A respawn that opens a different card at the same path, where the open itself fails (the
// partition asked for isn't there), must never hand out that card's bytes.
func TestFailedReopenNeverReadsAStranger(t *testing.T) {
	img := filepath.Join(t.TempDir(), "card.img")
	os.WriteFile(img, mbrImage(2, 'A'), 0o644)
	c := spawn(t, Spec{From: img, Raw: true, Volume: 1}, 5*time.Second)
	r, _, cl, err := c.OpenDevice()
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	b := make([]byte, 4)
	if _, err := r.ReadAt(b, 4<<20); err != nil || string(b) != "AAAA" {
		t.Fatalf("first card: %q %v", b, err)
	}
	c.hook("Crash")
	os.WriteFile(img, mbrImage(1, 'B'), 0o644) // another card, one partition
	for i := 0; i < 3; i++ {
		b = make([]byte, 4)
		n, err := r.ReadAt(b, 4<<20)
		if err == nil || bytes.Contains(b[:n], []byte("B")) {
			t.Fatalf("read %d of a different card: n=%d %q err=%v", i, n, b[:n], err)
		}
	}
}

// OA7: a listing (whose first page may take 30× the stall timeout) ignored Stop. A cancelled
// call returns at once; the next call gets a fresh worker.
func TestCancelledCallReturnsAtOnce(t *testing.T) {
	d := folder(t)
	c := spawn(t, Spec{From: d}, 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	if err := c.callCtx(ctx, "W.Hook", "Hang", &struct{}{}, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("Stop took %v", el)
	}
	if _, err := c.ReadDir("."); err != nil {
		t.Fatalf("after Stop: %v", err)
	}
}

// OA12/SA-9: only the listings honoured Stop. Every other call — a read through a file or device
// handle, Validate, and the re-open of a respawned worker (up to a minute on a hung card) — kept
// Stop waiting. The run's context, once set, ends them all.
func TestStopEndsEveryCallAndAReopen(t *testing.T) {
	d := folder(t)
	c := spawn(t, Spec{From: d}, 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	c.SetContext(ctx)
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	if err := c.hook("Hang"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("Stop took %v", el)
	}
	// a respawn whose new worker never answers its open
	ctx, cancel = context.WithCancel(context.Background())
	c.SetContext(ctx)
	old := Command
	defer func() { Command = old }()
	Command = func() *exec.Cmd {
		cmd := old()
		cmd.Env = append(cmd.Env, envHang+"=1")
		return cmd
	}
	time.AfterFunc(200*time.Millisecond, cancel)
	start = time.Now()
	if _, err := c.ReadDir("."); !errors.Is(err, context.Canceled) {
		t.Fatalf("reopen: err %v", err)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("Stop during a reopen took %v", el)
	}
}
