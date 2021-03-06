// Copyright 2016 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may not
// use this file except in compliance with the License. A copy of the
// License is located at
//
// http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing
// permissions and limitations under the License.

// Package manager encapsulates everything related to long running plugin manager that starts, stops & configures long running plugins
package manager

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/ioutil"
	"sync"
	"time"

	"github.com/aws/amazon-ssm-agent/agent/appconfig"
	"github.com/aws/amazon-ssm-agent/agent/context"
	"github.com/aws/amazon-ssm-agent/agent/contracts"
	"github.com/aws/amazon-ssm-agent/agent/fileutil"
	"github.com/aws/amazon-ssm-agent/agent/jsonutil"
	"github.com/aws/amazon-ssm-agent/agent/log"
	managerContracts "github.com/aws/amazon-ssm-agent/agent/longrunning/plugin"
	"github.com/aws/amazon-ssm-agent/agent/longrunning/plugin/cloudwatch"
	"github.com/aws/amazon-ssm-agent/agent/platform"
	"github.com/aws/amazon-ssm-agent/agent/task"
	"github.com/aws/amazon-ssm-agent/agent/times"
	"github.com/carlescere/scheduler"
)

const (
	//name is the core module name for long running plugins manager
	Name = "LongRunningPluginsManager"

	// NameOfCloudWatchJsonFile is the name of ec2 config cloudwatch local configuration file
	NameOfCloudWatchJsonFile = "AWS.EC2.Windows.CloudWatch.json"

	//number of long running workers
	NumberOfLongRunningPluginWorkers = 5

	//number of cancel workers
	NumberOfCancelWorkers = 5

	//poll frequency for managing lifecycle of long running plugins
	PollFrequencyMinutes = 15

	//hardStopTimeout is the time before the manager will be shutdown during a hardstop = 4 seconds
	HardStopTimeout = 4 * time.Second

	//softStopTimeout is the time before the manager will be shutdown during a softstop = 20 seconds
	SoftStopTimeout = 20 * time.Second
)

// T manages long running plugins - get information of long running plugins and starts, stops & configures long running plugins
type T interface {
	contracts.ICoreModule
	GetRegisteredPlugins() map[string]managerContracts.Plugin
	StopPlugin(name string, cancelFlag task.CancelFlag) (err error)
	StartPlugin(name, configuration string, orchestrationDir string, cancelFlag task.CancelFlag) (err error)
	EnsurePluginRegistered(name string, plugin managerContracts.Plugin) (err error)
}

// Manager is the core module - that manages long running plugins
type Manager struct {
	context context.T

	//task pool to run long running plugins
	startPlugin task.Pool

	//task pool to stop long running plugins
	stopPlugin task.Pool

	//stores all writeable information about currently long running plugins
	runningPlugins map[string]managerContracts.PluginInfo

	//stores references of all the registered long running plugins
	registeredPlugins map[string]managerContracts.Plugin

	//manages lifecycle of all long running plugins
	managingLifeCycleJob *scheduler.Job
}

var singletonInstance *Manager
var once sync.Once

// EnsureManagerIsInitialized ensures that manager is initialized at least once
func EnsureInitialization(context context.T) {
	//todo: After we start using 1 task pool for entire agent (even for core modules), we can then move all initializations to init()

	//only components with access to context are expected to call this

	//this ensures that only one instance of lrpm exists
	once.Do(func() {
		managerContext := context.With("[" + Name + "]")
		log := managerContext.Log()
		//initialize pluginsInfo (which will store all information about long running plugins)
		plugins := map[string]managerContracts.PluginInfo{}
		//load all registered plugins
		regPlugins := RegisteredPlugins(context)
		jsonB, _ := json.Marshal(&regPlugins)
		log.Infof("registered plugins: %s", string(jsonB))

		// startPlugin and stopPlugin will be processed by separate worker pools
		// so we can define the number of workers for each pool
		cancelWaitDuration := 10000 * time.Millisecond
		clock := times.DefaultClock
		startPluginPool := task.NewPool(log, NumberOfLongRunningPluginWorkers, cancelWaitDuration, clock)
		stopPluginPool := task.NewPool(log, NumberOfCancelWorkers, cancelWaitDuration, clock)

		singletonInstance = &Manager{
			context:           managerContext,
			startPlugin:       startPluginPool,
			stopPlugin:        stopPluginPool,
			runningPlugins:    plugins,
			registeredPlugins: regPlugins,
		}
	})

}

// GetInstance returns an instance of Manager if its initialized otherwise it returns an error
func GetInstance() (*Manager, error) {
	lock.Lock()
	defer lock.Unlock()

	if singletonInstance == nil {
		return nil, errors.New("lrpm isn't initialized yet")
	} else {
		return singletonInstance, nil
	}
}

// GetRegisteredPlugins returns a map of all registered long running plugins
func (m *Manager) GetRegisteredPlugins() map[string]managerContracts.Plugin {
	return m.registeredPlugins
}

// Name returns the module name
func (m *Manager) ModuleName() string {
	return Name
}

// Execute starts long running plugin manager
func (m *Manager) ModuleExecute(context context.T) (err error) {

	log := m.context.Log()
	log.Infof("starting long running plugin manager")
	//read from data store to determine if there were any previously long running plugins which need to be started again
	var dataStoreMap map[string]managerContracts.PluginInfo
	dataStoreMap, err = dataStore.Read()
	if len(dataStoreMap) != 0 {
		m.runningPlugins = dataStoreMap
	}

	if err != nil {
		log.Errorf("%s is exiting - unable to read from data store", m.ModuleName())
		return
	}

	//revive older long running plugins if they were running before
	if len(m.runningPlugins) > 0 {
		for pluginName, pluginInfo := range m.runningPlugins {
			//get the corresponding registered plugin
			p, exists := m.registeredPlugins[pluginName]
			if !exists {
				//remove previously running plugins with no registered handlers
				delete(m.runningPlugins, pluginName)
				continue
			}
			p.Info = pluginInfo
			log.Infof("Detected %s as a previously executing long running plugin. Starting that plugin again", p.Info.Name)
			//submit the work of long running plugin to the task pool
			/*
				Note: All long running plugins are singleton in nature - hence jobId = plugin name.
				This is in sync with our task-pool - which rejects jobs with duplicate jobIds.
			*/
			//todo: implement the singleton thing - ensure that there are no more than 1 cloudwatch plugin running at a time
			//todo: orchestrationDir should be set accordingly - 3rd parameter for Start
			p.Handler.Start(m.context, p.Info.Configuration, "", task.NewChanneledCancelFlag())
			m.registeredPlugins[pluginName] = p
		}
	} else {
		log.Infof("there aren't any long running plugin to execute")
	}

	if isPlatformSupported(context.Log(), appconfig.PluginNameCloudWatch) {
		m.configCloudWatch(log)
	}

	//schedule periodic health check of all long running plugins
	if m.managingLifeCycleJob, err = scheduler.Every(PollFrequencyMinutes).Minutes().Run(m.ensurePluginsAreRunning); err != nil {
		context.Log().Errorf("unable to schedule long running plugins manager. %v", err)
	}

	return
}

// RequestStop handles the termination of the long running plugin manager
func (m *Manager) ModuleRequestStop(stopType contracts.StopType) (err error) {
	var waitTimeout time.Duration

	if stopType == contracts.StopTypeSoftStop {
		waitTimeout = SoftStopTimeout
	} else {
		waitTimeout = HardStopTimeout
	}

	var wg sync.WaitGroup

	// stop lifecycle management job that monitors execution of all long running plugins
	m.stopLifeCycleManagementJob()

	//there is no need to stop all individual plugins - because when the task pools are shutdown - all corresponding
	//jobs are also shutdown accordingly.

	// shutdown the send command pool in a separate go routine
	wg.Add(1)
	go func() {
		defer wg.Done()
		m.startPlugin.ShutdownAndWait(waitTimeout)
	}()

	// shutdown the cancel command pool in a separate go routine
	wg.Add(1)
	go func() {
		defer wg.Done()
		m.stopPlugin.ShutdownAndWait(waitTimeout)
	}()

	if len(m.runningPlugins) > 0 {
		m.stopLongRunningPlugins(stopType)
	}

	// wait for everything to shutdown
	wg.Wait()
	return nil
}

// stopLongRunningPlugins requests the long running plugins to stop
func (m *Manager) stopLongRunningPlugins(stopType contracts.StopType) {
	log := m.context.Log()
	log.Infof("long running manager stop requested. Stop type: %v", stopType)

	var wg sync.WaitGroup
	i := 0
	for pluginName, _ := range m.runningPlugins {
		go func(wgc *sync.WaitGroup, i int) {
			if stopType == contracts.StopTypeSoftStop {
				wgc.Add(1)
				defer wgc.Done()
			}

			plugin := m.registeredPlugins[pluginName]
			if err := plugin.Handler.Stop(m.context, task.NewChanneledCancelFlag()); err != nil {
				log.Errorf("Plugin (%v) failed to stop with error: %v",
					pluginName,
					err)
			}

		}(&wg, i)
		i++
	}
}

// EnsurePluginRegistered adds a long-running plugin if it is not already in the registry
func (m *Manager) EnsurePluginRegistered(name string, plugin managerContracts.Plugin) (err error) {
	if _, exists := m.registeredPlugins[name]; !exists {
		m.registeredPlugins[name] = plugin
	}
	return nil
}

// configCloudWatch checks the local configuration file for cloud watch plugin to see if any updates to config
func (m *Manager) configCloudWatch(log log.T) {

	var err error
	cloudwatch.Initialze()

	var instanceId string
	if instanceId, err = platform.InstanceID(); err != nil {
		log.Errorf("Cannot get instance id.")
		return
	}

	// Read from cloudwatch config file to check if any configuration need to make for cloud watch
	if err = cloudwatch.Update(log); err != nil {
		log.Debugf("There's no local configuration set for cloudwatch plugin. %v", err)

		// We also need to check if any configuration has been made by ec2 config before
		var hasConfiguration bool
		var localConfig bool
		if hasConfiguration, err = checkLegacyCloudWatchRunCommandConfig(instanceId); err != nil {
			log.Debugf("Have problem read configuration from ec2config file. %v", err)
			return
		}

		if !hasConfiguration {
			if localConfig, err = checkLegacyCloudWatchLocalConfig(); err != nil {
				log.Debugf("Have problem read configuration from ec2config file. %v", err)
				return
			}
		}

		if !hasConfiguration && !localConfig {
			log.Debug("There is no cloudwatch running in ec2 config service before.")
			return
		}
	}

	cloudWatchConfig := cloudwatch.Instance()
	if cloudWatchConfig.IsEnabled {
		log.Infof("Detected cloud watch has updated configuration. Configuring that plugin again")
		// TODO need to check the folder
		orchestrationDir := fileutil.BuildPath(
			appconfig.DefaultDataStorePath,
			instanceId,
			appconfig.DefaultDocumentRootDirName,
			appconfig.PluginNameCloudWatch)
		var config string
		if config, err = cloudwatch.ParseEngineConfiguration(); err != nil {
			log.Debug("Cannot parse EngineConfiguration to string format")
		}

		if err = m.StartPlugin(
			appconfig.PluginNameCloudWatch,
			config,
			orchestrationDir,
			task.NewChanneledCancelFlag()); err != nil {
			log.Errorf("Failed to start the cloud watch plugin bacause: %s", err)
		}

		// check if configure cloudwatch successfully
		stderrFilePath := fileutil.BuildPath(orchestrationDir, appconfig.PluginNameCloudWatch, "stderr")
		var errData []byte
		var errorReadingFile error
		if errData, errorReadingFile = ioutil.ReadFile(stderrFilePath); errorReadingFile != nil {
			log.Errorf("Unable to read the stderr file - %s: %s", stderrFilePath, errorReadingFile.Error())
		}
		serr := string(errData)

		if len(serr) > 0 {
			log.Errorf("Unable to start the plugin - %s: %s", appconfig.PluginNameCloudWatch, serr)
			// Stop the plugin if configuration failed.
			if err := m.StopPlugin(appconfig.PluginNameCloudWatch, task.NewChanneledCancelFlag()); err != nil {
				log.Errorf("Unable to start the plugin - %s: %s", appconfig.PluginNameCloudWatch, err.Error())
			}
		}

	} else {
		log.Infof("Detected cloud watch has been requested to stop. Stoping the plugin")
		if err = m.StopPlugin(appconfig.PluginNameCloudWatch, task.NewChanneledCancelFlag()); err != nil {
			log.Errorf("Failed to stop the cloud watch plugin bacause: %s", err)
		}
	}
}

// checkLegacyCloudWatchRunCommandConfig checks if ec2config has cloudwatch configuration document running before
func checkLegacyCloudWatchRunCommandConfig(instanceId string) (hasConfiguration bool, err error) {
	hasConfiguration = false
	// check if configured cloudwatch before with runcommand
	storeFileName := fileutil.BuildPath(
		appconfig.EC2ConfigDataStorePath,
		instanceId,
		appconfig.ConfigurationRootDirName,
		appconfig.WorkersRootDirName,
		"aws.cloudWatch.ec2config")

	if fileutil.Exists(storeFileName) {
		lock.RLock()
		defer lock.RUnlock()
		var documentModel contracts.DocumentContent
		if err = jsonutil.UnmarshalFile(storeFileName, &documentModel); err != nil {
			//log.Infof("Cannot read configuration from ec2 configuration service file. %v", err)
			return
		}
		pluginConfig := documentModel.RuntimeConfig[appconfig.PluginNameCloudWatch]
		cloudWatchConfig := &cloudwatch.CloudWatchConfig{
			EngineConfiguration: pluginConfig.Properties,
			IsEnabled:           true,
		}
		if err = cloudwatch.Enable(cloudWatchConfig); err != nil {
			return
		}
		hasConfiguration = true
	}
	return
}

// checkLegacyCloudWatchLocalConfig checks if users have cloudwatch local configuration before.
func checkLegacyCloudWatchLocalConfig() (hasConfiguration bool, err error) {
	hasConfiguration = false
	// first check the config.xml file to see if the cloudwatch plugin is enabled
	var isEnabled bool
	isEnabled, err = cloudwatch.ParseXml()
	if err != nil || !isEnabled {
		return
	}

	configFileName := fileutil.BuildPath(
		appconfig.EC2ConfigSettingPath,
		NameOfCloudWatchJsonFile)

	var content []byte
	content, err = ioutil.ReadFile(configFileName)

	validContent := checkAndRemoveBomCharacters(content)

	// Update the config file with new configuration
	var engineConfiguration interface{}
	json.Unmarshal([]byte(validContent), &engineConfiguration)

	cloudWatchConfig := &cloudwatch.CloudWatchConfig{
		EngineConfiguration: engineConfiguration,
		IsEnabled:           true,
	}

	if err = cloudwatch.Enable(cloudWatchConfig); err != nil {
		return
	}
	return true, nil
}

// checkAndRemoveBomCharacters checks if there is any invalid bom characters at the beginning of the bytes.
// If found, remove them to avoid unmarshall problem.
func checkAndRemoveBomCharacters(content []byte) []byte {
	bom := []byte{0xef, 0xbb, 0xbf} // UTF-8
	// if byte-order mark found
	if bytes.Equal(content[:3], bom) {
		content = content[3:]
	}
	return content
}
