
package dockerutil

import (
	"strings"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// LogConsumer defines the interface for consuming Docker container logs
type LogConsumer interface {
	Log(containerName, message string)
	Err(containerName, message string)
	Status(container, msg string)
	Register(container string)
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

// NewContainerLogConsumer creates a new log consumer with the specified configuration
func NewContainerLogConsumer(config LogConfig) *ContainerLogConsumer {
	return &ContainerLogConsumer{
		Prefix:      config.Prefix,
		ShowService: config.ShowService,
		ShowType:    config.ShowType,
		Level:       config.LogLevel,
		InitChan:    make(chan struct{}),
		FailedChan:  make(chan error),
		InitMessage: config.InitMessage,
	}
}

// LogConfig holds configuration for the log consumer
type LogConfig struct {
	Prefix      string
	ShowService bool
	ShowType    bool
	LogLevel    zerolog.Level
	InitMessage string
}

// DefaultLogConfig returns a default logging configuration
func DefaultLogConfig() LogConfig {
	return LogConfig{
		Prefix:      "docker",
		ShowService: true,
		ShowType:    true,
		LogLevel:    zerolog.Disabled,
		InitMessage: "", // Set appropriate default init message
	}
}

// Log handles stdout messages from containers
func (l *ContainerLogConsumer) Log(containerName, message string) {
	if l.InitMessage != "" && strings.Contains(message, l.InitMessage) {
		select {
		case l.InitChan <- struct{}{}:
		default: // Channel already closed or message already sent
		}
	}
	
	if l.Level == zerolog.Disabled {
		return
	}

	lines := strings.Split(message, "\n")
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			var event *zerolog.Event
			switch l.Level {
			case zerolog.DebugLevel:
				event = log.Debug()
			case zerolog.InfoLevel:
				event = log.Info()
			case zerolog.WarnLevel:
				event = log.Warn()
			case zerolog.ErrorLevel:
				event = log.Error()
			default:
				event = log.Info()
			}

			l.enrichEvent(event, containerName, "stdout")
			event.Msg(line)
		}
	}
}

// Err handles stderr messages from containers
func (l *ContainerLogConsumer) Err(containerName, message string) {
	if l.Level == zerolog.Disabled {
		return
	}
	
	lines := strings.Split(message, "\n")
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			event := log.Error()
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
	
	event := log.Info()
	l.enrichEvent(event, container, "status")
	event.Msg(msg)
}

// Register handles container registration events
func (l *ContainerLogConsumer) Register(container string) {
	if l.Level == zerolog.Disabled {
		return
	}

	event := log.Info()
	l.enrichEvent(event, container, "register")
	event.Msg("container registered")
}

// enrichEvent adds common fields to log events
func (l *ContainerLogConsumer) enrichEvent(event *zerolog.Event, containerName, eventType string) {
	if l.ShowService {
		event.Str("service", containerName)
	}
	if l.ShowType {
		event.Str("type", eventType)
	}
	if l.Prefix != "" {
		event.Str("component", l.Prefix)
	}
}

// SetLogLevel updates the logging level
func (l *ContainerLogConsumer) SetLogLevel(level zerolog.Level) {
	l.Level = level
}

// Close closes the initialization and failure channels
func (l *ContainerLogConsumer) Close() {
	close(l.InitChan)
	close(l.FailedChan)
}
