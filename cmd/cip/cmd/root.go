/*
Copyright 2020 The Kubernetes Authors.

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

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"sigs.k8s.io/k8s-container-image-promoter/legacy/cli"
	"sigs.k8s.io/release-utils/log"
)

// rootCmd represents the base command when called without any subcommands.
var rootCmd = &cobra.Command{
	Use:   "cip",
	Short: "Promote images from a staging registry to production",
	Long: `cip - Kubernetes container image promoter
`,
	PersistentPreRunE: initLogging,
}

var rootOpts = &cli.RootOptions{}

// Execute adds all child commands to the root command and sets flags.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		logrus.Fatal(err)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVar(
		&rootOpts.LogLevel,
		"log-level",
		"info",
		fmt.Sprintf("the logging verbosity, either %s", log.LevelNames()),
	)
}

func initLogging(*cobra.Command, []string) error {
	return log.SetupGlobalLogger(rootOpts.LogLevel)
}
