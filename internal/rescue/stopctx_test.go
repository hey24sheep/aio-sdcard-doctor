package rescue

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/job"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/testutil"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/worker"
)

// ctxFake records the context the reader process was told to stop with.
type ctxFake struct {
	*fake
	got []context.Context
}

func (c *ctxFake) SetContext(ctx context.Context) { c.got = append(c.got, ctx) }

type ctxKey struct{}

// OA12/SA-9: Stop must end a read stuck on the card (and a re-open after the reader restarts),
// so Plan and Run hand their own context to the reader process.
func TestStopReachesTheReaderProcess(t *testing.T) {
	src, dst := testutil.WriteFolder(t, testutil.CameraCard()), filepath.Join(t.TempDir(), "out")
	f := &ctxFake{fake: newFake(t, src)}
	r := newRescue(src, dst, f.fake)
	r.OpenSource = func(worker.Spec, time.Duration) (Source, error) { return f, nil }
	planCtx := context.WithValue(context.Background(), ctxKey{}, "plan")
	runCtx := context.WithValue(context.Background(), ctxKey{}, "run")
	env := job.Env{Prompt: &prompter{}}
	p, err := r.Plan(planCtx, env)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Run(runCtx, env, p); err != nil {
		t.Fatal(err)
	}
	if len(f.got) < 2 || f.got[0].Value(ctxKey{}) != "plan" || f.got[len(f.got)-1].Value(ctxKey{}) != "run" {
		t.Fatalf("reader contexts %v", f.got)
	}
}
