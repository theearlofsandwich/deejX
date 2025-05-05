package deej

import (
	"bufio"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.bug.st/serial"
	"go.uber.org/zap"

	"github.com/omriharel/deej/pkg/deej/util"
)

// SerialIO provides a deej-aware abstraction layer to managing serial I/O
type SerialIO struct {
	comPort  string
	baudRate uint

	deej   *Deej
	logger *zap.SugaredLogger

	stopChannel chan bool
	connected   bool
	conn        serial.Port

	lastKnownNumSliders        int
	currentSliderPercentValues []float32

	sliderMoveConsumers []chan SliderMoveEvent
	reconnectNotifiers  []chan bool

	reconnectTicker *time.Ticker
	stopTicker      chan bool

	retryCount int
	maxRetries int
}

// SliderMoveEvent represents a single slider move captured by deej
type SliderMoveEvent struct {
	SliderID     int
	PercentValue float32
	Command      string
}

var expectedLinePattern = regexp.MustCompile(`^(\d{1,4}|[=\+\^\-])(\|(\d{1,4}|[=\+\^\-]))*\r\n$`)

// NewSerialIO creates a SerialIO instance that uses the provided deej
// instance's connection info to establish communications with the arduino chip
func NewSerialIO(deej *Deej, logger *zap.SugaredLogger) (*SerialIO, error) {
	logger = logger.Named("serial")

	// Log the connection info from the config
	logger.Debugw("Connection info from config",
		"comPort", deej.config.ConnectionInfo.COMPort,
		"baudRate", deej.config.ConnectionInfo.BaudRate)

	// Check if the values are empty or zero
	if deej.config.ConnectionInfo.COMPort == "" {
		logger.Warn("COM port is empty in config, using default COM4")
		deej.config.ConnectionInfo.COMPort = "COM5"
	}

	if deej.config.ConnectionInfo.BaudRate == 0 {
		logger.Warn("Baud rate is zero in config, using default 9600")
		deej.config.ConnectionInfo.BaudRate = 9600
	}

	sio := &SerialIO{
		deej:                deej,
		logger:              logger,
		stopChannel:         make(chan bool),
		connected:           false,
		conn:                nil,
		sliderMoveConsumers: []chan SliderMoveEvent{},
		reconnectTicker:     time.NewTicker(30 * time.Second),
		stopTicker:          make(chan bool),
		maxRetries:          5,
		comPort:             deej.config.ConnectionInfo.COMPort,
		baudRate:            uint(deej.config.ConnectionInfo.BaudRate),
	}

	// Log the values after setting them
	logger.Debugw("Created serial i/o instance",
		"comPort", sio.comPort,
		"baudRate", sio.baudRate)

	// respond to config changes
	sio.setupOnConfigReload()

	return sio, nil
}

// Start attempts to connect to our arduino chip
func (sio *SerialIO) Start() error {

	// don't allow multiple concurrent connections
	if sio.connected {
		sio.logger.Warn("Already connected, can't start another without closing first")
		return errors.New("serial: connection already active")
	}

	if err := sio.connect(); err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}

	// Add reconnection goroutine
	go func() {
		for {
			select {
			case <-sio.reconnectTicker.C:
				if !sio.connected {
					sio.logger.Debug("Attempting to reconnect...")
					if err := sio.connect(); err != nil {
						sio.logger.Warnw("Failed to reconnect", "error", err)
					} else {
						sio.logger.Info("Reconnection successful")
					}
				}
			case <-sio.stopTicker:
				sio.reconnectTicker.Stop()
				return
			}
		}
	}()

	return nil
}

// Stop signals us to shut down our serial connection, if one is active
func (sio *SerialIO) Stop() {
	sio.stopTicker <- true
	if sio.connected {
		sio.logger.Debug("Shutting down serial connection")
		sio.stopChannel <- true
	} else {
		sio.logger.Debug("Not currently connected, nothing to stop")
	}
}

// SubscribeToSliderMoveEvents returns an unbuffered channel that receives
// a sliderMoveEvent struct every time a slider moves
func (sio *SerialIO) SubscribeToSliderMoveEvents() chan SliderMoveEvent {
	ch := make(chan SliderMoveEvent, 32) // Add buffer
	sio.sliderMoveConsumers = append(sio.sliderMoveConsumers, ch)
	return ch
}

func (sio *SerialIO) setupOnConfigReload() {
	configReloadedChannel := sio.deej.config.SubscribeToChanges()

	const stopDelay = 50 * time.Millisecond

	go func() {
		for {
			select {
			case <-configReloadedChannel:

				// make any config reload unset our slider number to ensure process volumes are being re-set
				// (the next read line will emit SliderMoveEvent instances for all sliders)\
				// this needs to happen after a small delay, because the session map will also re-acquire sessions
				// whenever the config file is reloaded, and we don't want it to receive these move events while the map
				// is still cleared. this is kind of ugly, but shouldn't cause any issues
				go func() {
					<-time.After(stopDelay)
					sio.lastKnownNumSliders = 0
				}()

				// if connection params have changed, attempt to stop and start the connection
				if sio.deej.config.ConnectionInfo.COMPort != sio.comPort ||
					uint(sio.deej.config.ConnectionInfo.BaudRate) != sio.baudRate {

					sio.logger.Info("Detected change in connection parameters, attempting to renew connection")
					sio.Stop()

					// let the connection close
					<-time.After(stopDelay)

					if err := sio.Start(); err != nil {
						sio.logger.Warnw("Failed to renew connection after parameter change", "error", err)
					} else {
						sio.logger.Debug("Renewed connection successfully")
					}
				}
			}
		}
	}()
}

func (sio *SerialIO) close(logger *zap.SugaredLogger) {
	if err := sio.conn.Close(); err != nil {
		logger.Warnw("Failed to close serial connection", "error", err)
	} else {
		logger.Debug("Serial connection closed")
	}

	sio.conn = nil
	sio.connected = false
}

func (sio *SerialIO) handleLine(logger *zap.SugaredLogger, line string) {

	//logger.Infow("Got line", "line", line)

	if !expectedLinePattern.MatchString(line) {
		return
	}

	line = strings.TrimSuffix(line, "\r\n")
	splitLine := strings.Split(line, "|")
	numSliders := len(splitLine)

	sio.updateSliderCount(logger, numSliders)
	moveEvents := sio.processSliderValues(logger, splitLine)
	sio.deliverMoveEvents(moveEvents)
}

func (sio *SerialIO) updateSliderCount(logger *zap.SugaredLogger, numSliders int) {
	if numSliders != sio.lastKnownNumSliders {
		logger.Infow("Detected sliders", "amount", numSliders)
		sio.lastKnownNumSliders = numSliders
		sio.currentSliderPercentValues = make([]float32, numSliders)

		for idx := range sio.currentSliderPercentValues {
			sio.currentSliderPercentValues[idx] = -1.0
		}
	}
}

func (sio *SerialIO) processSliderValues(logger *zap.SugaredLogger, splitLine []string) []SliderMoveEvent {
	moveEvents := []SliderMoveEvent{}

	for sliderIdx, stringValue := range splitLine {

		// skip to other values if first value is "="
		if stringValue == "=" {
			continue
		}

		// if the value is a special character, handle it
		if stringValue == "+" || stringValue == "-" || stringValue == "^" {
			moveEvents = append(moveEvents, SliderMoveEvent{
				SliderID:     sliderIdx,
				PercentValue: 1.0,
				Command:      stringValue,
			})

			if sio.deej.Verbose() {
				logger.Debugw("Command received", "event", moveEvents[len(moveEvents)-1])
			}
			continue
		}

		number, _ := strconv.Atoi(stringValue)

		// Error if master volume > 100
		if sliderIdx == 0 && number > 100 {
			logger.Debugw("Got malformed line from serial, ignoring", "line", strings.Join(splitLine, "|"))
			return moveEvents
		}

		// Convert percentage to 0 - 1
		normalizedScalar := sio.calculateNormalizedValue(number)

		//if util.SignificantlyDifferent(sio.currentSliderPercentValues[sliderIdx], normalizedScalar, sio.deej.config.NoiseReductionLevel) {
		sio.currentSliderPercentValues[sliderIdx] = normalizedScalar
		moveEvents = append(moveEvents, SliderMoveEvent{
			SliderID:     sliderIdx,
			PercentValue: normalizedScalar,
			Command:      "=",
		})

		if sio.deej.Verbose() {
			logger.Debugw("Slider moved", "event", moveEvents[len(moveEvents)-1])
		}
	}

	return moveEvents
}

func (sio *SerialIO) calculateNormalizedValue(rawValue int) float32 {
	dirtyFloat := float32(rawValue) / 100.0
	normalizedScalar := util.NormalizeScalar(dirtyFloat)

	if sio.deej.config.InvertSliders {
		normalizedScalar = 1 - normalizedScalar
	}

	return normalizedScalar
}

func (sio *SerialIO) deliverMoveEvents(moveEvents []SliderMoveEvent) {
	if len(moveEvents) > 0 {
		for _, consumer := range sio.sliderMoveConsumers {
			for _, moveEvent := range moveEvents {
				consumer <- moveEvent
			}
		}
	}
}

func (sio *SerialIO) connect() error {
	sio.logger.Debugw("Attempting to connect", "comPort", sio.comPort, "baudRate", sio.baudRate)

	// Configure serial port
	mode := &serial.Mode{
		BaudRate: int(sio.baudRate),
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	}

	// Open the port
	conn, err := serial.Open(sio.comPort, mode)
	if err != nil {
		return fmt.Errorf("failed to open serial port: %w", err)
	}

	sio.conn = conn
	sio.connected = true

	// Start reading routine
	go sio.readFromSerial()

	return nil
}

func (sio *SerialIO) readFromSerial() {
	logger := sio.logger.Named("read")
	reader := bufio.NewReader(sio.conn)

	defer func() {
		sio.connected = false
		logger.Debug("Serial connection closed, notifying subscribers")

		// Notify reconnect subscribers
		for _, notifier := range sio.reconnectNotifiers {
			notifier <- false
		}
	}()

	for {
		select {
		case <-sio.stopChannel:
			logger.Debug("Received stop signal, closing connection")
			sio.close(logger)
			return
		default:
			line, err := reader.ReadString('\n')
			if err != nil {
				logger.Warnw("Failed to read line from serial", "error", err)
				sio.close(logger)
				return
			}

			sio.handleLine(logger, line)
		}
	}
}

func (sio *SerialIO) SendToArduino(message string) error {
	if !sio.connected || sio.conn == nil {
		return errors.New("serial not connected")
	}

	_, err := sio.conn.Write([]byte(message))
	if err != nil {
		sio.logger.Warnw("Failed to write to Arduino", "error", err)
		return err
	}

	return nil
}

// notifyReconnected signals that a reconnection was successful
func (sio *SerialIO) notifyReconnected() {
	// Only notify if this wasn't the first connection
	if sio.retryCount > 0 {
		sio.logger.Info("Serial connection re-established successfully")

		// Notify subscribers about reconnection
		for _, ch := range sio.reconnectNotifiers {
			select {
			case ch <- true:
				// Successfully sent notification
			default:
				// Channel buffer full, skip notification
			}
		}
	}
}

// SubscribeToReconnectEvents returns a buffered channel that receives
// a notification when serial connection is re-established
func (sio *SerialIO) SubscribeToReconnectEvents() chan bool {
	ch := make(chan bool, 1) // Buffer of 1 to prevent blocking
	sio.reconnectNotifiers = append(sio.reconnectNotifiers, ch)
	return ch
}
