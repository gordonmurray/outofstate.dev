// Command oos finds AWS resources that are not tracked in Terraform state.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/gordonmurray/outofstate.dev/internal/cost"
	"github.com/gordonmurray/outofstate.dev/internal/discover"
	"github.com/gordonmurray/outofstate.dev/internal/report"
	"github.com/gordonmurray/outofstate.dev/internal/scan"
	"github.com/gordonmurray/outofstate.dev/internal/tfrun"
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
	cost          bool
	noCost        bool // deprecated: cost is off unless --cost is given
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
	f.BoolVar(&o.cost, "cost", false, "also report RDS spend from Cost Explorer (slower; each request costs US$0.01, a run makes two)")
	f.BoolVar(&o.noCost, "no-cost", false, "no effect: cost is off unless --cost is given")
	_ = f.MarkHidden("no-cost")
	f.BoolVar(&o.noColor, "no-color", false, "disable colour")
	f.StringVar(&o.inventoryFile, "inventory-file", "", "read the AWS inventory from a JSON file instead of AWS (testing)")
	_ = f.MarkHidden("inventory-file")
	return cmd
}

// sourceConcurrency bounds how many Terraform subprocesses run at once.
const sourceConcurrency = 4

func scanCommand(o *options, stdout io.Writer, now func() time.Time) (int, error) {
	if o.format != "text" && o.format != "json" {
		return scan.ExitFatal, fmt.Errorf("--format must be text or json, not %q", o.format)
	}

	stateFiles := dedupePaths(o.stateFiles)
	sourceDirs := dedupePaths(o.sources)
	if len(stateFiles) == 0 && len(sourceDirs) == 0 {
		sourceDirs = []string{"."}
	}

	// Ctrl-C cancels AWS calls and kills Terraform subprocesses.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Discovery and state loading run at the same time.
	var (
		inv     *discover.Inventory
		awsAPI  *discover.AWS
		discErr error
		sources []scan.LoadedSource
		wg      sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		inv, awsAPI, discErr = discoverInventory(ctx, o)
	}()
	go func() {
		defer wg.Done()
		opts := tfrun.Options{Binary: o.tfBinary, Timeout: o.sourceTimeout}
		sources = scan.LoadDirectories(ctx, sourceDirs, opts, sourceConcurrency, now)
		for _, p := range stateFiles {
			sources = append(sources, scan.LoadStateFile(p, now))
		}
	}()
	wg.Wait()
	if discErr != nil {
		return scan.ExitFatal, discErr
	}
	if ctx.Err() != nil {
		return scan.ExitFatal, errors.New("interrupted")
	}

	// Cost needs the account ID, so it runs after discovery. It is optional:
	// any failure is reported in the cost block, never as a scan failure.
	var costs *cost.Result
	if o.cost && !o.noCost && awsAPI != nil {
		costs = cost.Fetch(ctx, cost.NewClient(awsAPI.Config()), inv.Partition, inv.AccountID, inv.RegionNames(), now())
		if ctx.Err() != nil {
			// Ctrl-C during the cost calls is an interruption, not a cost error.
			return scan.ExitFatal, errors.New("interrupted")
		}
	}

	rep := scan.Run(scan.Input{Inventory: inv, Sources: sources, Cost: costs, Version: version.Version, Time: now()})

	if err := write(o, stdout, rep); err != nil {
		return scan.ExitFatal, err
	}
	return scan.ExitCode(rep), nil
}

// discoverInventory returns the inventory, and the AWS client when live
// discovery was used (nil for --inventory-file, which also means no cost).
func discoverInventory(ctx context.Context, o *options) (*discover.Inventory, *discover.AWS, error) {
	if o.inventoryFile != "" {
		inv, err := discover.LoadFile(o.inventoryFile)
		return inv, nil, err
	}
	a, err := discover.NewAWS(ctx, o.profile)
	if err != nil {
		return nil, nil, err
	}
	regions := dedupe(o.regions)
	if len(regions) == 0 && a.DefaultRegion() != "" {
		regions = []string{a.DefaultRegion()}
	}
	inv, err := a.Discover(ctx, regions)
	return inv, a, err
}

func write(o *options, stdout io.Writer, rep *report.Report) error {
	render := report.WriteText
	if o.format == "json" {
		render = report.WriteJSON
	}
	if o.output == "" {
		if o.format == "text" && useColour(o, stdout) {
			return report.WriteTextWith(stdout, rep, report.TextOptions{Color: true})
		}
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

// useColour is true only for text written to a terminal, unless NO_COLOR is
// set (https://no-color.org) or --no-color is given.
func useColour(o *options, w io.Writer) bool {
	if o.noColor || os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// dedupe removes repeated values, keeping the first occurrence.
func dedupe(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range values {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
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
