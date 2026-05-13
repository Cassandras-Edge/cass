package main

import (
	"os"

	"github.com/Cassandras-Edge/cass/internal/cmd"
)

func main() {
	if err := cmd.New().Execute(); err != nil {
		os.Exit(1)
	}
}
