package main

import (
	"fmt"
	"log"
	"os"
)

// Log levels
const (
	DEBUG = 1 << iota
	INFO
	WARNING
	ERROR
	FATAL
)

var (
	LOG_STR_LV = map[string]int{
		"DEBUG":   DEBUG,
		"INFO":    INFO,
		"WARNING": WARNING,
		"ERROR":   ERROR,
		"FATAL":   FATAL,
	}

	LOG_LV_STR = map[int]string{
		DEBUG:   "DEBUG",
		INFO:    "INFO",
		WARNING: "WARNING",
		ERROR:   "ERROR",
		FATAL:   "FATAL",
	}

	LOG_LEVEL = INFO
)

// parseLogLevel returns the level named s (e.g. "INFO").
func parseLogLevel(s string) (int, error) {
	lv, ok := LOG_STR_LV[s]
	if !ok {
		return 0, fmt.Errorf("unknown log level %q (want DEBUG, INFO, WARNING, ERROR or FATAL)", s)
	}
	return lv, nil
}

func ext_log(level int, format string, a ...interface{}) {
	if level >= LOG_LEVEL {
		log.Printf("[%s] %s", LOG_LV_STR[level], fmt.Sprintf(format, a...))
	}
}

func Debug(format string, a ...interface{}) {
	ext_log(DEBUG, format, a...)
}

func Info(format string, a ...interface{}) {
	ext_log(INFO, format, a...)
}

func Warn(format string, a ...interface{}) {
	ext_log(WARNING, format, a...)
}

func Error(format string, a ...interface{}) {
	ext_log(ERROR, format, a...)
}

func Fatal(format string, a ...interface{}) {
	ext_log(FATAL, format, a...)
	os.Exit(1)
}
