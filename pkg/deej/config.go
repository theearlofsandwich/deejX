package deej

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
	"go.uber.org/zap"

	"github.com/omriharel/deej/pkg/deej/util"
)

// CanonicalConfig provides application-wide access to configuration fields,
// as well as loading/file watching logic for deej's configuration file
type CanonicalConfig struct {
	SliderMapping   *sliderMap
	IgnoreUnmapped  []string
	SliderMaxVolume map[int]int // Add this field to store max volume per slider

	ConnectionInfo struct {
		COMPort  string
		BaudRate int
	}

	SliderNames string

	InvertSliders bool

	NoiseReductionLevel string

	logger             *zap.SugaredLogger
	notifier           Notifier
	stopWatcherChannel chan bool

	reloadConsumers []chan bool

	userConfig     *viper.Viper
	internalConfig *viper.Viper
}

const (
	userConfigFilepath     = "config.yaml"
	internalConfigFilepath = "preferences.yaml"

	userConfigName     = "config"
	internalConfigName = "preferences"

	userConfigPath = "."

	configType = "yaml"

	configKeySliderMapping       = "slider_mapping"
	configKeyIgnoreUnmapped      = "ignore_unmapped"
	configKeySliderNames         = "slider_names"
	configKeySliderNamesMap      = "slider_names"
	configKeyInvertSliders       = "invert_sliders"
	configKeyCOMPort             = "com_port"
	configKeyBaudRate            = "baud_rate"
	configKeyNoiseReductionLevel = "noise_reduction"
	configKeySliderMaxVolume     = "slider_max_volume"

	defaultCOMPort  = "COM4"
	defaultBaudRate = 9600
)

// has to be defined as a non-constant because we're using path.Join
var internalConfigPath = path.Join(".", logDirectory)

var defaultSliderMapping = func() *sliderMap {
	emptyMap := newSliderMap()
	emptyMap.set(0, []string{masterSessionName})

	return emptyMap
}()

// NewConfig creates a config instance for the deej object and sets up viper instances for deej's config files
func NewConfig(logger *zap.SugaredLogger, notifier Notifier) (*CanonicalConfig, error) {
	logger = logger.Named("config")

	cc := &CanonicalConfig{
		logger:             logger,
		notifier:           notifier,
		reloadConsumers:    []chan bool{},
		stopWatcherChannel: make(chan bool),
	}

	// distinguish between the user-provided config (config.yaml) and the internal config (logs/preferences.yaml)
	userConfig := viper.New()
	userConfig.SetConfigName(userConfigName)
	userConfig.SetConfigType(configType)
	userConfig.AddConfigPath(userConfigPath)

	userConfig.SetDefault(configKeySliderMapping, map[string][]string{})
	userConfig.SetDefault(configKeySliderNames, "")
	userConfig.SetDefault(configKeyInvertSliders, false)
	userConfig.SetDefault(configKeyCOMPort, defaultCOMPort)
	userConfig.SetDefault(configKeyBaudRate, defaultBaudRate)

	internalConfig := viper.New()
	internalConfig.SetConfigName(internalConfigName)
	internalConfig.SetConfigType(configType)
	internalConfig.AddConfigPath(internalConfigPath)

	cc.userConfig = userConfig
	cc.internalConfig = internalConfig

	logger.Debug("Created config instance")

	return cc, nil
}

// Load reads deej's config files from disk and tries to parse them
func (cc *CanonicalConfig) Load() error {
	cc.logger.Debugw("Loading config", "path", userConfigFilepath)

	// make sure it exists
	if !util.FileExists(userConfigFilepath) {
		cc.logger.Warnw("Config file not found", "path", userConfigFilepath)
		cc.notifier.Notify("Can't find configuration!",
			fmt.Sprintf("%s must be in the same directory as deej. Please re-launch", userConfigFilepath))

		return fmt.Errorf("config file doesn't exist: %s", userConfigFilepath)
	}

	// load the user config
	if err := cc.userConfig.ReadInConfig(); err != nil {
		cc.logger.Warnw("Viper failed to read user config", "error", err)

		// if the error is yaml-format-related, show a sensible error. otherwise, show 'em to the logs
		if strings.Contains(err.Error(), "yaml:") {
			cc.notifier.Notify("Invalid configuration!",
				fmt.Sprintf("Please make sure %s is in a valid YAML format.", userConfigFilepath))
		} else {
			cc.notifier.Notify("Error loading configuration!", "Please check deej's logs for more details.")
		}

		return fmt.Errorf("read user config: %w", err)
	}

	// load the internal config - this doesn't have to exist, so it can error
	if err := cc.internalConfig.ReadInConfig(); err != nil {
		cc.logger.Debugw("Viper failed to read internal config", "error", err, "reminder", "this is fine")
	}

	// canonize the configuration with viper's helpers
	if err := cc.populateFromVipers(); err != nil {
		cc.logger.Warnw("Failed to populate config fields", "error", err)
		return fmt.Errorf("populate config fields: %w", err)
	}

	cc.logger.Info("Loaded config successfully")
	cc.logger.Infow("Config values",
		"sliderMapping", cc.SliderMapping,
		"connectionInfo", cc.ConnectionInfo,
		"invertSliders", cc.InvertSliders)

	return nil
}

// SubscribeToChanges allows external components to receive updates when the config is reloaded
func (cc *CanonicalConfig) SubscribeToChanges() chan bool {
	c := make(chan bool)
	cc.reloadConsumers = append(cc.reloadConsumers, c)

	return c
}

// WatchConfigFileChanges starts watching for configuration file changes
// and attempts reloading the config when they happen
func (cc *CanonicalConfig) WatchConfigFileChanges() {
	cc.logger.Debugw("Starting to watch user config file for changes", "path", userConfigFilepath)

	const (
		minTimeBetweenReloadAttempts = time.Millisecond * 500
		delayBetweenEventAndReload   = time.Millisecond * 50
	)

	lastAttemptedReload := time.Now()

	// establish watch using viper as opposed to doing it ourselves, though our internal cooldown is still required
	cc.userConfig.WatchConfig()
	cc.userConfig.OnConfigChange(func(event fsnotify.Event) {

		// when we get a write event...
		if event.Op&fsnotify.Write == fsnotify.Write {

			now := time.Now()

			// ... check if it's not a duplicate (many editors will write to a file twice)
			if lastAttemptedReload.Add(minTimeBetweenReloadAttempts).Before(now) {

				// and attempt reload if appropriate
				cc.logger.Debugw("Config file modified, attempting reload", "event", event)

				// wait a bit to let the editor actually flush the new file contents to disk
				<-time.After(delayBetweenEventAndReload)

				if err := cc.Load(); err != nil {
					cc.logger.Warnw("Failed to reload config file", "error", err)
				} else {
					cc.logger.Info("Reloaded config successfully")
					cc.notifier.Notify("Configuration reloaded!", "Your changes have been applied.")

					cc.onConfigReloaded()
				}

				// don't forget to update the time
				lastAttemptedReload = now
			}
		}
	})

	// wait till they stop us
	<-cc.stopWatcherChannel
	cc.logger.Debug("Stopping user config file watcher")
	cc.userConfig.OnConfigChange(nil)
}

// StopWatchingConfigFile signals our filesystem watcher to stop
func (cc *CanonicalConfig) StopWatchingConfigFile() {
	cc.stopWatcherChannel <- true
}

func (cc *CanonicalConfig) populateFromVipers() error {

	// merge the slider mappings from the user and internal configs
	cc.SliderMapping = sliderMapFromConfigs(
		cc.userConfig.GetStringMapStringSlice(configKeySliderMapping),
		cc.internalConfig.GetStringMapStringSlice(configKeySliderMapping),
	)

	cc.IgnoreUnmapped = cc.userConfig.GetStringSlice(configKeyIgnoreUnmapped)

	// get the rest of the config fields - viper saves us a lot of effort here
	cc.ConnectionInfo.COMPort = cc.userConfig.GetString(configKeyCOMPort)
	if cc.ConnectionInfo.COMPort == "" {
		cc.logger.Warnw("Empty COM port specified, using default value",
			"key", configKeyCOMPort,
			"defaultValue", defaultCOMPort)
		cc.ConnectionInfo.COMPort = defaultCOMPort
	}

	cc.ConnectionInfo.BaudRate = cc.userConfig.GetInt(configKeyBaudRate)
	if cc.ConnectionInfo.BaudRate <= 0 {
		cc.logger.Warnw("Invalid baud rate specified, using default value",
			"key", configKeyBaudRate,
			"invalidValue", cc.ConnectionInfo.BaudRate,
			"defaultValue", defaultBaudRate)

		cc.ConnectionInfo.BaudRate = defaultBaudRate
	}

	cc.logger.Debugw("Populated connection info",
		"comPort", cc.ConnectionInfo.COMPort,
		"baudRate", cc.ConnectionInfo.BaudRate)

	// Check if slider_names is a string or a map
	if cc.userConfig.IsSet(configKeySliderNames) && cc.userConfig.GetString(configKeySliderNames) != "" {
		// Old format: slider_names is a string
		cc.SliderNames = cc.userConfig.GetString(configKeySliderNames)
	} else if cc.userConfig.IsSet(configKeySliderNamesMap) {
		// New format: slider_names is a map
		sliderNamesMap := cc.userConfig.GetStringMapString(configKeySliderNamesMap)

		// Create a slice to hold names in order
		maxSliderIdx := -1
		for sliderIdxStr := range sliderNamesMap {
			sliderIdx, _ := strconv.Atoi(sliderIdxStr)
			if sliderIdx > maxSliderIdx {
				maxSliderIdx = sliderIdx
			}
		}

		// Create a slice with enough capacity
		sliderNames := make([]string, maxSliderIdx+1)

		// Fill the slice with names from the map
		for sliderIdxStr, name := range sliderNamesMap {
			sliderIdx, _ := strconv.Atoi(sliderIdxStr)
			sliderNames[sliderIdx] = name
		}

		// Join the names with pipe separator
		cc.SliderNames = strings.Join(sliderNames, "|")
	}

	cc.InvertSliders = cc.userConfig.GetBool(configKeyInvertSliders)
	cc.NoiseReductionLevel = cc.userConfig.GetString(configKeyNoiseReductionLevel)

	// Initialize the SliderMaxVolume map
	cc.SliderMaxVolume = make(map[int]int)

	// Check if slider_max_volume is set in the config
	if cc.userConfig.IsSet(configKeySliderMaxVolume) {
		// Get the map from the config
		maxVolumeMap := cc.userConfig.GetStringMap(configKeySliderMaxVolume)

		// Convert the map keys to integers and populate our SliderMaxVolume map
		for sliderIdxStr, maxVolumeValue := range maxVolumeMap {
			sliderIdx, err := strconv.Atoi(sliderIdxStr)
			if err != nil {
				cc.logger.Warnw("Invalid slider index in slider_max_volume",
					"index", sliderIdxStr, "error", err)
				continue
			}

			// Convert the value to an integer
			maxVolume := 100 // Default to 100%

			switch v := maxVolumeValue.(type) {
			case int:
				maxVolume = v
			case float64:
				maxVolume = int(v)
			case string:
				parsedValue, err := strconv.Atoi(v)
				if err != nil {
					cc.logger.Warnw("Invalid max volume value",
						"slider", sliderIdx, "value", v, "error", err)
					continue
				}
				maxVolume = parsedValue
			default:
				cc.logger.Warnw("Unsupported max volume value type",
					"slider", sliderIdx, "type", fmt.Sprintf("%T", v))
				continue
			}

			// Ensure the max volume is between 1 and 100
			if maxVolume < 1 {
				cc.logger.Warnw("Max volume too low, setting to 1", "slider", sliderIdx)
				maxVolume = 1
			} else if maxVolume > 100 {
				cc.logger.Warnw("Max volume too high, setting to 100", "slider", sliderIdx)
				maxVolume = 100
			}

			cc.SliderMaxVolume[sliderIdx] = maxVolume
			cc.logger.Debugw("Set max volume for slider", "slider", sliderIdx, "maxVolume", maxVolume)
		}
	}

	cc.logger.Debug("Populated config fields from vipers")

	return nil
}

func (cc *CanonicalConfig) onConfigReloaded() {
	cc.logger.Debug("Notifying consumers about configuration reload")

	for _, consumer := range cc.reloadConsumers {
		consumer <- true
	}
}
