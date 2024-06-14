package logger

import (
	"github.com/sirupsen/logrus"
)

type entryWrapper struct {
	*logrus.Entry
}

func (ew entryWrapper) addCtx(ctx ...Ctx) {
	for _, c := range ctx {
		ew.WithFields(logrus.Fields(c))
	}
}

func (ew entryWrapper) Panic(msg string, ctx ...Ctx) {
	ew.addCtx(ctx...)
	ew.Entry.Panic(msg)
}

func (ew entryWrapper) Fatal(msg string, ctx ...Ctx) {
	ew.addCtx(ctx...)
	ew.Entry.Fatal(msg)
}

func (ew entryWrapper) Error(msg string, ctx ...Ctx) {
	ew.addCtx(ctx...)
	ew.Entry.Error(msg)
}

func (ew entryWrapper) Warn(msg string, ctx ...Ctx) {
	ew.addCtx(ctx...)
	ew.Entry.Warn(msg)
}

func (ew entryWrapper) Info(msg string, ctx ...Ctx) {
	ew.addCtx(ctx...)
	ew.Entry.Info(msg)
}

func (ew entryWrapper) Debug(msg string, ctx ...Ctx) {
	ew.addCtx(ctx...)
	ew.Entry.Debug(msg)
}

func (ew entryWrapper) Trace(msg string, ctx ...Ctx) {
	ew.addCtx(ctx...)
	ew.Entry.Trace(msg)
}

// ctxLogger returns a logger target with all provided ctx applied.
func (lw *logWrapper) ctxLogger(ctx ...Ctx) Entry {
	var entry *logrus.Entry
	for _, c := range ctx {
		if entry == nil {
			entry = lw.target.WithFields(logrus.Fields(c))
		}
		entry = entry.WithFields(logrus.Fields(c))
	}

	return entryWrapper{entry}
}

func newWrapper(target targetLogger) Logger {
	return &logWrapper{target}
}

type logWrapper struct {
	target targetLogger
}

func (lw *logWrapper) Panic(msg string, ctx ...Ctx) {
	lw.ctxLogger(ctx...).Panic(msg)
}

func (lw *logWrapper) Fatal(msg string, ctx ...Ctx) {
	lw.ctxLogger(ctx...).Fatal(msg)
}

func (lw *logWrapper) Error(msg string, ctx ...Ctx) {
	lw.ctxLogger(ctx...).Error(msg)
}

func (lw *logWrapper) Warn(msg string, ctx ...Ctx) {
	lw.ctxLogger(ctx...).Warn(msg)
}

func (lw *logWrapper) Info(msg string, ctx ...Ctx) {
	lw.ctxLogger(ctx...).Info(msg)
}

func (lw *logWrapper) Debug(msg string, ctx ...Ctx) {
	lw.ctxLogger(ctx...).Debug(msg)
}

func (lw *logWrapper) Trace(msg string, ctx ...Ctx) {
	lw.ctxLogger(ctx...).Trace(msg)
}

func (lw *logWrapper) AddContext(ctx Ctx) Entry {
	return lw.ctxLogger(ctx)
}

func (lw *logWrapper) GetLevel() logrus.Level {
	return lw.target.GetLevel()
}
