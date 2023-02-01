package basic

import (
	"fmt"

	"go.uber.org/zap"
)

const loggerName = "basic_cluster"

var defaultLogger *zap.SugaredLogger

func init() {
	logger, err := zap.NewProduction()
	if err != nil {
		panic(fmt.Sprintf("error constructing default logger: %s", err))
	}
	defaultLogger = logger.Sugar().Named(loggerName)
}

// Must panics if the last arg in its arg list is an error.
func Must(args ...interface{}) {
	err, ok := args[len(args)-1].(error)
	if ok {
		panic(err)
	}
}

func Must2[V any](v V, err error) V {
	if err != nil {
		panic(err)
	}
	return v
}
