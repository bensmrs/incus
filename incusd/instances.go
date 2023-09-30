package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/lxc/incus/internal/server/db"
	"github.com/lxc/incus/internal/server/db/cluster"
	"github.com/lxc/incus/internal/server/db/warningtype"
	"github.com/lxc/incus/internal/server/instance"
	"github.com/lxc/incus/internal/server/instance/instancetype"
	"github.com/lxc/incus/internal/server/project"
	"github.com/lxc/incus/internal/server/state"
	"github.com/lxc/incus/internal/server/warnings"
	internalUtil "github.com/lxc/incus/internal/util"
	"github.com/lxc/incus/shared/api"
	"github.com/lxc/incus/shared/logger"
	"github.com/lxc/incus/shared/util"
)

var instancesCmd = APIEndpoint{
	Name: "instances",
	Path: "instances",

	Get:  APIEndpointAction{Handler: instancesGet, AccessHandler: allowProjectPermission()},
	Post: APIEndpointAction{Handler: instancesPost, AccessHandler: allowProjectPermission()},
	Put:  APIEndpointAction{Handler: instancesPut, AccessHandler: allowProjectPermission()},
}

var instanceCmd = APIEndpoint{
	Name: "instance",
	Path: "instances/{name}",

	Get:    APIEndpointAction{Handler: instanceGet, AccessHandler: allowProjectPermission()},
	Put:    APIEndpointAction{Handler: instancePut, AccessHandler: allowProjectPermission()},
	Delete: APIEndpointAction{Handler: instanceDelete, AccessHandler: allowProjectPermission()},
	Post:   APIEndpointAction{Handler: instancePost, AccessHandler: allowProjectPermission()},
	Patch:  APIEndpointAction{Handler: instancePatch, AccessHandler: allowProjectPermission()},
}

var instanceRebuildCmd = APIEndpoint{
	Name: "instanceRebuild",
	Path: "instances/{name}/rebuild",

	Post: APIEndpointAction{Handler: instanceRebuildPost, AccessHandler: allowProjectPermission()},
}

var instanceStateCmd = APIEndpoint{
	Name: "instanceState",
	Path: "instances/{name}/state",

	Get: APIEndpointAction{Handler: instanceState, AccessHandler: allowProjectPermission()},
	Put: APIEndpointAction{Handler: instanceStatePut, AccessHandler: allowProjectPermission()},
}

var instanceSFTPCmd = APIEndpoint{
	Name: "instanceFile",
	Path: "instances/{name}/sftp",

	Get: APIEndpointAction{Handler: instanceSFTPHandler, AccessHandler: allowProjectPermission()},
}

var instanceFileCmd = APIEndpoint{
	Name: "instanceFile",
	Path: "instances/{name}/files",

	Get:    APIEndpointAction{Handler: instanceFileHandler, AccessHandler: allowProjectPermission()},
	Head:   APIEndpointAction{Handler: instanceFileHandler, AccessHandler: allowProjectPermission()},
	Post:   APIEndpointAction{Handler: instanceFileHandler, AccessHandler: allowProjectPermission()},
	Delete: APIEndpointAction{Handler: instanceFileHandler, AccessHandler: allowProjectPermission()},
}

var instanceSnapshotsCmd = APIEndpoint{
	Name: "instanceSnapshots",
	Path: "instances/{name}/snapshots",

	Get:  APIEndpointAction{Handler: instanceSnapshotsGet, AccessHandler: allowProjectPermission()},
	Post: APIEndpointAction{Handler: instanceSnapshotsPost, AccessHandler: allowProjectPermission()},
}

var instanceSnapshotCmd = APIEndpoint{
	Name: "instanceSnapshot",
	Path: "instances/{name}/snapshots/{snapshotName}",

	Get:    APIEndpointAction{Handler: instanceSnapshotHandler, AccessHandler: allowProjectPermission()},
	Post:   APIEndpointAction{Handler: instanceSnapshotHandler, AccessHandler: allowProjectPermission()},
	Delete: APIEndpointAction{Handler: instanceSnapshotHandler, AccessHandler: allowProjectPermission()},
	Patch:  APIEndpointAction{Handler: instanceSnapshotHandler, AccessHandler: allowProjectPermission()},
	Put:    APIEndpointAction{Handler: instanceSnapshotHandler, AccessHandler: allowProjectPermission()},
}

var instanceConsoleCmd = APIEndpoint{
	Name: "instanceConsole",
	Path: "instances/{name}/console",

	Get:    APIEndpointAction{Handler: instanceConsoleLogGet, AccessHandler: allowProjectPermission()},
	Post:   APIEndpointAction{Handler: instanceConsolePost, AccessHandler: allowProjectPermission()},
	Delete: APIEndpointAction{Handler: instanceConsoleLogDelete, AccessHandler: allowProjectPermission()},
}

var instanceExecCmd = APIEndpoint{
	Name: "instanceExec",
	Path: "instances/{name}/exec",

	Post: APIEndpointAction{Handler: instanceExecPost, AccessHandler: allowProjectPermission()},
}

var instanceMetadataCmd = APIEndpoint{
	Name: "instanceMetadata",
	Path: "instances/{name}/metadata",

	Get:   APIEndpointAction{Handler: instanceMetadataGet, AccessHandler: allowProjectPermission()},
	Patch: APIEndpointAction{Handler: instanceMetadataPatch, AccessHandler: allowProjectPermission()},
	Put:   APIEndpointAction{Handler: instanceMetadataPut, AccessHandler: allowProjectPermission()},
}

var instanceMetadataTemplatesCmd = APIEndpoint{
	Name: "instanceMetadataTemplates",
	Path: "instances/{name}/metadata/templates",

	Get:    APIEndpointAction{Handler: instanceMetadataTemplatesGet, AccessHandler: allowProjectPermission()},
	Post:   APIEndpointAction{Handler: instanceMetadataTemplatesPost, AccessHandler: allowProjectPermission()},
	Delete: APIEndpointAction{Handler: instanceMetadataTemplatesDelete, AccessHandler: allowProjectPermission()},
}

var instanceBackupsCmd = APIEndpoint{
	Name: "instanceBackups",
	Path: "instances/{name}/backups",

	Get:  APIEndpointAction{Handler: instanceBackupsGet, AccessHandler: allowProjectPermission()},
	Post: APIEndpointAction{Handler: instanceBackupsPost, AccessHandler: allowProjectPermission()},
}

var instanceBackupCmd = APIEndpoint{
	Name: "instanceBackup",
	Path: "instances/{name}/backups/{backupName}",

	Get:    APIEndpointAction{Handler: instanceBackupGet, AccessHandler: allowProjectPermission()},
	Post:   APIEndpointAction{Handler: instanceBackupPost, AccessHandler: allowProjectPermission()},
	Delete: APIEndpointAction{Handler: instanceBackupDelete, AccessHandler: allowProjectPermission()},
}

var instanceBackupExportCmd = APIEndpoint{
	Name: "instanceBackupExport",
	Path: "instances/{name}/backups/{backupName}/export",

	Get: APIEndpointAction{Handler: instanceBackupExportGet, AccessHandler: allowProjectPermission()},
}

type instanceAutostartList []instance.Instance

func (slice instanceAutostartList) Len() int {
	return len(slice)
}

func (slice instanceAutostartList) Less(i, j int) bool {
	iOrder := slice[i].ExpandedConfig()["boot.autostart.priority"]
	jOrder := slice[j].ExpandedConfig()["boot.autostart.priority"]

	if iOrder != jOrder {
		iOrderInt, _ := strconv.Atoi(iOrder)
		jOrderInt, _ := strconv.Atoi(jOrder)
		return iOrderInt > jOrderInt
	}

	return slice[i].Name() < slice[j].Name()
}

func (slice instanceAutostartList) Swap(i, j int) {
	slice[i], slice[j] = slice[j], slice[i]
}

var instancesStartMu sync.Mutex

// instanceShouldAutoStart returns whether the instance should be auto-started.
// Returns true if boot.autostart is enabled or boot.autostart is not set and instance was previously running.
func instanceShouldAutoStart(inst instance.Instance) bool {
	config := inst.ExpandedConfig()
	autoStart := config["boot.autostart"]
	lastState := config["volatile.last_state.power"]

	return util.IsTrue(autoStart) || (autoStart == "" && lastState == instance.PowerStateRunning)
}

func instancesStart(s *state.State, instances []instance.Instance) {
	instancesStartMu.Lock()
	defer instancesStartMu.Unlock()

	sort.Sort(instanceAutostartList(instances))

	maxAttempts := 3

	// Start the instances
	for _, inst := range instances {
		if !instanceShouldAutoStart(inst) {
			continue
		}

		// If already running, we're done.
		if inst.IsRunning() {
			continue
		}

		// Get the instance config.
		config := inst.ExpandedConfig()
		autoStartDelay := config["boot.autostart.delay"]

		instLogger := logger.AddContext(logger.Ctx{"project": inst.Project().Name, "instance": inst.Name()})

		// Try to start the instance.
		var attempt = 0
		for {
			attempt++
			err := inst.Start(false)
			if err != nil {
				if api.StatusErrorCheck(err, http.StatusServiceUnavailable) {
					break // Don't log or retry instances that are not ready to start yet.
				}

				instLogger.Warn("Failed auto start instance attempt", logger.Ctx{"attempt": attempt, "maxAttempts": maxAttempts, "err": err})

				if attempt >= maxAttempts {
					// If unable to start after 3 tries, record a warning.
					warnErr := s.DB.Cluster.UpsertWarningLocalNode(inst.Project().Name, cluster.TypeInstance, inst.ID(), warningtype.InstanceAutostartFailure, fmt.Sprintf("%v", err))
					if warnErr != nil {
						instLogger.Warn("Failed to create instance autostart failure warning", logger.Ctx{"err": warnErr})
					}

					instLogger.Error("Failed to auto start instance", logger.Ctx{"err": err})

					break
				}

				time.Sleep(5 * time.Second)

				continue
			}

			// Resolve any previous warning.
			warnErr := warnings.ResolveWarningsByLocalNodeAndProjectAndTypeAndEntity(s.DB.Cluster, inst.Project().Name, warningtype.InstanceAutostartFailure, cluster.TypeInstance, inst.ID())
			if warnErr != nil {
				instLogger.Warn("Failed to resolve instance autostart failure warning", logger.Ctx{"err": warnErr})
			}

			// Wait the auto-start delay if set.
			autoStartDelayInt, err := strconv.Atoi(autoStartDelay)
			if err == nil {
				time.Sleep(time.Duration(autoStartDelayInt) * time.Second)
			}

			break
		}
	}
}

type instanceStopList []instance.Instance

func (slice instanceStopList) Len() int {
	return len(slice)
}

func (slice instanceStopList) Less(i, j int) bool {
	iOrder := slice[i].ExpandedConfig()["boot.stop.priority"]
	jOrder := slice[j].ExpandedConfig()["boot.stop.priority"]

	if iOrder != jOrder {
		iOrderInt, _ := strconv.Atoi(iOrder)
		jOrderInt, _ := strconv.Atoi(jOrder)
		return iOrderInt > jOrderInt // check this line (prob <)
	}

	return slice[i].Name() < slice[j].Name()
}

func (slice instanceStopList) Swap(i, j int) {
	slice[i], slice[j] = slice[j], slice[i]
}

// Return all local instances on disk (if instance is running, it will attempt to populate the instance's local
// and expanded config using the backup.yaml file). It will clear the instance's profiles property to avoid needing
// to enrich them from the database.
func instancesOnDisk(s *state.State) ([]instance.Instance, error) {
	var err error

	instancePaths := map[instancetype.Type]string{
		instancetype.Container: internalUtil.VarPath("containers"),
		instancetype.VM:        internalUtil.VarPath("virtual-machines"),
	}

	instanceTypeNames := make(map[instancetype.Type][]os.DirEntry, 2)

	instanceTypeNames[instancetype.Container], err = os.ReadDir(instancePaths[instancetype.Container])
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	instanceTypeNames[instancetype.VM], err = os.ReadDir(instancePaths[instancetype.VM])
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	instances := make([]instance.Instance, 0, len(instanceTypeNames[instancetype.Container])+len(instanceTypeNames[instancetype.VM]))
	for instanceType, instanceNames := range instanceTypeNames {
		for _, file := range instanceNames {
			// Convert file name to project name and instance name.
			projectName, instanceName := project.InstanceParts(file.Name())

			var inst instance.Instance

			// Try and parse the backup file (if instance is running).
			// This allows us to stop VMs which require access to the vsock ID and volatile UUID.
			// Also generally it ensures that all devices are stopped cleanly too.
			backupYamlPath := filepath.Join(instancePaths[instanceType], file.Name(), "backup.yaml")
			if util.PathExists(backupYamlPath) {
				inst, err = instance.LoadFromBackup(s, projectName, filepath.Join(instancePaths[instanceType], file.Name()), false)
				if err != nil {
					logger.Warn("Failed loading instance", logger.Ctx{"project": projectName, "instance": instanceName, "backup_file": backupYamlPath, "err": err})
				}
			}

			if inst == nil {
				// Initialise dbArgs with a very basic config.
				// This will not be sufficient to stop an instance cleanly.
				instDBArgs := &db.InstanceArgs{
					Type:    instanceType,
					Project: projectName,
					Name:    instanceName,
					Config:  make(map[string]string),
				}

				emptyProject := api.Project{
					Name: projectName,
				}

				inst, err = instance.Load(s, *instDBArgs, emptyProject)
				if err != nil {
					logger.Warn("Failed loading instance", logger.Ctx{"project": projectName, "instance": instanceName, "err": err})
					continue
				}
			}

			instances = append(instances, inst)
		}
	}

	return instances, nil
}

func instancesShutdown(s *state.State, instances []instance.Instance) {
	sort.Sort(instanceStopList(instances))

	// Limit shutdown concurrency to number of instances or number of CPU cores (which ever is less).
	var wg sync.WaitGroup
	instShutdownCh := make(chan instance.Instance)
	maxConcurrent := runtime.NumCPU()
	instCount := len(instances)
	if instCount < maxConcurrent {
		maxConcurrent = instCount
	}

	for i := 0; i < maxConcurrent; i++ {
		go func(instShutdownCh <-chan instance.Instance) {
			for inst := range instShutdownCh {
				// Determine how long to wait for the instance to shutdown cleanly.
				timeoutSeconds := 30
				value, ok := inst.ExpandedConfig()["boot.host_shutdown_timeout"]
				if ok {
					timeoutSeconds, _ = strconv.Atoi(value)
				}

				err := inst.Shutdown(time.Second * time.Duration(timeoutSeconds))
				if err != nil {
					logger.Warn("Failed shutting down instance, forcefully stopping", logger.Ctx{"project": inst.Project().Name, "instance": inst.Name(), "err": err})
					err = inst.Stop(false)
					if err != nil {
						logger.Warn("Failed forcefully stopping instance", logger.Ctx{"project": inst.Project().Name, "instance": inst.Name(), "err": err})
					}
				}

				if inst.ID() > 0 {
					// If DB was available then the instance shutdown process will have set
					// the last power state to STOPPED, so set that back to RUNNING so that
					// when the daemon restarts the instance will be started again.
					_ = inst.VolatileSet(map[string]string{"volatile.last_state.power": instance.PowerStateRunning})
				}

				wg.Done()
			}
		}(instShutdownCh)
	}

	var currentBatchPriority int
	for i, inst := range instances {
		// Skip stopped instances.
		if !inst.IsRunning() {
			continue
		}

		priority, _ := strconv.Atoi(inst.ExpandedConfig()["boot.stop.priority"])

		// Shutdown instances in priority batches, logging at the start of each batch.
		if i == 0 || priority != currentBatchPriority {
			currentBatchPriority = priority

			// Wait for instances with higher priority to finish before starting next batch.
			wg.Wait()
			logger.Info("Stopping instances", logger.Ctx{"stopPriority": currentBatchPriority})
		}

		wg.Add(1)
		instShutdownCh <- inst
	}

	wg.Wait()
	close(instShutdownCh)
}
