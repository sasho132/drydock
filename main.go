// Command drydock keeps Docker containers up to date, safely.
//
// It pulls new images on a schedule, recreates containers from their exact
// configuration, verifies that the replacement is healthy, and automatically
// restores the previous container when it is not.
package main

import (
	"os"

	"github.com/sasho/drydock/cmd"
)

func main() {
	os.Exit(cmd.Main())
}
