package datapool

import (
	"time"

	"github.com/spf13/viper"
	"github.com/techquest-tech/gin-shared/pkg/core"
	"github.com/techquest-tech/gin-shared/pkg/parquet"
	"github.com/techquest-tech/gin-shared/pkg/schedule"
	"github.com/techquest-tech/monitor"
	"go.uber.org/zap"
)

// monitor data to datapool, which is in oss with parquet format
type Monitor2Datapool struct {
	CacheFolder string
	BufferSize  int // settings for batch
	BufferDur   string
	Compress    string
}

func Enabled2Datapool() {
	core.ProvideStartup(func(logger *zap.Logger) (core.Startup, error) {
		adaptor := &Monitor2Datapool{
			CacheFolder: "./data/monitor",
		}
		err := viper.UnmarshalKey("datapool", adaptor)
		if err != nil {
			logger.Error("unmarshal datapool config error", zap.Error(err))
			return nil, err
		}
		dur := 30 * time.Second
		if adaptor.BufferDur != "" {
			dur, err = time.ParseDuration(adaptor.BufferDur)
			if err != nil {
				logger.Error("parse datapool buffer duration error", zap.Error(err))
				return nil, err
			}
		}

		// tracing
		tr := &parquet.Chan2Parquet[monitor.TracingDetails]{
			Folder:     adaptor.CacheFolder,
			Processor:  "tracing",
			BufferSize: adaptor.BufferSize,
			BufferDur:  dur,
			Compress:   adaptor.Compress,
		}
		trCh := monitor.TracingAdaptor.Sub(tr.Processor)
		go tr.Start(trCh)
		// schedule
		schedule2pg := &parquet.Chan2Parquet[schedule.JobHistory]{
			Folder:     adaptor.CacheFolder,
			Processor:  "schedule",
			BufferSize: adaptor.BufferSize,
			BufferDur:  dur,
			Compress:   adaptor.Compress,
		}
		scheudleCh := schedule.JobHistoryAdaptor.Sub(schedule2pg.Processor)
		go schedule2pg.Start(scheudleCh)

		// error
		error2pg := &parquet.Chan2Parquet[core.ErrorReport]{
			Folder:     adaptor.CacheFolder,
			Processor:  "error",
			BufferSize: adaptor.BufferSize,
			BufferDur:  dur,
			Compress:   adaptor.Compress,
		}
		err2ch := core.ErrorAdaptor.Sub(error2pg.Processor)
		go error2pg.Start(err2ch)

		return nil, nil
	})
}
