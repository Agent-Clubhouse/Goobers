// Command architectureimports checks reviewed library import boundaries using
// the package graph reported by the Go tool.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
)

func main() {
	configPath := flag.String("config", "", "path to the reviewed import-boundary configuration")
	root := flag.String("root", ".", "module directory passed to the Go tool")
	flag.Parse()

	if *configPath == "" || flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	config, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "architectureimports: %v\n", err)
		os.Exit(1)
	}
	message, err := check(context.Background(), *root, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "architectureimports: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("architectureimports: %s\n", message)
}
