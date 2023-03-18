package common

import (
	"context"

	log "github.com/sirupsen/logrus"
)

type CtxKey string

func WithLogger(ctx context.Context, logger *log.Entry) context.Context {
	return context.WithValue(ctx, CtxKey("logger"), logger)
}

func GetLogger(ctx context.Context) *log.Entry {
	logger, ok := ctx.Value(CtxKey("logger")).(*log.Entry)
	if !ok {
		return log.WithField("daemon", "kubedtnd")
	}
	return logger
}
