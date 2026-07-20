package main

import (
	"os"

	"godbsync/cmd"
)

func main() {
	os.Exit(cmd.Execute())
}
