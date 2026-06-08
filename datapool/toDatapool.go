package datapool

import (
	"fmt"
	"os"
	"time"

	p "github.com/parquet-go/parquet-go"
	"github.com/samber/lo"
	"github.com/spf13/viper"
	"github.com/techquest-tech/gin-shared/pkg/core"
	"github.com/techquest-tech/gin-shared/pkg/parquet"
	"github.com/techquest-tech/gin-shared/pkg/schedule"
	"github.com/techquest-tech/monitor"
	"go.uber.org/zap"
)

const (
	SettingKey = "tracing.datapool"
)

var (
	EnabledAct = false
)

// monitor data to datapool, which is in oss with parquet format
type Monitor2Datapool struct {
	Path string
}

func hasOssEnv() bool {
	return os.Getenv("OSS_BUCKET") != "" &&
		os.Getenv("OSS_ID") != "" &&
		os.Getenv("OSS_SECRET") != "" &&
		os.Getenv("OSS_ENDPOINT") != "" &&
		os.Getenv("OSS_REGION") != ""
}

func Enabled2Datapool() {
	core.ProvideStartup(func(logger *zap.Logger) (core.Startup, error) {
		adaptor := &Monitor2Datapool{
			Path: "./data/monitor",
		}

		if !viper.IsSet(SettingKey) {
			env := os.Getenv("ENV")
			useOss := false
			if env == "uat" || env == "prd" {
				adaptor.Path = fmt.Sprintf("%s/%s/monitor", env, core.AppName)
				useOss = hasOssEnv()
			}
			cfg := map[string]any{
				"type": "local",
				"path": adaptor.Path,
			}
			if useOss {
				cfg["type"] = "oss"
			}
			viper.Set(SettingKey, cfg)
		}
		// or ovwrite by config
		err := viper.UnmarshalKey(SettingKey, adaptor)
		if err != nil {
			logger.Error("unmarshal datapool config error", zap.Error(err))
			return nil, err
		}
		if adaptor.Path == "-" {
			logger.Info("datapool disabled")
			return nil, nil
		}
		storageType := viper.GetString(SettingKey + ".type")
		logger.Info("datapool enabled", zap.String("cacheFolder", adaptor.Path),
			zap.String("storageType", storageType))
		// event, err := parquet.NewOssEventService(logger)
		// if err != nil {
		// 	logger.Warn("init oss event service error, use default", zap.Error(err))
		// 	// return nil, err
		// 	event = &parquet.DefaultPersistEvent{}
		// }

		settings := &parquet.ParquetSetting{
			FsKey:   SettingKey,
			Folder:  adaptor.Path,
			Ackfile: EnabledAct,
		}
		filenamePattern := "%s/20060102/150405"
		// tracing
		trCh := monitor.TracingAdaptor.Sub("monitor-datapool")
		schemaTracing := parquet.NewParquetDataServiceT(settings, filenamePattern, trCh) //.NewParquetDataServiceBySchema(, p.SchemaOf(&monitor.TracingDetails{}), core.ToAnyChan(trCh))
		// schemaTracing.Event = event
		go schemaTracing.Start(core.RootCtx())
		// schedule
		trSche := schedule.JobHistoryAdaptor.Sub("monitor-schedule")
		scheScheduleJob := parquet.NewParquetDataServiceT(settings, filenamePattern, trSche)
		// scheScheduleJob.Event = event
		go scheScheduleJob.Start(core.RootCtx())
		// error
		trError := core.ErrorAdaptor.Sub("monitor-error")
		scheError := parquet.NewParquetDataServiceBySchema(&parquet.ParquetSetting{
			FsKey:           SettingKey,
			Folder:          adaptor.Path,
			FilenamePattern: "errorReport/20060102/150405",
			Ackfile:         EnabledAct,
		}, p.SchemaOf(&ErrorReport4Parquet{}), core.ToAnyChan(trError))
		scheError.Filter = func(msg []any) []any {
			return lo.Map(msg, func(item any, index int) any {
				raw := item.(core.ErrorReport)
				return ErrorReport4Parquet{
					Error:     raw.Error.Error(),
					FullStack: raw.FullStack,
					Uri:       raw.Uri,
					HappendAT: raw.HappendAT,
				}
			})
		}
		// scheError.Event = event

		go scheError.Start(core.RootCtx())

		return nil, nil
	})
}

type ErrorReport4Parquet struct {
	Uri       string
	FullStack []byte
	Error     string
	HappendAT time.Time
}
