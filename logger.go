
package dockerutil

import (
	"strings"
	"io"
	"os"
	"bytes"
	"time"
	
	"github.com/rs/zerolog"
)

type LogOutput int
const (
	LogToNowhere LogOutput = iota
	LogToStdout
	LogToBuffer
	LogToBoth
)

var (
	// docker's logger:
	// never disable this logger at it is monitored for init message.
	// To hide logs pass level zerolog.Disabled in LogConfig to NewContainerLogConsumer.
	DockerLogger zerolog.Logger
	DockerLogBuffer bytes.Buffer
	logOutput = LogToNowhere
)

// SetLogOutput configures where Docker logs are written
func SetLogOutput(output LogOutput) {
	logOutput = output
	updateDockerLogger()
}

func updateDockerLogger() {
	var writers []io.Writer

	switch logOutput {
	case LogToNowhere:
	case LogToStdout:
		writers = append(writers, os.Stdout)
	case LogToBuffer:
		writers = append(writers, &DockerLogBuffer)
	case LogToBoth:
		// These will now be independent writers.
		writers = append(writers, os.Stdout, &DockerLogBuffer)
	default:
		writers = append(writers, os.Stdout, &DockerLogBuffer)
	}

	// Use MultiLevelWriter to handle multiple independent outputs.
	// ConsoleWriter will be used for all writers in the slice.
	multiWriter := zerolog.MultiLevelWriter(writers...)

	w := zerolog.ConsoleWriter{
		Out:        multiWriter,
		TimeFormat: time.TimeOnly,
	}

	DockerLogger = zerolog.New(w).With().Timestamp().Logger()
}

func init() {
	updateDockerLogger()
}

// LogConsumer defines the interface for consuming Docker container logs
type LogConsumer interface {
	Log(containerName, message string)
	Err(containerName, message string)
	Status(container, msg string)
	Register(container string)
	GetInitChan() chan struct{}
}

// ContainerLogConsumer implements log consumption for Docker containers
type ContainerLogConsumer struct {
	Prefix      string
	ShowService bool
	ShowType    bool
	Level       zerolog.Level
	InitChan    chan struct{}
	FailedChan  chan error
	InitMessage string // Message that indicates initialization is complete
}

// LogConfig holds configuration for the log consumer
type LogConfig struct {
	Prefix      string
	ShowService bool
	ShowType    bool
	LogLevel    zerolog.Level
	InitMessage string
}

// NewContainerLogConsumer creates a new log consumer with the specified configuration
func NewContainerLogConsumer(config LogConfig) *ContainerLogConsumer {
	return &ContainerLogConsumer{
		Prefix:      config.Prefix,
		ShowService: config.ShowService,
		ShowType:    config.ShowType,
		Level:       config.LogLevel,
		InitMessage: config.InitMessage,
		InitChan:    make(chan struct{}, 100),
		FailedChan:  make(chan error),
	}
}


// Log handles stdout messages from containers
func (l *ContainerLogConsumer) Log(containerName, message string) {
	l.checkInit(message)
	if l.Level == zerolog.Disabled {
		return
	}

	lines := strings.Split(message, "\n")
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			var event *zerolog.Event
			switch l.Level {
			case zerolog.DebugLevel:
				event = DockerLogger.Debug()
			case zerolog.InfoLevel:
				event = DockerLogger.Info()
			case zerolog.WarnLevel:
				event = DockerLogger.Warn()
			case zerolog.ErrorLevel:
				event = DockerLogger.Error()
			default:
				event = DockerLogger.Info()
			}

			l.enrichEvent(event, containerName, "stdout")
			event.Msg(line)
		}
	}
}

// Err handles stderr messages from containers
func (l *ContainerLogConsumer) Err(containerName, message string) {
	l.checkInit(message)
	if l.Level == zerolog.Disabled {
		return
	}
	
	lines := strings.Split(message, "\n")
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			event := DockerLogger.Error()
			l.enrichEvent(event, containerName, "stderr")
			event.Msg(line)
		}
	}
}

// Status handles container status messages
func (l *ContainerLogConsumer) Status(container, msg string) {
	if l.Level == zerolog.Disabled {
		return
	}
	
	event := DockerLogger.Info()
	l.enrichEvent(event, container, "status")
	event.Msg(msg)
}

// Register handles container registration events
func (l *ContainerLogConsumer) Register(container string) {
	if l.Level == zerolog.Disabled {
		return
	}

	event := DockerLogger.Info()
	l.enrichEvent(event, container, "register")
	event.Msg("container registered")
}

// enrichEvent adds common fields to log events
func (l *ContainerLogConsumer) enrichEvent(event *zerolog.Event, containerName, eventType string) {
	if l.ShowService {
		event.Str("service", containerName)
	}
	if l.Prefix != "" {
		event.Str("instance", l.Prefix)
	}
}


// Close closes the initialization and failure channels
func (l *ContainerLogConsumer) Close() {
	close(l.InitChan)
	close(l.FailedChan)
}

func (l *ContainerLogConsumer) checkInit(message string) {
	if l.InitMessage != "" && strings.Contains(message, l.InitMessage) {
		select {
		case l.InitChan <- struct{}{}:
		default: // Channel already closed or message already sent
		}
	}
}

func (l *ContainerLogConsumer) GetInitChan() chan struct{} {
	return l.InitChan
}

