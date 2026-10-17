package messaging

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/spf13/viper"
	"github.com/techquest-tech/gin-shared/pkg/core"
	sharedmessaging "github.com/techquest-tech/gin-shared/pkg/messaging"
	"go.uber.org/zap"
)

// 监控流专用 redis 连接（可选）。
//
// 不配置 messaging.redis.host 时，监控 adaptor 沿用容器注入的 messaging 服务，
// 也就是应用 redis: 段的那条默认连接（历史行为）。配置后，monitor.tracing /
// monitor.schedule / monitor.error / monitor.connection 四条流只走这条连接，
// 与业务缓存/业务消息队列的 redis 隔离。
//
// 生产端与消费端必须指向同一个 redis 实例/db，否则消费端读不到数据。
const dedicatedRedisConfigKey = "messaging.redis"

const (
	// 以下默认值与 gin-shared 的 redis: 段保持一致：监控 Push 在业务请求路径上
	// 同步执行，连接不可用时应尽快失败，不能把请求拖住。
	defaultRedisPort         = 6379
	defaultRedisPoolSize     = 10
	defaultRedisMinIdle      = 2
	defaultRedisDialTimeout  = time.Second
	defaultRedisReadTimeout  = time.Second
	defaultRedisWriteTimeout = time.Second
	defaultRedisPingTimeout  = 3 * time.Second
)

// redisConfig 是 messaging.redis 段。字段名与 gin-shared 的 redis: 段、
// monitor-adaptor 的 datapool.redis 段一致。
type redisConfig struct {
	Host         string
	Port         int
	Account      string
	Passwd       string
	DB           int
	TLS          bool
	Caroot       string
	PoolSize     int
	MinIdle      int
	ClientName   string
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	PoolTimeout  time.Duration
	MaxRetries   int
}

// loadRedisConfig 读取 messaging.redis。host 为空（或 enabled: false）时返回 nil，
// 表示未配置专属连接。
//
// 逐项用 viper.GetXxx 读取而不是 UnmarshalKey，这样 MESSAGING_REDIS_HOST /
// MESSAGING_REDIS_PASSWD 等环境变量也能覆盖（gin-shared 的 viper 装了
// "." → "_" 的 replacer）。
func loadRedisConfig() *redisConfig {
	host := strings.TrimSpace(viper.GetString(dedicatedRedisConfigKey + ".host"))
	if host == "" {
		return nil
	}
	if viper.IsSet(dedicatedRedisConfigKey+".enabled") && !viper.GetBool(dedicatedRedisConfigKey+".enabled") {
		return nil
	}

	cfg := &redisConfig{
		Host:         host,
		Port:         viper.GetInt(dedicatedRedisConfigKey + ".port"),
		Account:      viper.GetString(dedicatedRedisConfigKey + ".account"),
		Passwd:       viper.GetString(dedicatedRedisConfigKey + ".passwd"),
		DB:           viper.GetInt(dedicatedRedisConfigKey + ".db"),
		TLS:          viper.GetBool(dedicatedRedisConfigKey + ".tls"),
		Caroot:       viper.GetString(dedicatedRedisConfigKey + ".caroot"),
		PoolSize:     viper.GetInt(dedicatedRedisConfigKey + ".poolSize"),
		MinIdle:      viper.GetInt(dedicatedRedisConfigKey + ".minIdle"),
		ClientName:   viper.GetString(dedicatedRedisConfigKey + ".clientName"),
		DialTimeout:  viper.GetDuration(dedicatedRedisConfigKey + ".dialTimeout"),
		ReadTimeout:  viper.GetDuration(dedicatedRedisConfigKey + ".readTimeout"),
		WriteTimeout: viper.GetDuration(dedicatedRedisConfigKey + ".writeTimeout"),
		PoolTimeout:  viper.GetDuration(dedicatedRedisConfigKey + ".poolTimeout"),
		MaxRetries:   viper.GetInt(dedicatedRedisConfigKey + ".maxRetries"),
	}

	if cfg.Port == 0 {
		cfg.Port = defaultRedisPort
	}
	if cfg.PoolSize <= 0 {
		cfg.PoolSize = defaultRedisPoolSize
	}
	// minIdle 显式配 0 就是 0，只有未配置时才用默认值。
	if !viper.IsSet(dedicatedRedisConfigKey + ".minIdle") {
		cfg.MinIdle = defaultRedisMinIdle
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = defaultRedisDialTimeout
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = defaultRedisReadTimeout
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = defaultRedisWriteTimeout
	}
	if cfg.ClientName == "" {
		cfg.ClientName = strings.ReplaceAll(
			fmt.Sprintf("%s-%s-monitor", core.AppName, core.Version), " ", "_")
	}
	return cfg
}

// options 把配置转成 go-redis 选项。TLS 证书读取/解析失败返回 error（配置错误
// 需要暴露），连接是否可用不在这里判断。
func (c *redisConfig) options() (*redis.Options, error) {
	opts := &redis.Options{
		Addr:         fmt.Sprintf("%s:%d", c.Host, c.Port),
		Username:     c.Account,
		Password:     c.Passwd,
		DB:           c.DB,
		PoolSize:     c.PoolSize,
		MinIdleConns: c.MinIdle,
		ClientName:   c.ClientName,
		DialTimeout:  c.DialTimeout,
		ReadTimeout:  c.ReadTimeout,
		WriteTimeout: c.WriteTimeout,
		// MaxRetries 直接透传 go-redis 语义：-1 关闭重试，0 表示默认（3 次）。
		MaxRetries: c.MaxRetries,
	}
	if c.PoolTimeout > 0 {
		opts.PoolTimeout = c.PoolTimeout
	} else {
		opts.PoolTimeout = c.ReadTimeout + time.Second
	}

	if c.TLS {
		tconfig := &tls.Config{
			MinVersion: tls.VersionTLS13,
			ServerName: c.Host,
		}
		if c.Caroot != "" {
			caCert, err := os.ReadFile(c.Caroot)
			if err != nil {
				return nil, fmt.Errorf("read messaging.redis ca pem %q: %w", c.Caroot, err)
			}
			caPool := x509.NewCertPool()
			if !caPool.AppendCertsFromPEM(caCert) {
				return nil, fmt.Errorf("parse messaging.redis ca certs %q failed", c.Caroot)
			}
			tconfig.RootCAs = caPool
		}
		opts.TLSConfig = tconfig
	}
	return opts, nil
}

// applyMessagingSettings 让自建服务与 gin-shared 的 DefaultMessgingService 保持
// 同一份配置语义：messaging.pendingSchedule（pending 重投周期）与
// messaging.settings.<topic>（每条流的 MAXLEN）。
func applyMessagingSettings(svc *sharedmessaging.DefaultMessgingService) {
	if schedule := viper.GetString("messaging.pendingSchedule"); schedule != "" {
		svc.PendingSchedule = schedule
	}

	sub := viper.Sub("messaging")
	if sub == nil {
		return
	}
	for _, key := range sub.AllKeys() {
		if !strings.HasPrefix(key, "settings.") {
			continue
		}
		topic := strings.TrimPrefix(key, "settings.")
		svc.Settings[topic] = sub.GetInt64(key)
	}
}

// newDedicatedService 依据 messaging.redis 构建专属 messaging 服务。
// 未配置返回 (nil, false, nil)；TLS 等配置错误返回 error；连接不可用只告警，
// 不中止进程——客户端会持续重连，监控不应把业务进程拖垮。
func newDedicatedService(logger *zap.Logger) (sharedmessaging.MessagingService, bool, error) {
	cfg := loadRedisConfig()
	if cfg == nil {
		return nil, false, nil
	}

	opts, err := cfg.options()
	if err != nil {
		return nil, true, err
	}

	client := redis.NewClient(opts)
	pingCtx, cancel := context.WithTimeout(context.Background(), defaultRedisPingTimeout)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		logger.Error("dedicated monitor redis unavailable at startup, monitor streams will fail until it recovers",
			zap.String("addr", opts.Addr), zap.Int("db", opts.DB), zap.Error(err))
	} else {
		logger.Info("monitor messaging uses dedicated redis connection",
			zap.String("addr", opts.Addr), zap.Int("db", opts.DB))
	}

	svc := &sharedmessaging.DefaultMessgingService{
		Logger:   logger,
		Client:   client,
		Settings: map[string]int64{},
	}
	applyMessagingSettings(svc)

	core.OnServiceStopping(func() {
		if err := client.Close(); err != nil {
			logger.Warn("close dedicated monitor redis failed", zap.Error(err))
		}
	})
	return svc, true, nil
}

// serviceForAdaptors 返回监控 adaptor 使用的 messaging 服务：配置了
// messaging.redis 时用专属连接，否则回退到容器注入的默认服务。
func serviceForAdaptors(logger *zap.Logger, fallback sharedmessaging.MessagingService) sharedmessaging.MessagingService {
	svc, dedicated, err := newDedicatedService(logger)
	if err != nil {
		logger.Error("invalid messaging.redis config, fall back to the default redis connection", zap.Error(err))
		return fallback
	}
	if dedicated {
		return svc
	}
	logger.Info("monitor messaging uses the default redis connection (messaging.redis.host is empty)")
	return fallback
}
