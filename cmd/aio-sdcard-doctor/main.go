// Command aio-sdcard-doctor rescues, images, verifies, recovers and fixes SD cards and USB drives.
package main

import (
	"os"

	"github.com/hey24sheep/aio-sdcard-doctor/internal/cli"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

func main() {
	cli.Version = version
	os.Exit(cli.Main(os.Args[1:], cli.StdIO()))
}
