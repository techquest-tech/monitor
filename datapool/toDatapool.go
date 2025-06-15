package datapool

import (
	"time"

	"github.com/samber/lo"
	"github.com/spf13/viper"
	"github.com/techquest-tech/gin-shared/pkg/core"
	"github.com/techquest-tech/gin-shared/pkg/parquet"
	"github.com/techquest-tech/gin-shared/pkg/schedule"
	"github.com/techquest-tech/monitor"
	"go.uber.org/zap"

	p "github.com/parquet-go/parquet-go"
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
		trCh := monitor.TracingAdaptor.Sub("monitor-datapool")
		schemaTracing := parquet.NewParquetDataServiceBySchema(&parquet.ParquetSetting{
			BufferDur:  dur,
			BufferSize: adaptor.BufferSize,
			Compress:   adaptor.Compress,
			Folder:     adaptor.CacheFolder,
			Filename:   "tracing/20060102/150405",
		}, p.SchemaOf(&monitor.TracingDetails{}), core.ToAnyChan(trCh))
		go schemaTracing.Start(core.RootCtx())
		// schedule
		trSche := schedule.JobHistoryAdaptor.Sub("monitor-schedule")
		scheScheduleJob := parquet.NewParquetDataServiceBySchema(&parquet.ParquetSetting{
			BufferDur:  dur,
			BufferSize: adaptor.BufferSize,
			Compress:   adaptor.Compress,
			Folder:     adaptor.CacheFolder,
			Filename:   "schedule/20060102/150405",
		}, p.SchemaOf(&schedule.JobHistory{}), core.ToAnyChan(trSche))
		go scheScheduleJob.Start(core.RootCtx())
		// error
		trError := core.ErrorAdaptor.Sub("monitor-error")
		scheError := parquet.NewParquetDataServiceBySchema(&parquet.ParquetSetting{
			BufferDur:  dur,
			BufferSize: adaptor.BufferSize,
			Compress:   adaptor.Compress,
			Folder:     adaptor.CacheFolder,
			Filename:   "errorReport/20060102/150405",
		}, p.SchemaOf(&ErrorReport4Parquet{}), core.ToAnyChan(trError))
		scheError.Filter = func(msg []any) []any {
			return lo.Map(msg, func(item any, index int) any {
				raw := item.(*core.ErrorReport)
				return ErrorReport4Parquet{
					Error:     raw.Error.Error(),
					FullStack: raw.FullStack,
					Uri:       raw.Uri,
				}
			})
		}

		go scheError.Start(core.RootCtx())

		return nil, nil
	})
}

type ErrorReport4Parquet struct {
	Uri       string
	FullStack []byte
	Error     string
}
