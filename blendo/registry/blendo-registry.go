package blendoRegistry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"time"

	backendconfig "github.com/rudderlabs/rudder-server/config/backend-config"
	"github.com/rudderlabs/rudder-server/rruntime"
	"github.com/rudderlabs/rudder-server/utils"
	"github.com/rudderlabs/rudder-server/utils/logger"
	"github.com/rudderlabs/rudder-server/utils/sysUtils"

	"github.com/rudderlabs/rudder-server/config"
)

type BlendoSourceT struct {
	Role    string      `json:"role"`
	Options interface{} `json:"options"`
}

type BlendoDestinationT struct {
	Role    string      `json:"role"`
	Options interface{} `json:"options"`
}

type BlendoScheduleT struct {
	Type   string `json:"type"`
	Times  string `json:"times"`
	Hour   int    `json:"hour"`
	Minute int    `json:"minute"`
	Second int    `json:"second"`
}

type BlendoResourcesT struct {
	Role string `json:"role"`
}

type BlendoRegistryPipelineConfigT struct {
	Source    BlendoSourceT      `json:"source"`
	Sink      BlendoDestinationT `json:"sink"`
	Schedule  BlendoScheduleT    `json:"schedule"`
	Resources []BlendoResourcesT `json:"resources"`
	Paused    bool               `json:"paused"`
}
type BlendoRegistryI interface {
	Setup()
}

type BlendoRegistry struct {
	currentSourceJSON backendconfig.SourcesT
	isConfigSet       bool
}

var (
	configBackendURL, workspaceToken string
	blendoRegistryUrl                string
	pollInterval                     time.Duration
	Http                             sysUtils.HttpI   = sysUtils.NewHttp()
	IoUtil                           sysUtils.IoUtilI = sysUtils.NewIoUtil()
	log                              logger.LoggerI   = logger.NewLogger()
)
var mapRoles map[string]string = map[string]string{
	"POSTGRES": "postgres",
}

func loadConfig() {
	configBackendURL = config.GetEnv("CONFIG_BACKEND_URL", "https://api.rudderlabs.com")
	blendoRegistryUrl = config.GetEnv("BLENDO_REGISTRY_URL", "http://localhost:8111")
	pollInterval = config.GetDuration("BlendoConfig.pollIntervalInS", 5) * time.Second
}

func init() {
	config.Initialize()
	loadConfig()
}

// RequestToRegistry sends request to registry
func RequestToRegistry(id string, method string, data interface{}) (response []byte, ok bool) {
	client := &http.Client{}
	url := fmt.Sprintf("%s/syncs/%s", blendoRegistryUrl, id)
	var request *http.Request
	var err error
	if data != nil {
		dataJSON, _ := json.Marshal(data)
		dataJSONReader := bytes.NewBuffer(dataJSON)
		request, err = Http.NewRequest(method, url, dataJSONReader)
	} else {
		request, err = Http.NewRequest(method, url, nil)
	}
	if err != nil {
		log.Errorf("BLENDO Registry: Failed to make %s request: %s, Error: %s", method, url, err.Error())
		return []byte{}, false
	}

	request.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(request)
	if err != nil {
		log.Errorf("BLENDO Registry: Failed to execute %s request: %s, Error: %s", method, url, err.Error())
		return []byte{}, false
	}
	if resp.StatusCode != 200 && resp.StatusCode != 202 {
		log.Errorf("BLENDO Registry: Got error response %d", resp.StatusCode)
	}

	body, err := IoUtil.ReadAll(resp.Body)
	defer resp.Body.Close()

	log.Debugf("BLENDO Registry: Successful %s", string(body))
	return body, true
}

// putConfigToRegistry puts config in blendo registry
func (br *BlendoRegistry) putConfigToRegistry(id string, data interface{}) (response []byte, ok bool) {
	fmt.Printf("Putting to registry %s", id)
	return RequestToRegistry(id, "PUT", data)
}

// deleteConfigFromRegistry deletes config from blendo registry
func (br *BlendoRegistry) deleteConfigFromRegistry(id string) (response []byte, ok bool) {
	fmt.Println("Deleting from registry")
	return RequestToRegistry(id, "DELETE", nil)
}

// calculateDifferencesAndUpdateRegistry calculate the differences from the previous config and make the coresponding actions
// we need to update blendo registry when the destination or source configuration or enabled status change
// and delete from blendo registry when the connection is no longer exists or a source or destination is deleted
func (br *BlendoRegistry) calculateDifferencesAndUpdateRegistry(sources []backendconfig.SourceT) {
	prevSources := br.currentSourceJSON.Sources
	for _, source := range sources {
		if source.SourceDefinition.Category != "cloud" {
			continue
		}
		sourceExists := false
		if br.isConfigSet {
			for _, prevSource := range prevSources {
				if prevSource.ID == source.ID {
					sourceExists = true
					for _, destination := range source.Destinations {
						pipelineId := br.getPipelineId(source.ID, destination.ID)
						// if source deleted, delete all the pipelines from registry for this source
						if source.Deleted != prevSource.Deleted {
							br.deleteConfigFromRegistry(pipelineId)
							continue
						} else {
							destinationExists := false
							for _, prevDestination := range prevSource.Destinations {
								if prevDestination.ID == destination.ID {
									destinationExists = true
									// if destination deleted state has change (this means that deleted is true) remove the pipeline from registry
									if destination.Deleted != prevDestination.Deleted ||
										(destination.IsConnectionEnabled != prevDestination.IsConnectionEnabled &&
											!destination.IsConnectionEnabled) {
										br.deleteConfigFromRegistry(pipelineId)
										// if destination or source enabled or connection status or configuration has change then update registry configuration
									} else if destination.IsProcessorEnabled != prevDestination.IsProcessorEnabled ||
										(destination.IsConnectionEnabled != prevDestination.IsConnectionEnabled &&
											prevDestination.IsConnectionEnabled) ||
										!reflect.DeepEqual(destination.Config, prevDestination.Config) ||
										!reflect.DeepEqual(source.Config, prevSource.Config) {
										br.putConfigToRegistry(pipelineId, br.getConfig(source, destination))
									}
									break
								}
							}
							// if destination not found in the previous destinations, means is new
							if !destinationExists {
								// if source or destination is deleted remove pipeline from blendo registry
								if source.Deleted || destination.Deleted {
									br.deleteConfigFromRegistry(pipelineId)
									// or put the pipeline to blendo registry
								} else {
									br.putConfigToRegistry(pipelineId, br.getConfig(source, destination))
								}
							}
						}
					}
					break
				}
			}
		}
		// if source not found in the previous sources mean is new
		if !sourceExists {
			for _, destination := range source.Destinations {
				pipelineId := br.getPipelineId(source.ID, destination.ID)
				// for each destination remove deleted pipelines from blendo registry
				if source.Deleted || destination.Deleted {
					br.deleteConfigFromRegistry(pipelineId)
					// or put the pipeline to blendo registry
				} else {
					br.putConfigToRegistry(pipelineId, br.getConfig(source, destination))
				}
			}
		}
	}
}

// deleteRemovedSourcesDestinations Removes pipelines from registry that no longer exists comparing the previous configuration
func (br *BlendoRegistry) deleteRemovedSourcesDestinations(sources []backendconfig.SourceT) {
	prevSources := br.currentSourceJSON.Sources
	for _, prevSource := range prevSources {
		if prevSource.SourceDefinition.Category == "cloud" {
			sourceExists := false
			for _, source := range sources {
				if prevSource.ID == source.ID {
					sourceExists = true
					for _, prevDestination := range prevSource.Destinations {
						destExists := false
						for _, destination := range source.Destinations {
							if prevDestination.ID == destination.ID {
								destExists = true
								break
							}
						}
						if !destExists {
							br.deleteConfigFromRegistry(br.getPipelineId(source.ID, prevDestination.ID))
						}
					}
					break
				}
			}
			if !sourceExists {
				for _, destination := range prevSource.Destinations {
					br.deleteConfigFromRegistry(br.getPipelineId(prevSource.ID, destination.ID))
				}
			}
		}
	}
}

// getResources returns the resources of a pipeline in form that registry understands
func (br *BlendoRegistry) getResources(resources []interface{}) []BlendoResourcesT {
	resourcesArray := []BlendoResourcesT{}
	for _, role := range resources {
		resourcesArray = append(resourcesArray, BlendoResourcesT{Role: role.(string)})
	}
	return resourcesArray
}

// getPipelineId returns a pipeline id tha is constructed with a combination of source id and destination id
func (br *BlendoRegistry) getPipelineId(sourceId string, destinationId string) string {
	return fmt.Sprintf("%s_%s", sourceId, destinationId)
}

func (br *BlendoRegistry) mapDestinationconfig(config map[string]interface{}) map[string]interface{} {
	desConfog := make(map[string]interface{})
	for key, value := range config {
		if key == "user" {
			desConfog["username"] = value
		} else if key == "port" {
			port, _ := strconv.Atoi(value.(string))
			desConfog["port"] = port
		} else {
			desConfog[key] = value

		}
	}
	return desConfog
}

// getConfig returns the configuration of the pipeline
func (br *BlendoRegistry) getConfig(source backendconfig.SourceT, destination backendconfig.DestinationT) BlendoRegistryPipelineConfigT {
	sourceResourcesList := source.Config["resources"]
	var resources []BlendoResourcesT
	if sourceResourcesList != nil {
		resources = br.getResources(sourceResourcesList.([]interface{}))
	}
	return BlendoRegistryPipelineConfigT{
		Source: BlendoSourceT{
			Role:    source.SourceDefinition.Name,
			Options: source.Config,
		},
		Sink: BlendoDestinationT{
			Role:    mapRoles[destination.DestinationDefinition.Name],
			Options: br.mapDestinationconfig(destination.Config),
		},
		Schedule: BlendoScheduleT{
			Type: "once_per_hour",
		},
		Resources: resources,
		Paused:    !destination.IsProcessorEnabled,
	}
}

// handleSources updates the registry accordingly
func (br *BlendoRegistry) handleSources(sources []backendconfig.SourceT) {
	if br.isConfigSet {
		br.deleteRemovedSourcesDestinations(sources)
	}
	br.calculateDifferencesAndUpdateRegistry(sources)
}

// backendConfigSubscriber subscribes to backend-config change and updates the registry accordingly
func (br *BlendoRegistry) backendConfigSubscriber() {
	ch := make(chan utils.DataEvent)
	backendconfig.Subscribe(ch, backendconfig.TopicBackendConfig)
	for {
		config := <-ch
		sources := config.Data.(backendconfig.SourcesT)
		fmt.Println("Received Blendo Config")
		br.handleSources(sources.Sources)
		br.currentSourceJSON = config.Data.(backendconfig.SourcesT)
		br.isConfigSet = true
	}
}

func (br *BlendoRegistry) Setup() {
	rruntime.Go(br.backendConfigSubscriber)
}