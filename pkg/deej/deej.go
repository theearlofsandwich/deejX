// Package deej provides a machine-side client that pairs with an Arduino
// chip to form a tactile, physical volume control system/
package deej

import (
	"fmt"
	"os"
	"time"

	"go.uber.org/zap"

	"github.com/omriharel/deej/pkg/deej/util"
)

const (

	// when this is set to anything, deej won't use a tray icon
	envNoTray = "DEEJ_NO_TRAY_ICON"
)

// Deej is the main entity managing access to all sub-components
type Deej struct {
	logger   *zap.SugaredLogger
	notifier Notifier
	config   *CanonicalConfig
	serial   *SerialIO
	sessions *sessionMap

	stopChannel          chan bool
	version              string
	verbose              bool
	masterVolumeStopChan chan bool
}

// NewDeej creates a Deej instance
func NewDeej(logger *zap.SugaredLogger, verbose bool) (*Deej, error) {
	logger = logger.Named("deej")

	notifier, err := NewToastNotifier(logger)
	if err != nil {
		logger.Errorw("Failed to create ToastNotifier", "error", err)
		return nil, fmt.Errorf("create new ToastNotifier: %w", err)
	}

	config, err := NewConfig(logger, notifier)
	if err != nil {
		logger.Errorw("Failed to create Config", "error", err)
		return nil, fmt.Errorf("create new Config: %w", err)
	}

	// Load the config before creating other components
	if err := config.Load(); err != nil {
		logger.Errorw("Failed to load config during initialization", "error", err)
		return nil, fmt.Errorf("load config during init: %w", err)
	}

	d := &Deej{
		logger:      logger,
		notifier:    notifier,
		config:      config,
		stopChannel: make(chan bool),
		verbose:     verbose,
	}

	serial, err := NewSerialIO(d, logger)
	if err != nil {
		logger.Errorw("Failed to create SerialIO", "error", err)
		return nil, fmt.Errorf("create new SerialIO: %w", err)
	}

	d.serial = serial

	sessionFinder, err := newSessionFinder(logger)
	if err != nil {
		logger.Errorw("Failed to create SessionFinder", "error", err)
		return nil, fmt.Errorf("create new SessionFinder: %w", err)
	}

	sessions, err := newSessionMap(d, logger, sessionFinder)
	if err != nil {
		logger.Errorw("Failed to create sessionMap", "error", err)
		return nil, fmt.Errorf("create new sessionMap: %w", err)
	}

	d.sessions = sessions

	logger.Debug("Created deej instance")

	return d, nil
}

// Initialize sets up components and starts to run in the background
func (d *Deej) Initialize() error {
	d.logger.Debug("Initializing")

	// Config is already loaded in NewDeej, so we don't need to load it again
	// Just initialize the session map
	if err := d.sessions.initialize(); err != nil {
		d.logger.Errorw("Failed to initialize session map", "error", err)
		return fmt.Errorf("init session map: %w", err)
	}

	d.setupInterruptHandler()

	// decide whether to run with/without tray
	if _, noTraySet := os.LookupEnv(envNoTray); noTraySet {
		d.logger.Debugw("Running without tray icon", "reason", "envvar set")
		// run in main thread while waiting on ctrl+C
		d.run()
	} else {
		d.initializeTray(d.run)
	}

	return nil
}

func (d *Deej) sendSliderNamesToArduino() {
	if d.config.SliderNames == "" {
		d.logger.Debug("No slider names configured, skipping send to Arduino")
		return
	}

	message := fmt.Sprintf("<^%s>", d.config.SliderNames)
	d.logger.Infow("Sending to serial", "serial", message)
	d.serial.SendToArduino(message)
}

func (d *Deej) startMasterVolumeMonitor() {
	d.masterVolumeStopChan = make(chan bool)

	go func() {
		const (
			lowFreqInterval  = 10 * time.Millisecond
			highFreqInterval = 10 * time.Millisecond
			stableThreshold  = 100 // how many stable cycles before returning to low freq
		)

		var (
			ticker                  = time.NewTicker(lowFreqInterval)
			currentInterval         = lowFreqInterval
			lastVolume      float32 = -1
			lastMute        bool    = false
			stableCounter   int     = 0
		)

		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				sessions, ok := d.sessions.get(masterSessionName)
				if !ok || len(sessions) == 0 {
					continue
				}

				master := sessions[0]
				currentVolume := master.GetVolume()
				currentMute := master.GetMute()

				volumeChanged := lastVolume != currentVolume // && util.SignificantlyDifferent(lastVolume, currentVolume, d.config.NoiseReductionLevel)
				muteChanged := currentMute != lastMute

				if volumeChanged || muteChanged {
					lastVolume = currentVolume
					lastMute = currentMute
					stableCounter = 0

					volumePercent := int(currentVolume * 100)
					muteState := 0
					if currentMute {
						muteState = 1
					}

					message := fmt.Sprintf("<!%d|%d>", muteState, volumePercent)
					d.logger.Infow("Sending to serial", "serial", message)
					d.serial.SendToArduino(message)

					// Increase polling frequency
					if currentInterval != highFreqInterval {
						ticker.Stop()
						ticker = time.NewTicker(highFreqInterval)
						currentInterval = highFreqInterval
						d.logger.Debug("Switching to high-frequency polling")
					}
				} else {
					stableCounter++
					if stableCounter >= stableThreshold && currentInterval != lowFreqInterval {
						ticker.Stop()
						ticker = time.NewTicker(lowFreqInterval)
						currentInterval = lowFreqInterval
						d.logger.Debug("Switching to low-frequency polling")
					}
				}

			case <-d.masterVolumeStopChan:
				d.logger.Debug("Stopping master volume monitor")
				return
			}
		}
	}()
}

func (d *Deej) SendInitialMasterVolume() {
	sessions, ok := d.sessions.get(masterSessionName)
	if !ok || len(sessions) == 0 {
		return
	}

	master := sessions[0]
	currentVolume := master.GetVolume()
	currentMute := master.GetMute()

	volumePercent := int(currentVolume * 100)
	muteState := 0
	if currentMute {
		muteState = 1
	}

	message := fmt.Sprintf("<!%d|%d>", muteState, volumePercent)
	d.logger.Infow("Sending initial master volume to serial", "serial", message)
	d.serial.SendToArduino(message)
}

// SetVersion causes deej to add a version string to its tray menu if called before Initialize
func (d *Deej) SetVersion(version string) {
	d.version = version
}

// Verbose returns a boolean indicating whether deej is running in verbose mode
func (d *Deej) Verbose() bool {
	return d.verbose
}

func (d *Deej) setupInterruptHandler() {
	interruptChannel := util.SetupCloseHandler()

	go func() {
		signal := <-interruptChannel
		d.logger.Debugw("Interrupted", "signal", signal)
		d.signalStop()
	}()
}

func (d *Deej) run() {
	d.logger.Info("Run loop starting")

	// watch the config file for changes
	go d.config.WatchConfigFileChanges()

	// connect to the arduino for the first time
	go func() {
		if err := d.serial.Start(); err != nil {
			d.logger.Warnw("Failed to start first-time serial connection", "error", err)
			// existing error handling...
		}

		// Add small delay to ensure serial connection is ready
		time.Sleep(2000 * time.Millisecond)

		// Run initialization
		d.initializeArduino()

		// Subscribe to reconnection events
		reconnectChannel := d.serial.SubscribeToReconnectEvents()
		d.logger.Debug("Subscribed to serial reconnection events")

		// Listen for reconnection events
		go func() {
			for {
				select {
				case <-reconnectChannel:
					d.logger.Info("Detected serial reconnection, waiting 3 seconds before re-initializing Arduino")
					// Add 3-second delay to ensure serial connection is stable
					time.Sleep(3000 * time.Millisecond)
					d.logger.Info("Delay complete, now re-initializing Arduino")
					d.initializeArduino()
				case <-d.stopChannel:
					d.logger.Debug("Stopping reconnection listener")
					return
				}
			}
		}()
	}()

	// wait until stopped (gracefully)
	<-d.stopChannel
	d.logger.Debug("Stop channel signaled, terminating")

	if err := d.stop(); err != nil {
		d.logger.Warnw("Failed to stop deej", "error", err)
		os.Exit(1)
	} else {
		// exit with 0
		os.Exit(0)
	}
}

func (d *Deej) signalStop() {
	d.logger.Debug("Signalling stop channel")
	d.stopChannel <- true
}

func (d *Deej) stop() error {
	d.logger.Info("Stopping")

	d.config.StopWatchingConfigFile()
	d.serial.Stop()

	// release the session map
	if err := d.sessions.release(); err != nil {
		d.logger.Errorw("Failed to release session map", "error", err)
		return fmt.Errorf("release session map: %w", err)
	}

	d.stopTray()

	// attempt to sync on exit - this won't necessarily work but can't harm
	d.logger.Sync()

	return nil
}

func (d *Deej) startKeepAliveMessageSender() {
	go func() {
		const keepAliveMessage = "<#>"

		sendKeepAlive := func() {
			d.logger.Debugw("Sending keep-alive message", "message", keepAliveMessage)
			if err := d.serial.SendToArduino(keepAliveMessage); err != nil {
				d.logger.Warnw("Failed to send keep-alive message", "error", err)
			}
		}

		// Send initial keep-alive message immediately
		sendKeepAlive()

		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				sendKeepAlive()
			case <-d.stopChannel:
				d.logger.Debug("Stopping keep-alive sender")
				return
			}
		}
	}()
}

// initializeArduino sends all necessary initialization data to the Arduino
func (d *Deej) initializeArduino() {
	d.logger.Info("Initializing Arduino with configuration data")

	// Send slider names to Arduino
	d.sendSliderNamesToArduino()

	// Send initial master volume to Arduino
	d.SendInitialMasterVolume()

	// Start the master volume monitor if it's not already running
	d.startMasterVolumeMonitor()

	// Start the keep-alive sender if it's not already running
	d.startKeepAliveMessageSender()

	d.logger.Info("Arduino initialization complete")
}
