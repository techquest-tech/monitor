package messaging

import (
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sharedmessaging "github.com/techquest-tech/gin-shared/pkg/messaging"
	"go.uber.org/zap"
)

// resetViper 清空全局 viper 配置，避免测试之间互相污染。
func resetViper(t *testing.T) {
	t.Helper()
	viper.Reset()
	t.Cleanup(viper.Reset)
}

func TestLoadRedisConfigEmptyHostMeansDisabled(t *testing.T) {
	resetViper(t)
	assert.Nil(t, loadRedisConfig())

	// 只配了其它字段、没有 host，仍然视为未配置。
	viper.Set(dedicatedRedisConfigKey+".port", 6380)
	assert.Nil(t, loadRedisConfig())
}

func TestLoadRedisConfigDefaults(t *testing.T) {
	resetViper(t)
	viper.Set(dedicatedRedisConfigKey+".host", "  monitor-redis.example.com ")
	viper.Set(dedicatedRedisConfigKey+".db", 4)

	cfg := loadRedisConfig()
	require.NotNil(t, cfg)
	assert.Equal(t, "monitor-redis.example.com", cfg.Host)
	assert.Equal(t, 6379, cfg.Port)
	assert.Equal(t, 4, cfg.DB)
	assert.Equal(t, 10, cfg.PoolSize)
	assert.Equal(t, 2, cfg.MinIdle)
	assert.Equal(t, time.Second, cfg.DialTimeout)
	assert.Equal(t, time.Second, cfg.ReadTimeout)
	assert.Equal(t, time.Second, cfg.WriteTimeout)
	assert.NotEmpty(t, cfg.ClientName)

	opts, err := cfg.options()
	require.NoError(t, err)
	assert.Equal(t, "monitor-redis.example.com:6379", opts.Addr)
	assert.Equal(t, 4, opts.DB)
	assert.Equal(t, 10, opts.PoolSize)
	assert.Equal(t, 2, opts.MinIdleConns)
	// 未配 poolTimeout 时取 ReadTimeout+1s。
	assert.Equal(t, 2*time.Second, opts.PoolTimeout)
}

func TestLoadRedisConfigExplicitValues(t *testing.T) {
	resetViper(t)
	viper.Set(dedicatedRedisConfigKey+".host", "10.0.0.8")
	viper.Set(dedicatedRedisConfigKey+".port", 6380)
	viper.Set(dedicatedRedisConfigKey+".account", "monitor")
	viper.Set(dedicatedRedisConfigKey+".passwd", "secret")
	viper.Set(dedicatedRedisConfigKey+".db", 7)
	viper.Set(dedicatedRedisConfigKey+".poolSize", 40)
	viper.Set(dedicatedRedisConfigKey+".minIdle", 0)
	viper.Set(dedicatedRedisConfigKey+".clientName", "monitor-messaging")
	viper.Set(dedicatedRedisConfigKey+".dialTimeout", "2s")
	viper.Set(dedicatedRedisConfigKey+".readTimeout", "3s")
	viper.Set(dedicatedRedisConfigKey+".writeTimeout", "4s")
	viper.Set(dedicatedRedisConfigKey+".poolTimeout", "9s")
	viper.Set(dedicatedRedisConfigKey+".maxRetries", -1)

	cfg := loadRedisConfig()
	require.NotNil(t, cfg)
	opts, err := cfg.options()
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.8:6380", opts.Addr)
	assert.Equal(t, "monitor", opts.Username)
	assert.Equal(t, "secret", opts.Password)
	assert.Equal(t, 7, opts.DB)
	assert.Equal(t, 40, opts.PoolSize)
	assert.Equal(t, 0, opts.MinIdleConns)
	assert.Equal(t, "monitor-messaging", opts.ClientName)
	assert.Equal(t, 2*time.Second, opts.DialTimeout)
	assert.Equal(t, 3*time.Second, opts.ReadTimeout)
	assert.Equal(t, 4*time.Second, opts.WriteTimeout)
	assert.Equal(t, 9*time.Second, opts.PoolTimeout)
	assert.Equal(t, -1, opts.MaxRetries)
}

func TestLoadRedisConfigEnabledFalse(t *testing.T) {
	resetViper(t)
	viper.Set(dedicatedRedisConfigKey+".host", "monitor-redis")
	viper.Set(dedicatedRedisConfigKey+".enabled", false)
	assert.Nil(t, loadRedisConfig())
}

func TestRedisOptionsTLSMissingCaroot(t *testing.T) {
	resetViper(t)
	viper.Set(dedicatedRedisConfigKey+".host", "monitor-redis")
	viper.Set(dedicatedRedisConfigKey+".tls", true)
	viper.Set(dedicatedRedisConfigKey+".caroot", "/nonexistent/ca.pem")

	cfg := loadRedisConfig()
	require.NotNil(t, cfg)
	_, err := cfg.options()
	assert.Error(t, err)
}

func TestApplyMessagingSettings(t *testing.T) {
	resetViper(t)
	viper.Set("messaging.pendingSchedule", "@every 5m")
	viper.Set("messaging.settings.monitor.tracing", 100)
	viper.Set("messaging.settings.monitor.error", 200)

	svc := &sharedmessaging.DefaultMessgingService{Settings: map[string]int64{}}
	applyMessagingSettings(svc)

	assert.Equal(t, "@every 5m", svc.PendingSchedule)
	assert.Equal(t, int64(100), svc.Settings["monitor.tracing"])
	assert.Equal(t, int64(200), svc.Settings["monitor.error"])
}

func TestServiceForAdaptorsFallsBackWhenNotConfigured(t *testing.T) {
	resetViper(t)
	fallback := &sharedmessaging.DefaultMessgingService{
		Logger:   zap.NewNop(),
		Settings: map[string]int64{},
	}

	got := serviceForAdaptors(zap.NewNop(), fallback)
	assert.Same(t, fallback, got)
}

func TestServiceForAdaptorsUsesDedicatedConnection(t *testing.T) {
	resetViper(t)
	// 127.0.0.1:1 必然拒绝连接：验证会构建专属客户端，且连接失败不中止。
	viper.Set(dedicatedRedisConfigKey+".host", "127.0.0.1")
	viper.Set(dedicatedRedisConfigKey+".port", 1)

	fallback := &sharedmessaging.DefaultMessgingService{
		Logger:   zap.NewNop(),
		Settings: map[string]int64{},
	}

	got := serviceForAdaptors(zap.NewNop(), fallback)
	require.NotNil(t, got)
	assert.NotSame(t, fallback, got)

	dedicated, ok := got.(*sharedmessaging.DefaultMessgingService)
	require.True(t, ok)
	require.NotNil(t, dedicated.Client)
	assert.Equal(t, "127.0.0.1:1", dedicated.Client.Options().Addr)
	assert.NotNil(t, dedicated.Settings)
}
