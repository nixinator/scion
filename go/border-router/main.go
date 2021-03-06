// Copyright 2020 Anapaya Systems
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"fmt"
	"net/http"
	"os"
	"sync"

	"github.com/spf13/cobra"

	"github.com/scionproto/scion/go/border/brconf"
	libconfig "github.com/scionproto/scion/go/lib/config"
	"github.com/scionproto/scion/go/lib/env"
	"github.com/scionproto/scion/go/lib/fatal"
	"github.com/scionproto/scion/go/lib/log"
	"github.com/scionproto/scion/go/lib/prom"
	"github.com/scionproto/scion/go/lib/serrors"
	"github.com/scionproto/scion/go/pkg/command"
	"github.com/scionproto/scion/go/pkg/router"
	"github.com/scionproto/scion/go/pkg/router/control"
	"github.com/scionproto/scion/go/pkg/service"
)

func main() {
	var flags struct {
		config string
	}
	cmd := &cobra.Command{
		Use:           "border-router",
		Short:         "SCION border router",
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(flags.config)
		},
	}
	cmd.AddCommand(
		command.NewCompletion(cmd),
		command.NewSample(cmd, command.NewSampleConfig(&brconf.Config{})),
		command.NewVersion(cmd),
	)
	cmd.Flags().StringVar(&flags.config, "config", "", "Configuration file (required)")
	cmd.MarkFlagRequired("config")
	if err := cmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}

func run(file string) error {
	fatal.Init()
	cfg, err := setupBasic(file)
	if err != nil {
		return err
	}
	defer log.Flush()
	defer env.LogAppStopped("BR", cfg.General.ID)
	defer log.HandlePanic()
	if err := validateCfg(cfg); err != nil {
		return err
	}
	brConf, err := loadBRConf(cfg)
	if err != nil {
		return err
	}
	stop := make(chan struct{})
	wg := new(sync.WaitGroup)
	dp := &router.Connector{
		DataPlane: router.DataPlane{},
	}
	iaCtx := &control.IACtx{
		BRConf:                   brConf,
		DP:                       dp,
		Stop:                     stop,
		DisableLegacyIfStateMgmt: cfg.Features.HeaderV2,
	}
	if err := iaCtx.Start(wg, cfg.General.ReconnectToDispatcher); err != nil {
		return serrors.WrapStr("starting dataplane", err)
	}
	if err := setupHTTPHandlers(cfg); err != nil {
		return serrors.WrapStr("starting HTTP endpoints", err)
	}
	if err := dp.DataPlane.Run(); err != nil {
		return serrors.WrapStr("starting dataplane", err)
	}

	// XXX(lukedirtwalker): Currently not reachable because the dataplan run is
	// blocking.
	select {
	case <-fatal.ShutdownChan():
		// Whenever we receive a SIGINT or SIGTERM we exit without an error.
		// Deferred shutdowns for all running servers run now.
		close(stop)
		wg.Wait()
		return nil
	case <-fatal.FatalChan():
		return serrors.New("shutdown on error")
	}
}

func setupBasic(file string) (brconf.Config, error) {
	var cfg brconf.Config
	if err := libconfig.LoadFile(file, &cfg); err != nil {
		return brconf.Config{}, serrors.WrapStr("loading config from file", err, "file", file)
	}
	cfg.InitDefaults()
	if err := log.Setup(cfg.Logging); err != nil {
		return brconf.Config{}, serrors.WrapStr("initialize logging", err)
	}
	prom.ExportElementID(cfg.General.ID)
	if err := env.LogAppStarted("BR", cfg.General.ID); err != nil {
		return brconf.Config{}, err
	}
	return cfg, nil
}

func validateCfg(cfg brconf.Config) error {
	if err := cfg.Validate(); err != nil {
		return serrors.WrapStr("validating config", err)
	}
	if !cfg.Features.HeaderV2 {
		return serrors.New("border-router only works for new header format")
	}
	return nil
}

func loadBRConf(cfg brconf.Config) (*brconf.BRConf, error) {
	newConf, err := brconf.Load(cfg.General.ID, cfg.General.ConfigDir)
	if err != nil {
		return nil, serrors.WrapStr("loading topology", err)
	}
	return newConf, nil
}

func setupHTTPHandlers(cfg brconf.Config) error {
	statusPages := service.StatusPages{
		"info":      service.NewInfoHandler(),
		"config":    service.NewConfigHandler(cfg),
		"log/level": log.ConsoleLevel.ServeHTTP,
		// TODO: Add topology page
	}
	if err := statusPages.Register(http.DefaultServeMux, cfg.General.ID); err != nil {
		return err
	}
	cfg.Metrics.StartPrometheus()
	return nil
}
