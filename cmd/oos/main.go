// Command oos finds AWS resources that are not tracked in Terraform state.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/gordonmurray/outofstate.dev/internal/version"
)

// Exit codes, see the "Exit codes" section of the README.
const (
	exitOK    = 0
	exitFatal = 1
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	cmd := newRootCommand()
	cmd.SetArgs(args)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	if err := cmd.Execute(); err != nil {
		return exitFatal
	}
	return exitOK
}

func newRootCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "oos",
		Short:         "Find what's running outside your Terraform",
		Long:          "Out of State compares the AWS resources in an account with the Terraform states you supply,\nand reports the ones that no state tracks.",
		Version:       version.String(),
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.ErrOrStderr(), "oos: scanning is not implemented yet (milestone M0 stub)")
			if err != nil {
				return err
			}
			return fmt.Errorf("not implemented")
		},
	}
	cmd.SetVersionTemplate("oos {{.Version}}\n")
	return cmd
}
