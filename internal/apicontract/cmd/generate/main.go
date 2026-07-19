package main

import (
	"flag"
	"log"
	"os"

	"github.com/goobers/goobers/internal/apicontract"
)

func main() {
	output := flag.String("output", "", "generated TypeScript output path")
	flag.Parse()
	if *output == "" {
		log.Fatal("-output is required")
	}
	content, err := apicontract.TypeScriptContract()
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*output, content, 0o644); err != nil {
		log.Fatal(err)
	}
}
