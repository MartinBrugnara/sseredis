package main

import (
	"fmt"
	"log"
)

const (
	DEBUG = 1 << iota
	INFO
	WARNING
	ERROR
)

var (
	LOG_STR_LV = map[string]int{
		"DEBUG":   DEBUG,
		"INFO":    INFO,
		"WARNING": WARNING,
		"ERROR":   ERROR,
	}

	LOG_LV_STR = map[int]string{
		DEBUG:   "DEBUG",
		INFO:    "INFO",
		WARNING: "WARNING",
		ERROR:   "ERROR",
	}

	LOG_LEVEL int
)

func ext_log(level int, format string, a ...interface{}) {
	if level >= LOG_LEVEL {
		log.Printf(fmt.Sprintf("[%s] %s", LOG_LV_STR[level], format), a...)
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
