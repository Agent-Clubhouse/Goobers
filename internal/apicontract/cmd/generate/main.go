package main

import (
	"flag"
	"log"
	"os"

	"github.com/goobers/goobers/internal/apicontract"
)

func main() {
	contractOutput := flag.String("contract-output", "", "generated TypeScript contract output path")
	fixturesOutput := flag.String("fixtures-output", "", "generated TypeScript wire fixtures output path")
	flag.Parse()
	if *contractOutput == "" {
		log.Fatal("-contract-output is required")
	}
	if *fixturesOutput == "" {
		log.Fatal("-fixtures-output is required")
	}
	contract, err := apicontract.TypeScriptContract()
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*contractOutput, contract, 0o644); err != nil {
		log.Fatal(err)
	}
	fixtures, err := apicontract.TypeScriptWireFixtures()
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*fixturesOutput, fixtures, 0o644); err != nil {
		log.Fatal(err)
	}
}
