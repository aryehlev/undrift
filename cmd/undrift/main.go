// Command undrift keeps independently evolving implementations in sync with
// their upstream projects.
package main

import (
	"os"

	"github.com/aryehlev/undrift/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
