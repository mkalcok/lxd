package logger

import (
	"github.com/sirupsen/logrus"
)

// Ctx is the logging context.
type Ctx logrus.Fields

// Log contains the logger used by all the logging functions.
var Log Logger

type Entry interface {
	Panic(msg string, args ...Ctx)
	Fatal(msg string, args ...Ctx)
	Error(msg string, args ...Ctx)
	Warn(msg string, args ...Ctx)
	Info(msg string, args ...Ctx)
	Debug(msg string, args ...Ctx)
	Trace(msg string, args ...Ctx)
}

// Logger is the main logging interface.
type Logger interface {
	Entry
	AddContext(Ctx) Entry
	GetLevel() logrus.Level
}

// targetLogger represents the subset of logrus.Logger and logrus.Entry that we care about.
type targetLogger interface {
	Panic(args ...interface{})
	Fatal(args ...interface{})
	Error(args ...interface{})
	Warn(args ...interface{})
	Info(args ...interface{})
	Debug(args ...interface{})
	Trace(args ...interface{})
	WithFields(fields logrus.Fields) *logrus.Entry
	GetLevel() logrus.Level
}
