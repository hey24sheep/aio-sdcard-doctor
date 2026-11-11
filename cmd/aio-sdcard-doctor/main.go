// Command aio-sdcard-doctor rescues, images, verifies, recovers and fixes SD cards and USB drives.
package main

import (
	"os"
	"runtime/debug"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/cli"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

func main() {
	cli.Version = version
	if version == "dev" { // `go install …@v1.2.3` records the module version in the binary
		if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			cli.Version = bi.Main.Version
		}
	}
	os.Exit(cli.Main(os.Args[1:], cli.StdIO()))
}
