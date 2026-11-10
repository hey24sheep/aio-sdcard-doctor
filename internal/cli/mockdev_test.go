package cli

import (
	"runtime"
	"strings"
	"time"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/rescue"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/source"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/worker"
)

// mockDevice is a device path that no test ever opens: openSource answers it with "needs admin",
// so no test touches a real disk (the user's rule: mock cards, never touch real drives).
func mockDevice() string {
	if runtime.GOOS == "windows" {
		return `\\.\PhysicalDriveMOCK`
	}
	return "/dev/mockcard"
}

func init() {
	real := openSource
	openSource = func(s worker.Spec, stall time.Duration) (rescue.Source, error) {
		if strings.Contains(s.From, "MOCK") || strings.Contains(s.From, "mockcard") {
			return nil, source.ErrNeedsAdmin
		}
		return real(s, stall)
	}
}
