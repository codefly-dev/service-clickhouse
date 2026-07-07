package main

// chlog.go — structured rendering of the clickhouse-server log stream (mirrors
// postgres' pglog.go). Both runtimes point clickhouse's stdout/stderr at our
// Wool logger; raw, each line carries clickhouse's own timestamp + thread + log
// level and lands at a single undifferentiated level. chLogWriter parses the
// stock clickhouse log_line format, drops the redundant timestamp (Wool stamps
// its own), and emits each line at the Wool level its clickhouse severity maps
// to.

import (
	"bytes"
	"io"
	"strings"

	"github.com/codefly-dev/core/wool"
	"github.com/mind-build/gortk"
)

// chLog parses clickhouse's default log line:
//
//	2026.06.26 12:34:56.789012 [ 42 ] {} <Information> Application: message
//
// The thread id and the <Level> token are captured; clickhouse severities map
// to canonical levels.
var chLog = mustCompileLog(gortk.LogSpec{
	LineRegex: `^\d{4}\.\d{2}\.\d{2} \d{2}:\d{2}:\d{2}(?:\.\d+)? \[ (?P<pid>\d+) \] \{[^}]*\} <(?P<level>\w+)> (?P<msg>.*)$`,
	LevelMap: map[string]string{
		"Fatal": "fatal", "Critical": "fatal", "Error": "error",
		"Warning": "warn", "Notice": "info", "Information": "info",
		"Debug": "debug", "Trace": "debug", "Test": "debug",
	},
	DefaultLevel: "info",
})

func mustCompileLog(s gortk.LogSpec) *gortk.LogParser {
	p, err := s.Compile()
	if err != nil {
		panic("clickhouse chlog: " + err.Error())
	}
	return p
}

// chLogWriter parses the clickhouse log stream and re-emits each line through
// Wool at a severity-mapped level.
type chLogWriter struct {
	w   *wool.Wool
	buf []byte
}

func newCHLogWriter(w *wool.Wool) *chLogWriter {
	return &chLogWriter{w: w}
}

var _ io.Writer = (*chLogWriter)(nil)

func (p *chLogWriter) Write(b []byte) (int, error) {
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		line := string(bytes.TrimRight(p.buf[:i], "\r"))
		p.buf = p.buf[i+1:]
		p.emit(line)
	}
	return len(b), nil
}

func (p *chLogWriter) emit(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	rec := chLog.Parse(line)
	msg, _ := rec.Fields["msg"].(string)
	if msg == "" {
		msg = line
	}

	var fields []*wool.LogField
	if pid, ok := rec.Fields["pid"].(string); ok && pid != "" {
		fields = append(fields, wool.Field("thread", pid))
	}
	p.logAt(woolLevel(rec.Level), msg, fields...)
}

func woolLevel(level string) wool.Loglevel {
	switch level {
	case "fatal":
		return wool.FATAL
	case "error":
		return wool.ERROR
	case "warn":
		return wool.WARN
	case "debug":
		return wool.DEBUG
	default:
		return wool.INFO
	}
}

func (p *chLogWriter) logAt(level wool.Loglevel, msg string, fields ...*wool.LogField) {
	switch level {
	case wool.FATAL:
		p.w.Fatal(msg, fields...)
	case wool.ERROR:
		p.w.Error(msg, fields...)
	case wool.WARN:
		p.w.Warn(msg, fields...)
	case wool.DEBUG:
		p.w.Debug(msg, fields...)
	default:
		p.w.Info(msg, fields...)
	}
}
