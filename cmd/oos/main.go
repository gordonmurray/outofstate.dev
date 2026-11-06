// Command oos finds AWS resources that are not tracked in Terraform state.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/gordonmurray/outofstate.dev/internal/discover"
	"github.com/gordonmurray/outofstate.dev/internal/report"
	"github.com/gordonmurray/outofstate.dev/internal/scan"
	"github.com/gordonmurray/outofstate.dev/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, time.Now))
}

type options struct {
	profile       string
	regions       []string
	sources       []string
	stateFiles    []string
	tfBinary      string
	sourceTimeout time.Duration
	format        string
	output        string
	noCost        bool
	noColor       bool
	inventoryFile string // hidden: fake inventory for tests until live discovery (M3)
}

func run(args []string, stdout, stderr io.Writer, now func() time.Time) int {
	exit := scan.ExitOK
	cmd := newRootCommand(stdout, now, &exit)
	cmd.SetArgs(args)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	if err := cmd.Execute(); err != nil {
		_, _ = fmt.Fprintln(stderr, "oos:", err)
		return scan.ExitFatal
	}
	return exit
}

func newRootCommand(stdout io.Writer, now func() time.Time, exit *int) *cobra.Command {
	var o options
	cmd := &cobra.Command{
		Use:   "oos",
		Short: "Find what's running outside your Terraform",
		Long: `Out of State compares the AWS resources in an account with the Terraform
states you supply, and reports the ones that no state tracks.

Exit codes:
  0  complete scan, everything in scope is tracked
  1  error, no report produced
  2  complete scan, some resources have no match in the supplied states
  3  partial scan: a state source or region could not be read fully`,
		Version:       version.String(),
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			code, err := scanCommand(&o, stdout, now)
			*exit = code
			return err
		},
	}
	cmd.SetVersionTemplate("oos {{.Version}}\n")

	f := cmd.Flags()
	f.StringVar(&o.profile, "profile", "", "AWS profile for discovery and cost calls (default: the AWS SDK default chain)")
	f.StringArrayVar(&o.regions, "region", nil, "region to scan, repeatable (default: the region from your AWS config)")
	f.StringArrayVar(&o.sources, "source", nil, "initialised Terraform working directory, repeatable (default: . when no --state-file is given)")
	f.StringArrayVar(&o.stateFiles, "state-file", nil, "Terraform state JSON file, repeatable")
	f.StringVar(&o.tfBinary, "tf-binary", "terraform", "binary used for 'state pull' (for example tofu)")
	f.DurationVar(&o.sourceTimeout, "source-timeout", 2*time.Minute, "deadline for each Terraform subprocess")
	f.StringVar(&o.format, "format", "text", "report format: text or json")
	f.StringVar(&o.output, "output", "", "write the report to this file instead of stdout")
	f.BoolVar(&o.noCost, "no-cost", false, "skip Cost Explorer calls")
	f.BoolVar(&o.noColor, "no-color", false, "disable colour")
	f.StringVar(&o.inventoryFile, "inventory-file", "", "read the AWS inventory from a JSON file instead of AWS (testing)")
	_ = f.MarkHidden("inventory-file")
	return cmd
}

func scanCommand(o *options, stdout io.Writer, now func() time.Time) (int, error) {
	if o.format != "text" && o.format != "json" {
		return scan.ExitFatal, fmt.Errorf("--format must be text or json, not %q", o.format)
	}

	stateFiles := dedupePaths(o.stateFiles)
	sourceDirs := dedupePaths(o.sources)
	if len(stateFiles) == 0 && len(sourceDirs) == 0 {
		sourceDirs = []string{"."}
	}
	if len(sourceDirs) > 0 {
		return scan.ExitFatal, errors.New("reading Terraform working directories (--source) is not implemented yet; use --state-file")
	}

	if o.inventoryFile == "" {
		return scan.ExitFatal, errors.New("live AWS discovery is not implemented yet")
	}
	inv, err := discover.LoadFile(o.inventoryFile)
	if err != nil {
		return scan.ExitFatal, err
	}

	var sources []scan.LoadedSource
	for _, p := range stateFiles {
		sources = append(sources, scan.LoadStateFile(p, now))
	}

	rep := scan.Run(scan.Input{Inventory: inv, Sources: sources, Version: version.Version, Time: now()})
	if !o.noCost {
		rep.Cost = &report.CostBlock{Available: false, Reason: "not implemented yet"}
	}

	if err := write(o, stdout, rep); err != nil {
		return scan.ExitFatal, err
	}
	return scan.ExitCode(rep), nil
}

func write(o *options, stdout io.Writer, rep *report.Report) error {
	render := report.WriteText
	if o.format == "json" {
		render = report.WriteJSON
	}
	if o.output == "" {
		return render(stdout, rep)
	}
	f, err := os.Create(o.output)
	if err != nil {
		return fmt.Errorf("writing report: %w", err)
	}
	if err := render(f, rep); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing report: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing report: %w", err)
	}
	return nil
}

// dedupePaths removes repeats, comparing absolute paths, and keeps the
// first spelling the user gave.
func dedupePaths(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		key := p
		if abs, err := filepath.Abs(p); err == nil {
			key = abs
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, p)
	}
	return out
}
