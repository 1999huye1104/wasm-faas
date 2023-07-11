package agent

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/gorilla/mux"
	"go.opentelemetry.io/otel"
	"go.uber.org/zap"

	"github.com/fission/fission/pkg/crd"
	executorClient "github.com/fission/fission/pkg/executor/client"
	"github.com/fission/fission/pkg/throttler"
	"github.com/fission/fission/pkg/utils/httpserver"
	"github.com/fission/fission/pkg/utils/metrics"
	otelUtils "github.com/fission/fission/pkg/utils/otel"
	"github.com/go-redis/redis"
)

func agent(ctx context.Context, logger *zap.Logger, httpTriggerSet *HTTPTriggerSet) *mutableRouter {
	var mr *mutableRouter
	mux := mux.NewRouter()
	mux.Use(metrics.HTTPMetricMiddleware)

	
	useEncodedPath, _ := strconv.ParseBool(os.Getenv("USE_ENCODED_PATH"))
	if useEncodedPath {
		mr = newMutableRouter(logger, mux.UseEncodedPath())
	} else {
		mr = newMutableRouter(logger, mux)
	}

	httpTriggerSet.subscribeRouter(ctx, mr)
	return mr
}

func serve(ctx context.Context, logger *zap.Logger, port int,
	httpTriggerSet *HTTPTriggerSet, displayAccessLog bool) {
	mr := agent(ctx, logger, httpTriggerSet)
	handler := otelUtils.GetHandlerWithOTEL(mr, "fission-agent", otelUtils.UrlsToIgnore("/agent-healthz"))
	httpserver.StartServer(ctx, logger, "agent", fmt.Sprintf("%d", port), handler)
}

// Start starts a agent
func Start(ctx context.Context, logger *zap.Logger, port int, executorURL string) {
	// fmap := makeFunctionServiceMap(logger, time.Minute)

	fissionClient, kubeClient, _, _, err := crd.MakeFissionClient()
	if err != nil {
		logger.Fatal("error connecting to kubernetes API", zap.Error(err))
	}
	
	// 创建 Redis 客户端
    redisClient := redis.NewClient(&redis.Options{
		Addr:     "redis-service.fission:6379", // Redis 服务器地址和端口
		Password: "123456",              // Redis 服务器密码（如果有的话）
		DB:       0,                     // Redis 数据库索引
	})
	//连接测试
    _, err = redisClient.Ping().Result()
	if err != nil {
		logger.Fatal("error connecting to redis", zap.Error(err))
	}
	logger.Info("redis连接成功！！！")
	err = crd.WaitForCRDs(fissionClient)
	if err != nil {
		logger.Fatal("error waiting for CRDs", zap.Error(err))
	}

	executor := executorClient.MakeClient(logger, executorURL)

	timeoutStr := os.Getenv("ROUTER_ROUND_TRIP_TIMEOUT")
	timeout, err := time.ParseDuration(timeoutStr)
	if err != nil {
		logger.Fatal("failed to parse timeout duration from 'ROUTER_ROUND_TRIP_TIMEOUT'",
			zap.Error(err),
			zap.String("value", timeoutStr))
	}

	timeoutExponentStr := os.Getenv("ROUTER_ROUNDTRIP_TIMEOUT_EXPONENT")
	timeoutExponent, err := strconv.Atoi(timeoutExponentStr)
	if err != nil {
		logger.Fatal("failed to parse timeout exponent from 'ROUTER_ROUNDTRIP_TIMEOUT_EXPONENT'",
			zap.Error(err),
			zap.String("value", timeoutExponentStr))
	}

	keepAliveTimeStr := os.Getenv("ROUTER_ROUND_TRIP_KEEP_ALIVE_TIME")
	keepAliveTime, err := time.ParseDuration(keepAliveTimeStr)
	if err != nil {
		logger.Fatal("failed to parse keep alive duration from 'ROUTER_ROUND_TRIP_KEEP_ALIVE_TIME'",
			zap.Error(err),
			zap.String("value", keepAliveTimeStr))
	}

	disableKeepAliveStr := os.Getenv("ROUTER_ROUND_TRIP_DISABLE_KEEP_ALIVE")
	disableKeepAlive, err := strconv.ParseBool(disableKeepAliveStr)
	if err != nil {
		disableKeepAlive = true
		logger.Fatal("failed to parse enable keep alive from 'ROUTER_ROUND_TRIP_DISABLE_KEEP_ALIVE'",
			zap.Error(err),
			zap.String("value", disableKeepAliveStr))
	}

	maxRetriesStr := os.Getenv("ROUTER_ROUND_TRIP_MAX_RETRIES")
	maxRetries, err := strconv.Atoi(maxRetriesStr)
	if err != nil {
		logger.Fatal("failed to parse max retries from 'ROUTER_ROUND_TRIP_MAX_RETRIES'",
			zap.Error(err),
			zap.String("value", maxRetriesStr))
	}

	isDebugEnvStr := os.Getenv("DEBUG_ENV")
	isDebugEnv, err := strconv.ParseBool(isDebugEnvStr)
	if err != nil {
		logger.Fatal("failed to parse debug env from 'DEBUG_ENV'",
			zap.Error(err),
			zap.String("value", isDebugEnvStr))
	}

	// svcAddrRetryCount is the max times for RetryingRoundTripper to retry with a specific service address
	svcAddrRetryCountStr := os.Getenv("ROUTER_SVC_ADDRESS_MAX_RETRIES")
	svcAddrRetryCount, err := strconv.Atoi(svcAddrRetryCountStr)
	if err != nil {
		svcAddrRetryCount = 5
		logger.Error("failed to parse service address retry count from 'ROUTER_SVC_ADDRESS_MAX_RETRIES' - set to the default value",
			zap.Error(err),
			zap.String("value", svcAddrRetryCountStr),
			zap.Int("default", svcAddrRetryCount))
	}

	// svcAddrUpdateTimeout is the timeout setting for a goroutine to wait for the update of a service entry.
	// If the update process cannot be done within the timeout window, consider it failed.
	svcAddrUpdateTimeoutStr := os.Getenv("ROUTER_SVC_ADDRESS_UPDATE_TIMEOUT")
	svcAddrUpdateTimeout, err := time.ParseDuration(os.Getenv("ROUTER_SVC_ADDRESS_UPDATE_TIMEOUT"))
	if err != nil {
		svcAddrUpdateTimeout = 30 * time.Second
		logger.Error("failed to parse service address update timeout duration from 'ROUTER_ROUND_TRIP_SVC_ADDRESS_UPDATE_TIMEOUT' - set to the default value",
			zap.Error(err),
			zap.String("value", svcAddrUpdateTimeoutStr),
			zap.Duration("default", svcAddrUpdateTimeout))
	}

	// unTapServiceTimeout is the timeout used as timeout in the request context of unTapService
	unTapServiceTimeoutstr := os.Getenv("ROUTER_UNTAP_SERVICE_TIMEOUT")
	unTapServiceTimeout, err := time.ParseDuration(unTapServiceTimeoutstr)
	if err != nil {
		unTapServiceTimeout = 3600 * time.Second
		logger.Error("failed to parse unTap service timeout duration from 'ROUTER_UNTAP_SERVICE_TIMEOUT' - set to the default value",
			zap.Error(err),
			zap.String("value", unTapServiceTimeoutstr),
			zap.Duration("default", unTapServiceTimeout))
	}

	displayAccessLogStr := os.Getenv("DISPLAY_ACCESS_LOG")
	displayAccessLog, err := strconv.ParseBool(displayAccessLogStr)
	if err != nil {
		displayAccessLog = false
		logger.Error("failed to parse 'DISPLAY_ACCESS_LOG' - set to the default value",
			zap.Error(err),
			zap.String("value", displayAccessLogStr),
			zap.Bool("default", displayAccessLog))
	}

	triggers := makeHTTPTriggerSet(logger.Named("agenttriggerset"),  fissionClient, kubeClient, redisClient,executor, &tsRoundTripperParams{
		timeout:           timeout,
		timeoutExponent:   timeoutExponent,
		disableKeepAlive:  disableKeepAlive,
		keepAliveTime:     keepAliveTime,
		maxRetries:        maxRetries,
		svcAddrRetryCount: svcAddrRetryCount,
	}, isDebugEnv, unTapServiceTimeout, throttler.MakeThrottler(svcAddrUpdateTimeout))

	go metrics.ServeMetrics(ctx, logger)

	logger.Info("starting agent", zap.Int("port", port))

	tracer := otel.Tracer("agent")
	ctx, span := tracer.Start(ctx, "agent/Start")
	defer span.End()

	serve(ctx, logger, port, triggers, displayAccessLog)
}
