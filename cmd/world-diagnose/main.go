package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"worldsim/internal/diagnostic"
	"worldsim/internal/fsutil"
)

func main() {
	world := flag.String("world", "", "world directory (read only)")
	output := flag.String("output", "", "optional report path outside the world directory")
	flag.Parse()
	if *world == "" {
		fmt.Fprintln(os.Stderr, "-world is required")
		os.Exit(2)
	}
	report, err := diagnostic.Analyze(*world)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	b, _ := json.MarshalIndent(report, "", "  ")
	if *output != "" {
		if err := os.MkdirAll(filepath.Dir(*output), 0755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := fsutil.WriteFileAtomic(*output, b); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	fmt.Println(string(b))
}
