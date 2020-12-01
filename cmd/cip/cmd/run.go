/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cmd

import (
	"fmt"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"

	"sigs.k8s.io/k8s-container-image-promoter/legacy/cli"
)

// runCmd represents the base command when called without any subcommands
// TODO: Update command description.
var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Promote images from a staging registry to production",
	Long: `cip - Kubernetes container image promoter

Promote images from a staging registry to production
`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return errors.Wrap(
			cli.RunPromoteCmd(runOpts),
			"run `cip run`",
		)
	},
}

var runOpts = &cli.RunOptions{}

// TODO: Function 'init' is too long (171 > 60) (funlen)
// nolint: funlen
func init() {
	runCmd.PersistentFlags().BoolVar(
		&runOpts.NoDryRun,
		"no-dry-run",
		runOpts.NoDryRun,
		"initiate a PRODUCTION image promotion",
	)

	// TODO: Move this into a default options function in pkg/promobot
	runCmd.PersistentFlags().StringVar(
		&runOpts.Manifest,
		cli.PromoterManifestFlag,
		runOpts.Manifest,
		"the manifest file to load",
	)

	runCmd.PersistentFlags().StringVar(
		&runOpts.ThinManifestDir,
		cli.PromoterThinManifestDirFlag,
		runOpts.ThinManifestDir,
		`recursively read in all manifests within a folder, but all manifests
MUST be 'thin' manifests named 'promoter-manifest.yaml', which are like regular
manifests but instead of defining the 'images: ...' field directly, the
'imagesPath' field must be defined that points to another YAML file containing
the 'images: ...' contents`,
	)

	runCmd.PersistentFlags().IntVar(
		&runOpts.Threads,
		"threads",
		cli.PromoterDefaultThreads,
		"number of concurrent goroutines to use when talking to GCR",
	)

	runCmd.PersistentFlags().BoolVar(
		&runOpts.JSONLogSummary,
		"json-log-summary",
		runOpts.JSONLogSummary,
		"only log a JSON summary of important errors",
	)

	runCmd.PersistentFlags().BoolVar(
		&runOpts.ParseOnly,
		"parse-only",
		runOpts.ParseOnly,
		"only check that the given manifest file is parsable as a Manifest",
	)

	runCmd.PersistentFlags().StringVar(
		&runOpts.KeyFiles,
		"key-files",
		runOpts.KeyFiles,
		`CSV of service account key files that must be activated for the
promotion (<json-key-file-path>,...)`,
	)

	runCmd.PersistentFlags().StringVar(
		&runOpts.Snapshot,
		cli.PromoterSnapshotFlag,
		runOpts.Snapshot,
		"read all images in a repository and print to stdout",
	)

	runCmd.PersistentFlags().StringVar(
		&runOpts.SnapshotTag,
		"snapshot-tag",
		runOpts.SnapshotTag,
		"only snapshot images with the given tag",
	)

	runCmd.PersistentFlags().BoolVar(
		&runOpts.MinimalSnapshot,
		"minimal-snapshot",
		runOpts.MinimalSnapshot,
		fmt.Sprintf(`(only works with '--%s' or '--%s') discard tagless images
from snapshot output if they are referenced by a manifest list`,
			cli.PromoterSnapshotFlag,
			cli.PromoterManifestBasedSnapshotOfFlag,
		),
	)

	runCmd.PersistentFlags().StringVar(
		&runOpts.OutputFormat,
		cli.PromoterOutputFlag,
		cli.PromoterDefaultOutputFormat,
		fmt.Sprintf(`(only works with '--%s' or '--%s') choose output
format of the snapshot (allowed values: %q)`,
			cli.PromoterSnapshotFlag,
			cli.PromoterManifestBasedSnapshotOfFlag,
			cli.PromoterAllowedOutputFormats,
		),
	)

	runCmd.PersistentFlags().StringVar(
		&runOpts.SnapshotSvcAcct,
		"snapshot-service-account",
		runOpts.SnapshotSvcAcct,
		fmt.Sprintf(
			"service account to use for '--%s'",
			cli.PromoterSnapshotFlag,
		),
	)

	runCmd.PersistentFlags().StringVar(
		&runOpts.ManifestBasedSnapshotOf,
		cli.PromoterManifestBasedSnapshotOfFlag,
		runOpts.ManifestBasedSnapshotOf,
		fmt.Sprintf(`read all images in either '--%s' or '--%s' and print all
images that should be promoted to the given registry (assuming the given,
registry is empty); this is like '--%s', but instead of reading over the
network from a registry, it reads from the local manifests only`,
			cli.PromoterManifestFlag,
			cli.PromoterThinManifestDirFlag,
			cli.PromoterSnapshotFlag,
		),
	)

	runCmd.PersistentFlags().BoolVar(
		&runOpts.UseServiceAcct,
		"use-service-account",
		runOpts.UseServiceAcct,
		"pass '--account=...' to all gcloud calls",
	)

	runCmd.PersistentFlags().IntVar(
		&runOpts.MaxImageSize,
		"max-image-size",
		cli.PromoterDefaultMaxImageSize,
		"the maximum image size (in MiB) allowed for promotion",
	)

	// TODO: Set this in a function instead
	if runOpts.MaxImageSize <= 0 {
		runOpts.MaxImageSize = 2048
	}

	runCmd.PersistentFlags().IntVar(
		&runOpts.SeverityThreshold,
		"vuln-severity-threshold",
		cli.PromoterDefaultSeverityThreshold,
		`Using this flag will cause the promoter to only run the vulnerability
check. Found vulnerabilities at or above this threshold will result in the
vulnerability check failing [severity levels between 0 and 5; 0 - UNSPECIFIED,
1 - MINIMAL, 2 - LOW, 3 - MEDIUM, 4 - HIGH, 5 - CRITICAL]`,
	)

	rootCmd.AddCommand(runCmd)
}
