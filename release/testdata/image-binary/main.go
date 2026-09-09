package main

import (
	"fmt"

	"github.com/goobers/goobers/internal/version"
)

func main() {
	fmt.Println(version.Get())
}
