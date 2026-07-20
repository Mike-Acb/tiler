package main

import (
	"flag"
	"fmt"
	"os"

	"tiler/nlscmodelset"
)

func main() {
	input := flag.String("input", "", "path to a browser capture JSON file")
	output := flag.String("output", "", "directory for tileset.json and sample.glb")
	flag.Parse()
	if *input == "" || *output == "" {
		fmt.Fprintln(os.Stderr, "usage: nlsc-modelset-convert -input capture.json -output output-directory")
		os.Exit(2)
	}
	if err := nlscmodelset.ConvertCaptureFile(*input, *output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
