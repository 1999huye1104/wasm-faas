/*
Copyright 2016 The Fission Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
	"io/ioutil"
	"github.com/pkg/errors"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.uber.org/zap"
	k8stypes "k8s.io/apimachinery/pkg/types"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
	"github.com/fission/fission/pkg/crd"
	ferror "github.com/fission/fission/pkg/error"
	"github.com/fission/fission/pkg/error/network"
	executorClient "github.com/fission/fission/pkg/executor/client"
	"github.com/fission/fission/pkg/throttler"
	"github.com/fission/fission/pkg/utils"
	"github.com/go-redis/redis"
	otelUtils "github.com/fission/fission/pkg/utils/otel"
	
)

const (
	// FORWARDED represents the 'Forwarded' request header
	FORWARDED = "Forwarded"

	// X_FORWARDED_HOST represents the 'X_FORWARDED_HOST' request header
	X_FORWARDED_HOST = "X-Forwarded-Host"
)

type (
	functionHandler struct {
		logger                   *zap.Logger
		// fmap                     *functionServiceMap
		redisClient              *redis.Client
		executor                 *executorClient.Client
		function                 *fv1.Function
		httpTrigger              *fv1.HTTPTrigger
		functionMap              map[string]*fv1.Function
		fnWeightDistributionList []functionWeightDistribution
		tsRoundTripperParams     *tsRoundTripperParams
		isDebugEnv               bool
		svcAddrUpdateThrottler   *throttler.Throttler
		functionTimeoutMap       map[k8stypes.UID]int
		unTapServiceTimeout      time.Duration
		functionOutput           chan string 
	}

	tsRoundTripperParams struct {
		timeout          time.Duration
		timeoutExponent  int
		disableKeepAlive bool
		keepAliveTime    time.Duration

		// maxRetires is the max times for RetryingRoundTripper to retry a request.
		// Default maxRetries is 10, which means router will retry for
		// up to 10 times and abort it if still not succeeded.
		maxRetries int

		// svcAddrRetryCount is the max times for RetryingRoundTripper to retry with a specific service address
		// Router sends requests to a specific service address for each function.
		// A service address is considered as an invalid one if amount of non-network
		// errors router received is higher than svcAddrRetryCount.
		// Try to get a new one from executor.
		// Default svcAddrRetryCount is 5.
		svcAddrRetryCount int
	}

	// RetryingRoundTripper is a layer on top of http.DefaultTransport, with retries.
	RetryingRoundTripper struct {
		logger           *zap.Logger
		funcHandler      *functionHandler
		funcTimeout      time.Duration
		closeContextFunc *context.CancelFunc
		serviceURL       *url.URL
		urlFromCache     bool
		totalRetry       int
	}

	// To keep the request body open during retries, we create an interface with Close operation being a no-op.
	// Details : https://github.com/flynn/flynn/pull/875
	fakeCloseReadCloser struct {
		io.ReadCloser
	}

	svcEntryRecord struct {
		svcURL   *url.URL
		cacheHit bool
	}
)

func init() {
	// just seeding the random number for getting the canary function
	rand.Seed(time.Now().UnixNano())
}

func (w *fakeCloseReadCloser) Close() error {
	return nil
}

func (w *fakeCloseReadCloser) RealClose() error {
	if w.ReadCloser == nil {
		return nil
	}
	return w.ReadCloser.Close()
}

func (fh functionHandler) handler(responseWriter http.ResponseWriter, request *http.Request) {
	 ctx := request.Context()
	 code:=200
     //首先去redis数据库中查找是否有函数结果
	 fn:=fh.function
	 val, err := fh.redisClient.Get(string(fn.UID)).Result()
	 //数据库中存在
	 if err == nil {
		resp:=[]byte(val)
		fh.respondWithSuccess(responseWriter,resp)
		start := time.Now()
		go fh.collectFunctionMetric(start, request, code)
		return 
	 }else if  !errors.Is(err, redis.Nil){
		fh.logger.Error(err.Error())
		fh.respondWithError(responseWriter,err.Error())
		start := time.Now()
		go fh.collectFunctionMetric(start, request, code)
		return 
	 }

	 _, err = fh.getServiceEntryFromExecutor(ctx)
	if err!=nil{
		fh.logger.Error(err.Error())
		fh.respondWithError(responseWriter,err.Error())
		start := time.Now()
		code=404
		go fh.collectFunctionMetric(start, request, code)
		return
	}

    //处理同步
	 if fn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType==fv1.ExecutorTypeWasmShortS{
		
		select {
		case msg := <-fh.functionOutput:
			fmt.Println("********Received from channel:", msg)
			//将数据返回给用户
			body:="The Result is"+msg+"!!!"
			fh.respondWithSuccess(responseWriter,[]byte(body))
			start := time.Now()
		    go fh.collectFunctionMetric(start, request, code)
			return
		case <-time.After(10 * time.Second):
			fmt.Println("10 seconds Timeout occurred")
			msg:="10 seconds Timeout occurred"
			fh.respondWithError(responseWriter,msg)
			start := time.Now()
			code=404
		    go fh.collectFunctionMetric(start, request, code)
			return
		}
	 }
    
	message:="成功调用异步短函数"
    respons:=[]byte(message)
	fh.respondWithSuccess(responseWriter, respons)
	start := time.Now()
	go fh.collectFunctionMetric(start, request, code)
	return
}


func (fh functionHandler) respondWithSuccess(w http.ResponseWriter, resp []byte) {
	_, err := w.Write(resp)
	if err != nil {
		// this will probably fail too, but try anyway
		errstr:=err.Error()
		fh.respondWithError(w,errstr)
	}
}


func (fh functionHandler) respondWithError(w http.ResponseWriter,resp string) {
	http.Error(w, resp, http.StatusNotFound)
}

func (fh functionHandler) depoDateRedis(uid string, result string) error{
	// 执行 Redis 命令
	err := fh.redisClient.Set(uid, result, 0).Err()
	if err != nil {
		fmt.Println("Failed to set key:", err)
		return err
    }
	return nil
}

//处理funtionOutput回调函数
func (fh functionHandler) functionOutputHandler(w http.ResponseWriter, r *http.Request) {
	fh.logger.Info(r.URL.String())
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Error reading request body", http.StatusInternalServerError)
		return
	}
	// 打印请求体内容
	ans := string(body)
	uid := string(fh.function.UID)
	msg:="key is"+ans+" value is "+uid
	fh.logger.Info(msg)
	if fh.function.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType==fv1.ExecutorTypeWasmShortS{
         fh.functionOutput <-ans
	}
	//将结果存入redis数据库中
	fh.logger.Info("将数据存入数据库中")
	err = fh.depoDateRedis(uid, ans)
	if err != nil {
		fh.logger.Error(err.Error())
		http.Error(w, "fail to deposit data to redis", http.StatusInternalServerError)
	}
	
	w.WriteHeader(http.StatusOK)
}

//改成同步的我们所需要的。先去数据库中查找uid对应有没有结果，有结果直接返回就行。如果没有的话，就部署函数，然后通过一个通道获取函数结果。
func (roundTripper *RetryingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()

	// set the timeout for transport context
	roundTripper.addForwardedHostHeader(req)
	transport := roundTripper.getDefaultTransport()

	executingTimeout := roundTripper.funcHandler.tsRoundTripperParams.timeout

	// wrap the req.Body with another ReadCloser interface.
	if req.Body != nil {
		req.Body = &fakeCloseReadCloser{req.Body}
	}

	// close req body
	defer func() {
		if req.Body != nil {
			err := req.Body.(*fakeCloseReadCloser).RealClose()
			if err != nil {
				roundTripper.logger.Error("Error closing body", zap.Error(err))
			}
		}
	}()

	// The reason for request failure may vary from case to case.
	// After some investigation, found most of the failure are due to
	// network timeout or target function is under heavy workload. In
	// such cases, if router keeps trying to get new function service
	// will increase executor burden and cause 502 error.
	//
	// The "retryCounter" was introduced to solve this problem by retrying
	// requests for "limited threshold". Once a request's retryCounter higher
	// than the predefined threshold, reset retryCounter and remove service
	// cache, then retry to get new svc record from executor again.
	var retryCounter int
	var err error
	var fnMeta = &roundTripper.funcHandler.function.ObjectMeta

	logger := otelUtils.LoggerWithTraceID(ctx, roundTripper.logger).With(zap.String("function", fnMeta.Name), zap.String("namespace", fnMeta.Namespace))

	dumpReqFunc := func(request *http.Request) {
		if request == nil {
			return
		}
		reqMsg, err := httputil.DumpRequest(request, false)
		if err != nil {
			logger.Error("failed to dump request", zap.Error(err))
		} else {
			logger.Debug("round tripper request", zap.String("request", string(reqMsg)))
		}
	}
	dumpRespFunc := func(response *http.Response) {
		if response == nil {
			return
		}
		respMsg, err := httputil.DumpResponse(response, false)
		if err != nil {
			logger.Error("failed to dump response", zap.Error(err))
		} else {
			logger.Debug("round tripper response", zap.String("response", string(respMsg)))
		}
	}

	for i := 0; i < roundTripper.funcHandler.tsRoundTripperParams.maxRetries; i++ {
		// set service url of target service of request only when
		// trying to get new service url from cache/executor.
		if retryCounter == 0 {
			otelUtils.SpanTrackEvent(ctx, "getServiceEntry", otelUtils.MapToAttributes(map[string]string{
				"function-name":      fnMeta.Name,
				"function-namespace": fnMeta.Namespace})...)
			// get function service url from cache or executor
			roundTripper.serviceURL, roundTripper.urlFromCache, err = roundTripper.funcHandler.getServiceEntry(ctx)
			if err != nil {
				// We might want a specific error code or header for fission failures as opposed to
				// user function bugs.
				statusCode, errMsg := ferror.GetHTTPError(err)
				if statusCode == http.StatusTooManyRequests {
					return nil, err
				}
				if roundTripper.funcHandler.isDebugEnv {
					return &http.Response{
						StatusCode:    statusCode,
						Proto:         req.Proto,
						ProtoMajor:    req.ProtoMajor,
						ProtoMinor:    req.ProtoMinor,
						Body:          io.NopCloser(bytes.NewBufferString(errMsg)),
						ContentLength: int64(len(errMsg)),
						Request:       req,
						Header:        make(http.Header),
					}, nil
				}
				return nil, ferror.MakeError(http.StatusInternalServerError, err.Error())
			}
			if roundTripper.serviceURL == nil {
				logger.Warn("serviceURL is empty for function, retrying", zap.Duration("executingTimeout", executingTimeout))
				time.Sleep(executingTimeout)
				executingTimeout = executingTimeout * time.Duration(roundTripper.funcHandler.tsRoundTripperParams.timeoutExponent)
				continue
			}
			otelUtils.SpanTrackEvent(ctx, "serviceEntryReceived", otelUtils.MapToAttributes(map[string]string{
				"function-name":      fnMeta.Name,
				"function-namespace": fnMeta.Namespace,
				"service-entry":      roundTripper.serviceURL.String()})...)
			if roundTripper.funcHandler.function.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType == fv1.ExecutorTypePoolmgr {
				defer func(ctx context.Context, fn *fv1.Function, serviceURL *url.URL) {
					go roundTripper.funcHandler.unTapService(fn, serviceURL) //nolint errcheck
				}(ctx, roundTripper.funcHandler.function, roundTripper.serviceURL)
			}

			// modify the request to reflect the service url
			// this service url comes from executor response
			req.URL.Scheme = roundTripper.serviceURL.Scheme
			req.URL.Host = roundTripper.serviceURL.Host

			// With addition of routing support from functions if function supports routing,
			// 1. we trim prefix url and forward request
			// 2. otherwise we just keep default request to root path
			// We leave the query string intact (req.URL.RawQuery) where as we manipuate
			// req.URL.Path according to httpTrigger specification.
			prefixTrim := ""
			functionURL := utils.UrlForFunction(fnMeta.Name, fnMeta.Namespace)
			keepPrefix := false
			if roundTripper.funcHandler.httpTrigger != nil && roundTripper.funcHandler.httpTrigger.Spec.Prefix != nil && *roundTripper.funcHandler.httpTrigger.Spec.Prefix != "" {
				prefixTrim = *roundTripper.funcHandler.httpTrigger.Spec.Prefix
				keepPrefix = roundTripper.funcHandler.httpTrigger.Spec.KeepPrefix
			} else if strings.HasPrefix(req.URL.Path, functionURL) {
				prefixTrim = functionURL
			}
			if prefixTrim != "" {
				if !keepPrefix {
					req.URL.Path = strings.TrimPrefix(req.URL.Path, prefixTrim)
				}
				if !strings.HasPrefix(req.URL.Path, "/") {
					req.URL.Path = "/" + req.URL.Path
				}
			} else {
				req.URL.Path = "/"
			}

			logger.Debug("function invoke url",
				zap.String("prefixTrim", prefixTrim),
				zap.Bool("keepPrefix", keepPrefix),
				zap.String("hitURL", req.URL.Path))
			// Overwrite request host with internal host,
			// or request will be blocked in some situations
			// (e.g. istio-proxy)
			req.Host = roundTripper.serviceURL.Host
		}

		// over-riding default settings.
		transport.DialContext = (&net.Dialer{
			Timeout:   executingTimeout,
			KeepAlive: roundTripper.funcHandler.tsRoundTripperParams.keepAliveTime,
		}).DialContext

		// Do NOT assign returned request to "req"
		// because the request used in the last round
		// will be canceled when calling setContext.
		newReq := roundTripper.setContext(req)

		if roundTripper.funcHandler.isDebugEnv {
			dumpReqFunc(newReq)
		}

		// forward the request to the function service
		otelUtils.SpanTrackEvent(ctx, "roundtrip", otelUtils.MapToAttributes(map[string]string{
			"function-name":      fnMeta.Name,
			"function-namespace": fnMeta.Namespace,
			"function-url":       newReq.URL.String(),
			"retryCounter":       fmt.Sprintf("%d", retryCounter)})...)
		otelRoundTripper := otelhttp.NewTransport(transport)
		resp, err := otelRoundTripper.RoundTrip(newReq)
		if roundTripper.funcHandler.isDebugEnv {
			dumpRespFunc(resp)
		}
		if err == nil {
			// return response back to user
			return resp, nil
		}

		roundTripper.totalRetry++

		if i >= roundTripper.funcHandler.tsRoundTripperParams.maxRetries-1 {
			// return here if we are in the last round
			logger.Error("error getting response from function",
				zap.Error(err))
			return nil, err
		}

		// if transport.RoundTrip returns a non-network dial error, then relay it back to user
		netErr := network.Adapter(err)

		// dial timeout or dial network errors goes here
		var isNetDialErr, isNetTimeoutErr bool
		if netErr != nil {
			isNetDialErr = netErr.IsDialError()
			isNetTimeoutErr = netErr.IsTimeoutError()
		}

		// if transport.RoundTrip returns a non-network dial error (e.g. "context canceled"), then relay it back to user
		if !isNetDialErr {
			logger.Error("encountered non-network dial error", zap.Error(err))
			return resp, err
		}

		// close response body before entering next loop
		if resp != nil {
			resp.Body.Close()
		}

		// Check whether an error is an timeout error ("dial tcp i/o timeout").
		if isNetTimeoutErr {
			logger.Debug("request errored out - backing off before retrying",
				zap.String("url", req.URL.Host),
				zap.Error(err))
			retryCounter++
		}

		// If it's not a timeout error or retryCounter exceeded pre-defined threshold,
		if retryCounter >= roundTripper.funcHandler.tsRoundTripperParams.svcAddrRetryCount {
			logger.Debug(fmt.Sprintf(
				"retry counter exceeded pre-defined threshold of %v",
				roundTripper.funcHandler.tsRoundTripperParams.svcAddrRetryCount))
			if roundTripper.urlFromCache {
				roundTripper.funcHandler.removeServiceEntryFromCache()
			}
			retryCounter = 0
		}

		logger.Debug("Backing off before retrying", zap.Duration("backoff_time", executingTimeout), zap.Error(err))
		time.Sleep(executingTimeout)
		executingTimeout = executingTimeout * time.Duration(roundTripper.funcHandler.tsRoundTripperParams.timeoutExponent)
	}

	e := errors.New("Unable to get service url for connection")
	logger.Error(e.Error())
	return nil, e
}

// getDefaultTransport returns a pointer to new copy of http.Transport object to prevent
// the value of http.DefaultTransport from being changed by goroutines.
func (roundTripper RetryingRoundTripper) getDefaultTransport() *http.Transport {
	// The transport setup here follows the configurations of http.DefaultTransport
	// but without Dialer since we will change it later.
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// Default disables caching, Please refer to issue and specifically comment:
		// https://github.com/fission/fission/issues/723#issuecomment-398781995
		// You can change it by setting environment variable "ROUTER_ROUND_TRIP_DISABLE_KEEP_ALIVE"
		// of router or helm variable "disableKeepAlive" before installation to false.
		DisableKeepAlives: roundTripper.funcHandler.tsRoundTripperParams.disableKeepAlive,
	}
}

// setContext returns a shallow copy of request with a new timeout context.
func (roundTripper *RetryingRoundTripper) setContext(req *http.Request) *http.Request {
	if roundTripper.closeContextFunc != nil {
		(*roundTripper.closeContextFunc)()
	}
	// pass request context as parent context for the case
	// that user aborts connection before timeout. Otherwise,
	// the request won't be canceled until the deadline exceeded
	// which may be a potential security issue.
	ctx, closeCtx := context.WithTimeout(req.Context(), roundTripper.funcTimeout)
	roundTripper.closeContextFunc = &closeCtx

	return req.WithContext(ctx)
}

// closeContext closes the context to release resources.
func (roundTripper *RetryingRoundTripper) closeContext() {
	if roundTripper.closeContextFunc != nil {
		(*roundTripper.closeContextFunc)()
	}
}

func (fh *functionHandler) tapService(fn *fv1.Function, serviceURL *url.URL) {
	if fh.executor == nil || serviceURL == nil {
		return
	}
	fh.executor.TapService(fn.ObjectMeta, fn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType, *serviceURL)
}




// findCeil picks a function from the functionWeightDistribution list based on the
// random number generated. It uses the prefix calculated for the function weights.
func findCeil(randomNumber int, wtDistrList []functionWeightDistribution) string {
	low := 0
	high := len(wtDistrList) - 1

	for {
		if low >= high {
			break
		}

		mid := low + high/2
		if randomNumber >= wtDistrList[mid].sumPrefix {
			low = mid + 1
		} else {
			high = mid
		}
	}

	if wtDistrList[low].sumPrefix >= randomNumber {
		return wtDistrList[low].name
	}
	return ""
}

// picks a function to route to based on a random number generated
func getCanaryBackend(fnMap map[string]*fv1.Function, fnWtDistributionList []functionWeightDistribution) *fv1.Function {
	randomNumber := rand.Intn(fnWtDistributionList[len(fnWtDistributionList)-1].sumPrefix + 1)
	fnName := findCeil(randomNumber, fnWtDistributionList)
	return fnMap[fnName]
}

// addForwardedHostHeader add "forwarded host" to request header
func (roundTripper RetryingRoundTripper) addForwardedHostHeader(req *http.Request) {
	// for more detailed information, please visit:
	// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Forwarded

	if len(req.Header.Get(FORWARDED)) > 0 || len(req.Header.Get(X_FORWARDED_HOST)) > 0 {
		// forwarded headers were set by external proxy, leave them intact
		return
	}

	// Format of req.Host is <host>:<port>
	// We need to extract hostname from it, than
	// check whether a host is ipv4 or ipv6 or FQDN
	reqURL := fmt.Sprintf("%s://%s", req.Proto, req.Host)
	u, err := url.Parse(reqURL)
	if err != nil {
		roundTripper.logger.Error("error parsing request url while adding forwarded host headers",
			zap.Error(err),
			zap.String("url", reqURL))
		return
	}

	var host string

	// ip will be nil if the Hostname is a FQDN string
	ip := net.ParseIP(u.Hostname())

	// ip == nil -> hostname is FQDN instead of ip address
	// The order of To4() and To16() here matters, To16() will
	// converts an IPv4 address to IPv6 format address and may
	// cause router append wrong host value to header. To prevent
	// this we need to check whether To4() is nil first.
	if ip == nil || (ip != nil && ip.To4() != nil) {
		host = fmt.Sprintf(`host=%s;`, req.Host)
	} else if ip != nil && ip.To16() != nil {
		// For the "Forwarded" header, if a host is an IPv6 address it should be quoted
		host = fmt.Sprintf(`host="%s";`, req.Host)
	}

	req.Header.Set(FORWARDED, host)
	req.Header.Set(X_FORWARDED_HOST, req.Host)
}

// unTapservice marks the serviceURL in executor's cache as inactive, so that it can be reused
func (fh functionHandler) unTapService(fn *fv1.Function, serviceUrl *url.URL) error {
	fh.logger.Debug("UnTapService Called")
	ctx, cancel := context.WithTimeout(context.Background(), fh.unTapServiceTimeout)
	defer cancel()
	err := fh.executor.UnTapService(ctx, fn.ObjectMeta, fn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType, serviceUrl)
	if err != nil {
		statusCode, errMsg := ferror.GetHTTPError(err)
		fh.logger.Error("error from UnTapService",
			zap.Error(err),
			zap.String("error_message", errMsg),
			zap.Any("function", fh.function),
			zap.Int("status_code", statusCode))
		return err
	}
	return nil
}

// getServiceEntryFromCache returns service url entry returns from cache
func (fh functionHandler) getServiceEntryFromCache() (serviceUrl *url.URL, err error) {
	// // cache lookup to get serviceUrl
	// serviceUrl, err = fh.fmap.lookup(&fh.function.ObjectMeta)
	// if err != nil {
	// 	var errMsg string

	// 	e, ok := err.(ferror.Error)
	// 	if !ok {
	// 		errMsg = fmt.Sprintf("Unknown error when looking up service entry: %v", err)
	// 	} else {
	// 		// Ignore ErrorNotFound error here, it's an expected error,
	// 		// roundTripper will try to get service url later.
	// 		if e.Code == ferror.ErrorNotFound {
	// 			return nil, nil
	// 		}
	// 		errMsg = fmt.Sprintf("Error getting function %v;s service entry from cache: %v", fh.function.ObjectMeta.Name, err)
	// 	}
	// 	return nil, ferror.MakeError(http.StatusInternalServerError, errMsg)
	// }
	return serviceUrl, nil
}

// addServiceEntryToCache add service url entry to cache
func (fh functionHandler) addServiceEntryToCache(serviceURL *url.URL) {
// 	fh.fmap.assign(&fh.function.ObjectMeta, serviceURL)
}

// removeServiceEntryFromCache removes service url entry from cache
func (fh functionHandler) removeServiceEntryFromCache() {
	// err := fh.fmap.remove(&fh.function.ObjectMeta)
	// if err != nil {
	// 	fh.logger.Error("Error removing key:", zap.Error(err))
	// }
}

func (fh functionHandler) getServiceEntryFromExecutor(ctx context.Context) (serviceUrl *url.URL, err error) {
	logger := otelUtils.LoggerWithTraceID(ctx, fh.logger)
	// send a request to executor to specialize a new pod
	fh.logger.Debug("function timeout specified", zap.Int("timeout", fh.function.Spec.FunctionTimeout))

	var fContext context.Context
	if fh.function.Spec.FunctionTimeout > 0 {
		timeout := time.Second * time.Duration(fh.function.Spec.FunctionTimeout)
		f, cancel := context.WithTimeout(ctx, timeout)
		fContext = f
		defer cancel()
	} else {
		fContext = ctx
	}

	service, err := fh.executor.GetServiceForFunction(fContext, fh.function)
	if err != nil {
		statusCode, errMsg := ferror.GetHTTPError(err)
		logger.Error("error from GetServiceForFunction",
			zap.Error(err),
			zap.String("error_message", errMsg),
			zap.Any("function", fh.function),
			zap.Int("status_code", statusCode))
		return nil, err
	}
	// parse the address into url
	svcURL, err := url.Parse(fmt.Sprintf("http://%v", service))
	if err != nil {
		logger.Error("error parsing service url",
			zap.Error(err),
			zap.String("service_url", svcURL.String()))
		return nil, err
	}
	return svcURL, err
}

// getServiceEntryFromExecutor returns service url entry returns from executor
func (fh functionHandler) getServiceEntry(ctx context.Context) (svcURL *url.URL, cacheHit bool, err error) {
	if fh.function.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType == fv1.ExecutorTypePoolmgr {
		svcURL, err = fh.getServiceEntryFromExecutor(ctx)
		return svcURL, false, err
	}
	// Check if service URL present in cache
	svcURL, err = fh.getServiceEntryFromCache()
	if err == nil && svcURL != nil {
		return svcURL, true, nil
	} else if err != nil {
		return nil, false, err
	}

	fnMeta := &fh.function.ObjectMeta
	recordObj, err := fh.svcAddrUpdateThrottler.RunOnce(
		crd.CacheKey(fnMeta),
		func(firstToTheLock bool) (interface{}, error) {
			if !firstToTheLock {
				svcURL, err := fh.getServiceEntryFromCache()
				if err != nil {
					return nil, err
				}
				return svcEntryRecord{svcURL: svcURL, cacheHit: true}, err
			}
			svcURL, err = fh.getServiceEntryFromExecutor(ctx)
			if err != nil {
				return nil, err
			}
			fh.addServiceEntryToCache(svcURL)
			return svcEntryRecord{
				svcURL:   svcURL,
				cacheHit: false,
			}, nil
		},
	)

	record, ok := recordObj.(svcEntryRecord)
	if !ok {
		return nil, false, fmt.Errorf("unexpected type of recordObj %T: %w", recordObj, err)
	}
	return record.svcURL, record.cacheHit, err
}

// getProxyErrorHandler returns a reverse proxy error handler
func (fh functionHandler) getProxyErrorHandler(start time.Time, rrt *RetryingRoundTripper) func(rw http.ResponseWriter, req *http.Request, err error) {
	return func(rw http.ResponseWriter, req *http.Request, err error) {
		var status int
		var msg string
		ctx := req.Context()
		logger := otelUtils.LoggerWithTraceID(ctx, fh.logger)
		switch err {
		case context.Canceled:
			// 499 CLIENT CLOSED REQUEST
			// A non-standard status code introduced by nginx for the case
			// when a client closes the connection while nginx is processing the request.
			// Reference: https://httpstatuses.com/499
			status = 499
			msg = "client closes the connection"
			logger.Debug(msg, zap.Any("function", fh.function), zap.String("status", "Client Closed Request"))
		case context.DeadlineExceeded:
			status = http.StatusGatewayTimeout
			msg := "no response from function before timeout"
			logger.Error(msg, zap.Any("function", fh.function), zap.String("status", http.StatusText(status)))
		default:
			code, _ := ferror.GetHTTPError(err)
			status = code
			msg = "error sending request to function"
			logger.Error(msg, zap.Error(err), zap.Any("function", fh.function),
				zap.Any("status", http.StatusText(status)), zap.Int("code", code))
		}

		// go fh.collectFunctionMetric(start, rrt, req, &http.Response{
		// 	StatusCode:    status,
		// 	ContentLength: req.ContentLength,
		// })

		// TODO: return error message that contains traceable UUID back to user. Issue #693
		rw.WriteHeader(status)
		_, err = rw.Write([]byte(msg))
		if err != nil {
			logger.Error(
				"error writing HTTP response",
				zap.Error(err),
				zap.Any("function", fh.function),
			)
		}
	}
}

func (fh functionHandler) collectFunctionMetric(start time.Time, req *http.Request,code int) {
	duration := time.Since(start)
	var path string

	if fh.httpTrigger != nil {
		if fh.httpTrigger.Spec.Prefix != nil && *fh.httpTrigger.Spec.Prefix != "" {
			path = *fh.httpTrigger.Spec.Prefix
		} else {
			path = fh.httpTrigger.Spec.RelativeURL
		}
	}

	wasmfunctionCalls.WithLabelValues(fh.function.ObjectMeta.Namespace,
		fh.function.ObjectMeta.Name, path, req.Method,
		fmt.Sprint(code)).Inc()

	if code >= 400 {
		wasmfunctionCallErrors.WithLabelValues(fh.function.ObjectMeta.Namespace,
			fh.function.ObjectMeta.Name, path, req.Method,
			fmt.Sprint(code)).Inc()
	}

	wasmfunctionCallOverhead.WithLabelValues(fh.function.ObjectMeta.Namespace,
		fh.function.ObjectMeta.Name, path, req.Method,
		fmt.Sprint(code)).
		Observe(float64(duration.Nanoseconds()) / 1e9)

	fh.logger.Debug("Request complete", zap.String("function", fh.function.ObjectMeta.Name),zap.Duration("total-time", duration))
}
