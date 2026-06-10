package logging

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Level string

const (
	LevelInfo     Level = "info"
	LevelWarn     Level = "warn"
	LevelError    Level = "error"
	LevelCritical Level = "critical"
)

type Event struct {
	Timestamp string         `json:"timestamp"`
	Level     Level          `json:"level"`
	OfficeKey string         `json:"officeKey,omitempty"`
	Component string         `json:"component"`
	Name      string         `json:"name"`
	Message   string         `json:"message,omitempty"`
	Fields    map[string]any `json:"fields,omitempty"`
}

type Logger struct {
	mu        sync.Mutex
	file      *os.File
	path      string
	officeKey string
}

var defaultLogger = &Logger{}

func Configure(logDir, officeKey string) (string, error) {
	return defaultLogger.Configure(logDir, officeKey)
}

func Path() string {
	return defaultLogger.Path()
}

func Info(component, name, message string, fields map[string]any) {
	defaultLogger.Log(LevelInfo, component, name, message, fields)
}

func Warn(component, name, message string, fields map[string]any) {
	defaultLogger.Log(LevelWarn, component, name, message, fields)
}

func Error(component, name, message string, fields map[string]any) {
	defaultLogger.Log(LevelError, component, name, message, fields)
}

func Critical(component, name, message string, fields map[string]any) {
	defaultLogger.Log(LevelCritical, component, name, message, fields)
}

func (l *Logger) Configure(logDir, officeKey string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return "", err
	}

	fileName := "events-" + officeKey + "-" + time.Now().UTC().Format("20060102-150405") + ".jsonl"
	path := filepath.Join(logDir, fileName)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", err
	}

	if l.file != nil {
		_ = l.file.Close()
	}
	l.file = file
	l.path = path
	l.officeKey = officeKey
	return path, nil
}

func (l *Logger) Path() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.path
}

func (l *Logger) Log(level Level, component, name, message string, fields map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file == nil {
		return
	}

	event := Event{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Level:     level,
		OfficeKey: l.officeKey,
		Component: component,
		Name:      name,
		Message:   message,
		Fields:    cloneFields(fields),
	}

	raw, err := json.Marshal(event)
	if err != nil {
		return
	}
	raw = append(raw, '\n')
	_, _ = l.file.Write(raw)
}

func cloneFields(fields map[string]any) map[string]any {
	if len(fields) == 0 {
		return nil
	}
	cloned := make(map[string]any, len(fields))
	for k, v := range fields {
		cloned[k] = v
	}
	return cloned
}
