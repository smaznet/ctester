package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/aria/x-tester/internal/app"
)

func main() {
	cfgPath := flag.String("c", "config.yaml", "path to config yaml")
	flag.Parse()
	if err := app.Run(*cfgPath); err != nil {
		fmt.Fprintf(os.Stderr, "x-tester: %v\n", err)
		os.Exit(1)
	}
}
