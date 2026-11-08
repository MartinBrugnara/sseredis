package main

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
)

func TestParseLogLevel(t *testing.T) {
	for name, want := range map[string]int{
		"DEBUG": DEBUG, "INFO": INFO, "WARNING": WARNING, "ERROR": ERROR, "FATAL": FATAL,
	} {
		got, err := parseLogLevel(name)
		if err != nil || got != want {
			t.Errorf("parseLogLevel(%q) = %d, %v; want %d", name, got, err, want)
		}
	}
	for _, bad := range []string{"", "info", "VERBOSE"} {
		if _, err := parseLogLevel(bad); err == nil {
			t.Errorf("parseLogLevel(%q) did not fail", bad)
		}
	}
}

func TestLogLevelFilterAndLabel(t *testing.T) {
	var buf bytes.Buffer
	oldLevel := LOG_LEVEL
	t.Cleanup(func() { log.SetOutput(os.Stderr); LOG_LEVEL = oldLevel })
	log.SetOutput(&buf)

	LOG_LEVEL = WARNING
	Debug("hidden debug")
	Info("hidden info")
	Warn("shown %d", 1)
	ext_log(FATAL, "fatal %s", "label")

	out := buf.String()
	if strings.Contains(out, "hidden") {
		t.Errorf("messages below the level were logged:\n%s", out)
	}
	if !strings.Contains(out, "[WARNING] shown 1") {
		t.Errorf("missing warning:\n%s", out)
	}
	if !strings.Contains(out, "[FATAL] fatal label") {
		t.Errorf("fatal message not labelled FATAL:\n%s", out)
	}
}
