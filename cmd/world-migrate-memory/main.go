// world-migrate-memory previews or explicitly archives legacy memories without
// modifying the source or promoting unverified text to character knowledge.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"worldsim/internal/characteragent"
)

func run(args []string, out, errOut io.Writer) int {
	flags := flag.NewFlagSet("world-migrate-memory", flag.ContinueOnError)
	flags.SetOutput(errOut)
	world := flags.String("world", "", "existing world directory (stop the world before applying)")
	apply := flags.Bool("apply", false, "explicitly create quarantine archives; source is never changed")
	planID := flags.String("plan", "", "plan ID returned by preview; required with --apply")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *world == "" || flags.NArg() != 0 || (*apply && *planID == "") {
		fmt.Fprintln(errOut, "usage: world-migrate-memory --world <directory> [--apply --plan <preview-id>]")
		return 2
	}
	var plan characteragent.LegacyMigrationPlan
	var err error
	mode := "preview"
	if *apply {
		mode = "applied_quarantine_only"
		plan, err = characteragent.ApplyLegacyMigration(*world, *planID)
	} else {
		plan, err = characteragent.PreviewLegacyMigration(*world)
	}
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(struct {
		Mode string                             `json:"mode"`
		Plan characteragent.LegacyMigrationPlan `json:"plan"`
	}{mode, plan}); err != nil {
		fmt.Fprintln(errOut, "cannot write migration report")
		return 1
	}
	return 0
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
