package cli

import (
	"github.com/spf13/cobra"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/app"
	"github.com/hey24sheep/aio-sdcard-doctor/internal/gui"
)

// Seams: tests never open a real window.
var (
	openWindow = gui.OpenAppWindow
	appDeps    = app.ProductionDeps
)

func guiCmd(io IO) *cobra.Command {
	var noOpen bool
	var port int
	c := &cobra.Command{
		Use:   "gui",
		Short: "Open the app in a window (a local page only this computer can reach)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGUI(cmd.Context(), io, noOpen, port)
		},
	}
	c.Flags().BoolVar(&noOpen, "no-open", false, "don't open a window; just print the link")
	c.Flags().IntVar(&port, "port", 0, "port on 127.0.0.1 (0 = any free port)")
	return c
}
