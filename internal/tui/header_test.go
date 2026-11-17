package tui

import (
	"strings"
	"testing"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/app"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/drives"
)

// The header promised "Only read" in every flow, Card tools included — where repair, format and
// the health test write to the card. It says so there instead.
func TestHeaderNeverSaysOnlyReadForCardTools(t *testing.T) {
	card := &drives.Drive{Root: `E:\`, Label: "CARD", FS: "exFAT", Total: 64 << 30}
	for _, c := range []struct {
		req  app.Request
		want string
	}{
		{app.Request{Action: "rescue"}, "Only read"},
		{app.Request{Action: "fix", FixOp: "check"}, "Only read"},
		{app.Request{Action: "fix"}, "write to the card"},
		{app.Request{Action: "fix", FixOp: "format"}, "write to the card"},
		{app.Request{Action: "fix", FixOp: "health-test"}, "write to the card"},
	} {
		m := &Model{w: 120, v: app.View{Card: card, Req: c.req}}
		h := m.header(m.pal())
		if !strings.Contains(h, c.want) || c.want != "Only read" && strings.Contains(h, "Only read") {
			t.Errorf("%+v: %q", c.req, h)
		}
	}
}
