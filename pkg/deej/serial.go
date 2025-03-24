package deej

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jacobsa/go-serial/serial"
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
	connOptions serial.OpenOptions
	conn        io.ReadWriteCloser

	lastKnownNumSliders        int
	currentSliderPercentValues []float32

	sliderMoveConsumers []chan SliderMoveEvent

	reconnectTicker *time.Ticker
	stopTicker      chan bool

	retryCount int
	maxRetries int
}

// SliderMoveEvent represents a single slider move captured by deej
type SliderMoveEvent struct {
	SliderID     int
	PercentValue float32
}

var expectedLinePattern = regexp.MustCompile(`^\d{1,4}(\|\d{1,4})*\r\n$`)

// NewSerialIO creates a SerialIO instance that uses the provided deej
// instance's connection info to establish communications with the arduino chip
func NewSerialIO(deej *Deej, logger *zap.SugaredLogger) (*SerialIO, error) {
	logger = logger.Named("serial")

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
		connOptions: serial.OpenOptions{
			DataBits:        8,
			StopBits:        1,
			MinimumReadSize: 1,
		},
	}

	logger.Debug("Created serial i/o instance")

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
				if sio.deej.config.ConnectionInfo.COMPort != sio.connOptions.PortName ||
					uint(sio.deej.config.ConnectionInfo.BaudRate) != sio.connOptions.BaudRate {

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
		number, _ := strconv.Atoi(stringValue)

		if sliderIdx == 0 && number > 1023 {
			logger.Debugw("Got malformed line from serial, ignoring", "line", strings.Join(splitLine, "|"))
			return moveEvents
		}

		normalizedScalar := sio.calculateNormalizedValue(number)

		if util.SignificantlyDifferent(sio.currentSliderPercentValues[sliderIdx], normalizedScalar, sio.deej.config.NoiseReductionLevel) {
			sio.currentSliderPercentValues[sliderIdx] = normalizedScalar
			moveEvents = append(moveEvents, SliderMoveEvent{
				SliderID:     sliderIdx,
				PercentValue: normalizedScalar,
			})

			if sio.deej.Verbose() {
				logger.Debugw("Slider moved", "event", moveEvents[len(moveEvents)-1])
			}
		}
	}

	return moveEvents
}

func (sio *SerialIO) calculateNormalizedValue(rawValue int) float32 {
	dirtyFloat := float32(rawValue) / 1023.0
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
	if sio.connected {
		return nil
	}

	// Update connection options
	sio.connOptions.PortName = sio.deej.config.ConnectionInfo.COMPort
	sio.connOptions.BaudRate = uint(sio.deej.config.ConnectionInfo.BaudRate)

	var err error
	sio.conn, err = serial.Open(sio.connOptions)
	if err != nil {
		sio.retryCount++
		backoff := time.Duration(sio.retryCount) * time.Second

		if sio.retryCount > sio.maxRetries {
			sio.logger.Errorw("Max connection retries reached",
				"attempts", sio.retryCount,
				"error", err)
			return fmt.Errorf("max retries reached: %w", err)
		}

		sio.logger.Warnw("Connection failed, will retry",
			"attempt", sio.retryCount,
			"backoff", backoff,
			"error", err)

		time.Sleep(backoff)
		return sio.connect()
	}

	// Reset retry count on successful connection
	sio.retryCount = 0
	sio.connected = true
	sio.startReading()
	return nil
}

func (sio *SerialIO) startReading() {
	connReader := bufio.NewReader(sio.conn)
	readTimeout := time.Second * 5

	go func() {
		for {
			select {
			case <-sio.stopChannel:
				sio.close(sio.logger)
				return
			default:
				// Set read deadline
				if timeout, ok := sio.conn.(interface{ SetReadDeadline(time.Time) error }); ok {
					_ = timeout.SetReadDeadline(time.Now().Add(readTimeout))
				}

				line, err := connReader.ReadString('\n')
				if err != nil {
					if err != io.EOF && !errors.Is(err, os.ErrDeadlineExceeded) {
						sio.logger.Warnw("Failed to read line", "error", err)
					}
					sio.connected = false
					return
				}
				sio.handleLine(sio.logger, line)
			}
		}
	}()
}
