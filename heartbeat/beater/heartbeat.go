// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package beater

import (
	"errors"
	"fmt"
	"sync"
	"syscall"
	"time"

	"github.com/elastic/beats/v7/heartbeat/config"
	"github.com/elastic/beats/v7/heartbeat/hbregistry"
	"github.com/elastic/beats/v7/heartbeat/monitors"
	"github.com/elastic/beats/v7/heartbeat/monitors/plugin"
	"github.com/elastic/beats/v7/heartbeat/monitors/stdfields"
	"github.com/elastic/beats/v7/heartbeat/scheduler"
	"github.com/elastic/beats/v7/libbeat/autodiscover"
	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/beats/v7/libbeat/cfgfile"
	"github.com/elastic/beats/v7/libbeat/common"
	"github.com/elastic/beats/v7/libbeat/common/reload"
	"github.com/elastic/beats/v7/libbeat/logp"
	"github.com/elastic/beats/v7/libbeat/management"
	"github.com/elastic/beats/v7/x-pack/functionbeat/function/core"

	_ "github.com/elastic/beats/v7/libbeat/processors/script"
)

// Heartbeat represents the root datastructure of this beat.
type Heartbeat struct {
	done chan struct{}
	// config is used for iterating over elements of the config.
	config          config.Config
	scheduler       *scheduler.Scheduler
	monitorReloader *cfgfile.Reloader
	dynamicFactory  *monitors.RunnerFactory
	autodiscover    *autodiscover.Autodiscover
}

// New creates a new heartbeat.
func New(b *beat.Beat, rawConfig *common.Config) (beat.Beater, error) {
	parsedConfig := config.DefaultConfig
	if err := rawConfig.Unpack(&parsedConfig); err != nil {
		return nil, fmt.Errorf("Error reading config file: %v", err)
	}
	limit := parsedConfig.Scheduler.Limit
	locationName := parsedConfig.Scheduler.Location
	if locationName == "" {
		locationName = "Local"
	}
	location, err := time.LoadLocation(locationName)
	if err != nil {
		return nil, err
	}
	jobConfig := parsedConfig.Jobs

	scheduler := scheduler.NewWithLocation(limit, hbregistry.SchedulerRegistry, location, jobConfig)

	bt := &Heartbeat{
		done:      make(chan struct{}),
		config:    parsedConfig,
		scheduler: scheduler,
		// dynamicFactory is the factory used for dynamic configs, e.g. autodiscover / reload
		dynamicFactory: monitors.NewFactory(b.Info, scheduler),
	}
	return bt, nil
}

// Run executes the beat.
func (bt *Heartbeat) Run(b *beat.Beat) error {
	logp.Info("heartbeat is running! Hit CTRL-C to stop it.")
	groups, _ := syscall.Getgroups()
	logp.Info("Effective user/group ids: %d/%d, with groups: %v", syscall.Geteuid(), syscall.Getegid(), groups)

	if bt.config.RunOnce != nil {
		err := bt.runRunOnce(b)
		if err != nil {
			return err
		}
		return nil
	}

	stopStaticMonitors, err := bt.RunStaticMonitors(b)
	if err != nil {
		return err
	}
	defer stopStaticMonitors()

	if b.Manager.Enabled() {
		bt.RunCentralMgmtMonitors(b)
	}

	if bt.config.ConfigMonitors.Enabled() {
		bt.monitorReloader = cfgfile.NewReloader(b.Publisher, bt.config.ConfigMonitors)
		defer bt.monitorReloader.Stop()

		err := bt.RunReloadableMonitors(b)
		if err != nil {
			return err
		}
	}

	if bt.config.Autodiscover != nil {
		bt.autodiscover, err = bt.makeAutodiscover(b)
		if err != nil {
			return err
		}

		bt.autodiscover.Start()
		defer bt.autodiscover.Stop()
	}

	if err := bt.scheduler.Start(); err != nil {
		return err
	}
	defer bt.scheduler.Stop()

	<-bt.done

	logp.Info("Shutting down.")
	return nil
}

// runRunOnce runs the given config then exits immediately after any queued events have been sent to ES
func (bt *Heartbeat) runRunOnce(b *beat.Beat) error {
	logp.Info("Starting run_once run. This is an experimental feature and may be changed or removed in the future!")
	cfgs := bt.config.RunOnce

	publishClient, err := core.NewSyncClient(logp.NewLogger("run_once mode"), b.Publisher, beat.ClientConfig{})
	if err != nil {
		return fmt.Errorf("could not create sync client: %w", err)
	}
	defer publishClient.Close()

	wg := &sync.WaitGroup{}
	for _, cfg := range cfgs {
		err := runRunOnceSingleConfig(cfg, publishClient, wg)
		if err != nil {
			logp.Warn("error running run_once config: %s", err)
		}
	}

	wg.Wait()
	publishClient.Wait()

	logp.Info("Ending run_once run")

	return nil
}

func runRunOnceSingleConfig(cfg *common.Config, publishClient *core.SyncClient, wg *sync.WaitGroup) (err error) {
	sf, err := stdfields.ConfigToStdMonitorFields(cfg)
	if err != nil {
		return fmt.Errorf("could not get stdmon fields: %w", err)
	}
	pluginFactory, exists := plugin.GlobalPluginsReg.Get(sf.Type)
	if !exists {
		return fmt.Errorf("no plugin for type: %s", sf.Type)
	}
	plugin, err := pluginFactory.Make(sf.Type, cfg)
	if err != nil {
		return err
	}

	results := plugin.RunWrapped(sf)

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer plugin.Close()
		for {
			event := <-results
			if event == nil {
				break
			}
			publishClient.Publish(*event)
		}
	}()

	return nil
}

// RunStaticMonitors runs the `heartbeat.monitors` portion of the yaml config if present.
func (bt *Heartbeat) RunStaticMonitors(b *beat.Beat) (stop func(), err error) {
	factory := monitors.NewFactory(b.Info, bt.scheduler)

	var runners []cfgfile.Runner
	for _, cfg := range bt.config.Monitors {
		created, err := factory.Create(b.Publisher, cfg)
		if err != nil {
			if errors.Is(err, monitors.ErrMonitorDisabled) {
				logp.Info("skipping disabled monitor: %s", err)
				continue // don't stop loading monitors just because they're disabled
			}

			return nil, fmt.Errorf("could not create monitor: %w", err)
		}

		created.Start()
		runners = append(runners, created)
	}

	stop = func() {
		for _, runner := range runners {
			runner.Stop()
		}
	}
	return stop, nil
}

// RunCentralMgmtMonitors loads any central management configured configs.
func (bt *Heartbeat) RunCentralMgmtMonitors(b *beat.Beat) {
	monitors := cfgfile.NewRunnerList(management.DebugK, bt.dynamicFactory, b.Publisher)
	reload.Register.MustRegisterList(b.Info.Beat+".monitors", monitors)
	inputs := cfgfile.NewRunnerList(management.DebugK, bt.dynamicFactory, b.Publisher)
	reload.Register.MustRegisterList("inputs", inputs)
}

// RunReloadableMonitors runs the `heartbeat.config.monitors` portion of the yaml config if present.
func (bt *Heartbeat) RunReloadableMonitors(b *beat.Beat) (err error) {
	// Check monitor configs
	if err := bt.monitorReloader.Check(bt.dynamicFactory); err != nil {
		logp.Error(fmt.Errorf("error loading reloadable monitors: %w", err))
	}

	// Execute the monitor
	go bt.monitorReloader.Run(bt.dynamicFactory)

	return nil
}

// makeAutodiscover creates an autodiscover object ready to be started.
func (bt *Heartbeat) makeAutodiscover(b *beat.Beat) (*autodiscover.Autodiscover, error) {
	autodiscover, err := autodiscover.NewAutodiscover(
		"heartbeat",
		b.Publisher,
		bt.dynamicFactory,
		autodiscover.QueryConfig(),
		bt.config.Autodiscover,
		b.Keystore,
	)
	if err != nil {
		return nil, err
	}
	return autodiscover, nil
}

// Stop stops the beat.
func (bt *Heartbeat) Stop() {
	close(bt.done)
}
