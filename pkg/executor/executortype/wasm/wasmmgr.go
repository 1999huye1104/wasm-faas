package wasm

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	multierror "github.com/hashicorp/go-multierror"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	apiv1 "k8s.io/api/core/v1"
	k8sErrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8sTypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	appsinformers "k8s.io/client-go/informers/apps/v1"
	coreinformers "k8s.io/client-go/informers/core/v1"
	batchinformers "k8s.io/client-go/informers/batch/v1"
	"k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	batchlisters "k8s.io/client-go/listers/batch/v1"
	k8sCache "k8s.io/client-go/tools/cache"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
	"github.com/fission/fission/pkg/executor/executortype"
	"github.com/fission/fission/pkg/executor/fscache"
	"github.com/fission/fission/pkg/executor/metrics"
	"github.com/fission/fission/pkg/executor/reaper"
	hpautils "github.com/fission/fission/pkg/executor/util/hpa"
	"github.com/fission/fission/pkg/generated/clientset/versioned"
	finformerv1 "github.com/fission/fission/pkg/generated/informers/externalversions/core/v1"
	"github.com/fission/fission/pkg/throttler"
	"github.com/fission/fission/pkg/utils"
	"github.com/fission/fission/pkg/utils/maps"
	otelUtils "github.com/fission/fission/pkg/utils/otel"
)

var (
	_ executortype.ExecutorType = &Wasm{}
)


type (
	// Wasm represents an ExecutorType
	Wasm struct {
		logger *zap.Logger

		kubernetesClient kubernetes.Interface
		fissionClient    versioned.Interface
		instanceID       string
		// fetcherConfig    *fetcherConfig.Config

		runtimeImagePullPolicy apiv1.PullPolicy
		namespace              string
		useIstio               bool

		fsCache *fscache.FunctionServiceCache // cache funcSvc's by function, address and pod name

		throttler *throttler.Throttler

		defaultIdlePodReapTime time.Duration

		jobLister batchlisters.JobLister
		svcLister  corelisters.ServiceLister
		deplLister appslisters.DeploymentLister

		jobListerSynced k8sCache.InformerSynced
		svcListerSynced  k8sCache.InformerSynced
		deplListerSynced k8sCache.InformerSynced

		hpaops *hpautils.HpaOperations
	}
)

// MakeWasm initializes and returns an instance of wasm
func MakeWasm(
	ctx context.Context,
	logger *zap.Logger,
	fissionClient versioned.Interface,
	kubernetesClient kubernetes.Interface,
	namespace string,
	instanceID string,
	funcInformer finformerv1.FunctionInformer,
	jobInformer batchinformers.JobInformer,
	deplInformer appsinformers.DeploymentInformer,
	svcInformer coreinformers.ServiceInformer,
) (executortype.ExecutorType, error) {
	enableIstio := false
	if len(os.Getenv("ENABLE_ISTIO")) > 0 {
		istio, err := strconv.ParseBool(os.Getenv("ENABLE_ISTIO"))
		if err != nil {
			logger.Error("failed to parse 'ENABLE_ISTIO', set to false", zap.Error(err))
		}
		enableIstio = istio
	}

	wasm := &Wasm{
		logger: logger.Named("Wasm"),

		fissionClient:    fissionClient,
		kubernetesClient: kubernetesClient,
		instanceID:       instanceID,

		namespace: namespace,
		fsCache:   fscache.MakeFunctionServiceCache(logger),
		throttler: throttler.MakeThrottler(1 * time.Minute),

		runtimeImagePullPolicy: utils.GetImagePullPolicy(os.Getenv("RUNTIME_IMAGE_PULL_POLICY")),
		useIstio:               enableIstio,
		// Time is set slightly higher than NewDeploy as cold starts are longer for wasm
		defaultIdlePodReapTime: 1 * time.Minute,

		hpaops: hpautils.NewHpaOperations(logger, kubernetesClient, instanceID),
	}
	wasm.jobLister = jobInformer.Lister()
	wasm.jobListerSynced = jobInformer.Informer().HasSynced
    
    wasm.deplLister=deplInformer.Lister()
	wasm.deplListerSynced=deplInformer.Informer().HasSynced

	wasm.svcLister = svcInformer.Lister()
	wasm.svcListerSynced = svcInformer.Informer().HasSynced

	funcInformer.Informer().AddEventHandler(wasm.FuncInformerHandler(ctx))
	return wasm, nil
}

// Run start the function along with an object reaper.
func (wasm *Wasm) Run(ctx context.Context) {
	if ok := k8sCache.WaitForCacheSync(ctx.Done(), wasm.jobListerSynced,wasm.deplListerSynced,wasm.svcListerSynced); !ok {
		wasm.logger.Fatal("failed to wait for caches to sync")
	}
	go wasm.idleObjectReaper(ctx)
}

// GetTypeName returns the executor type name.
func (wasm *Wasm) GetTypeName(ctx context.Context) fv1.ExecutorType {
	return fv1.ExecutorTypeWasm
}

// GetTotalAvailable has not been implemented for wasm.
func (wasm *Wasm) GetTotalAvailable(fn *fv1.Function) int {
	// Not Implemented for wasm.
	return 0
}

// UnTapService has not been implemented for wasm.
func (wasm *Wasm) UnTapService(ctx context.Context, key string, svcHost string) {
	// Not Implemented for wasm.
}

// GetFuncSvc returns a function service; error otherwise.
func (wasm *Wasm) GetFuncSvc(ctx context.Context, fn *fv1.Function) (*fscache.FuncSvc, error) {
	if fn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType==fv1.ExecutorTypeWasm{
		return wasm.createFunction(ctx, fn)
	}else{
        return wasm.createShortFunction(ctx, fn)
	}
	
}

// GetFuncSvcFromCache returns a function service from cache; error otherwise.
func (wasm *Wasm) GetFuncSvcFromCache(ctx context.Context, fn *fv1.Function) (*fscache.FuncSvc, error) {
	otelUtils.SpanTrackEvent(ctx, "GetFuncSvcFromCache", otelUtils.GetAttributesForFunction(fn)...)
	return wasm.fsCache.GetByFunctionUID(fn.UID)
}

// DeleteFuncSvcFromCache deletes a function service from cache.
func (wasm *Wasm) DeleteFuncSvcFromCache(ctx context.Context, fsvc *fscache.FuncSvc) {
	wasm.fsCache.DeleteEntry(fsvc)
}

// GetFuncSvcFromPoolCache has not been implemented for Wasm Functions
func (wasm *Wasm) GetFuncSvcFromPoolCache(ctx context.Context, fn *fv1.Function, requestsPerPod int) (*fscache.FuncSvc, int, error) {
	// Not Implemented for NewDeployment. Will be used when support of concurrent specialization of same function is added.
	return nil, 0, nil
}

// TapService makes a TouchByAddress request to the cache.
func (wasm *Wasm) TapService(ctx context.Context, svcHost string) error {
	otelUtils.SpanTrackEvent(ctx, "TapService")
	err := wasm.fsCache.TouchByAddress(svcHost)
	if err != nil {
		return err
	}
	return nil
}

// IsValid does a get on the service address to ensure it's a valid service, then
// scale deployment to 1 replica if there are no available replicas for function.
// Return true if no error occurs, return false otherwise.
func (wasm *Wasm) IsValid(ctx context.Context, fsvc *fscache.FuncSvc) bool {
	logger := otelUtils.LoggerWithTraceID(ctx, wasm.logger)
	otelUtils.SpanTrackEvent(ctx, "IsValid", fscache.GetAttributesForFuncSvc(fsvc)...)
	if len(strings.Split(fsvc.Address, ".")) == 0 {
		logger.Error("address not found in function service")
		return false
	}
	if len(fsvc.KubernetesObjects) == 0 {
		logger.Error("no kubernetes object related to function", zap.String("function", fsvc.Function.Name))
		return false
	}
	for _, obj := range fsvc.KubernetesObjects {
		if strings.ToLower(obj.Kind) == "service" {
			_, err := wasm.svcLister.Services(obj.Namespace).Get(obj.Name)
			if err != nil {
				if !k8sErrs.IsNotFound(err) {
					logger.Error("error validating function service", zap.String("function", fsvc.Function.Name), zap.Error(err))
				}
				return false
			}
		} else if strings.ToLower(obj.Kind) == "deployment" {
			currentDeploy, err := wasm.deplLister.Deployments(obj.Namespace).Get(obj.Name)
			if err != nil {
				if !k8sErrs.IsNotFound(err) {
					logger.Error("error validating function deployment", zap.String("function", fsvc.Function.Name), zap.Error(err))
				}
				return false
			}
			if currentDeploy.Status.AvailableReplicas < 1 {
				return false
			}
		}else if strings.ToLower(obj.Kind) == "job"{
			_, err := wasm.jobLister.Jobs(obj.Namespace).Get(obj.Name)
			if err != nil {
				if !k8sErrs.IsNotFound(err) {
					logger.Error("error validating function job", zap.String("function", fsvc.Function.Name), zap.Error(err))
				}
				return false
			}
		}
	}
	return true
}

// RefreshFuncPods deletes pods related to the function so that new pods are replenished
func (wasm *Wasm) RefreshFuncPods(ctx context.Context, logger *zap.Logger, f fv1.Function) error {

	funcLabels := wasm.getDeployLabels(f.ObjectMeta)

	dep, err := wasm.kubernetesClient.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		LabelSelector: labels.Set(funcLabels).AsSelector().String(),
	})
	if err != nil {
		return err
	}

	// Ideally there should be only one deployment but for now we rely on label/selector to ensure that condition
	for _, deployment := range dep.Items {
		rvCount, err := referencedResourcesRVSum(ctx, wasm.kubernetesClient, deployment.Namespace, f.Spec.Secrets, f.Spec.ConfigMaps)
		if err != nil {
			return err
		}

		patch := fmt.Sprintf(`{"spec" : {"template": {"spec":{"Wasms":[{"name": "%s", "env":[{"name": "%s", "value": "%v"}]}]}}}}`,
			f.ObjectMeta.Name, fv1.ResourceVersionCount, rvCount)

		_, err = wasm.kubernetesClient.AppsV1().Deployments(deployment.ObjectMeta.Namespace).Patch(ctx, deployment.ObjectMeta.Name,
			k8sTypes.StrategicMergePatchType,
			[]byte(patch), metav1.PatchOptions{})
		if err != nil {
			return err
		}
	}
	return nil
}

// AdoptExistingResources attempts to adopt resources for functions in all namespaces.
func (wasm *Wasm) AdoptExistingResources(ctx context.Context) {
	fnList, err := wasm.fissionClient.CoreV1().Functions(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		wasm.logger.Error("error getting function list", zap.Error(err))
		return
	}

	wg := &sync.WaitGroup{}

	for i := range fnList.Items {
		fn := &fnList.Items[i]
		if fn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType == fv1.ExecutorTypeWasm ||fn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType == fv1.ExecutorTypeWasmShortA||fn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType == fv1.ExecutorTypeWasmShortS{
			wg.Add(1)
			go func() {
				defer wg.Done()

				_, err = wasm.fnCreate(ctx, fn)
				if err != nil {
					wasm.logger.Warn("failed to adopt resources for function", zap.Error(err))
					return
				}
				wasm.logger.Info("adopt resources for function", zap.String("function", fn.ObjectMeta.Name))
			}()
		}
	}

	wg.Wait()
}

// CleanupOldExecutorObjects cleans orphaned resources.
func (wasm *Wasm) CleanupOldExecutorObjects(ctx context.Context) {
	wasm.logger.Info("wasm starts to clean orphaned resources", zap.String("instanceID", wasm.instanceID))

	errs := &multierror.Error{}
	listOpts := metav1.ListOptions{
		LabelSelector: labels.Set(map[string]string{fv1.EXECUTOR_TYPE: string(fv1.ExecutorTypeWasm)}).AsSelector().String(),
	}

	err := reaper.CleanupHpa(ctx, wasm.logger, wasm.kubernetesClient, wasm.instanceID, listOpts)
	if err != nil {
		errs = multierror.Append(errs, err)
	}

	err = reaper.CleanupDeployments(ctx, wasm.logger, wasm.kubernetesClient, wasm.instanceID, listOpts)
	if err != nil {
		errs = multierror.Append(errs, err)
	}

	err = reaper.CleanupServices(ctx, wasm.logger, wasm.kubernetesClient, wasm.instanceID, listOpts)
	if err != nil {
		errs = multierror.Append(errs, err)
	}

	if errs.ErrorOrNil() != nil {
		// TODO retry reaper; logged and ignored for now
		wasm.logger.Error("Failed to cleanup old executor objects", zap.Error(err))
	}
}

//部署短函数
func (wasm *Wasm) createShortFunction(ctx context.Context, fn *fv1.Function) (*fscache.FuncSvc, error) {
	if fn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType != fv1.ExecutorTypeWasmShortA && fn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType != fv1.ExecutorTypeWasmShortS{
		return nil, nil
	}
	fsvcObj, err := wasm.throttler.RunOnce(string(fn.ObjectMeta.UID), func(ableToCreate bool) (interface{}, error) {
		if ableToCreate {
			return wasm.fnShortCreate(ctx, fn)
		}
		return wasm.fsCache.GetByFunctionUID(fn.ObjectMeta.UID)
	})
	if err != nil {
		e := "error creating k8s resources for function"
		wasm.logger.Error(e,
			zap.Error(err),
			zap.String("function_name", fn.ObjectMeta.Name),
			zap.String("function_namespace", fn.ObjectMeta.Namespace))
		return nil, errors.Wrapf(err, "%s %s_%s", e, fn.ObjectMeta.Name, fn.ObjectMeta.Namespace)
	}

	fsvc, ok := fsvcObj.(*fscache.FuncSvc)
	if !ok {
		wasm.logger.Panic("receive unknown object while creating function - expected pointer of function service object")
	}

	return fsvc, err

}


func (wasm *Wasm) createFunction(ctx context.Context, fn *fv1.Function) (*fscache.FuncSvc, error) {
	if fn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType != fv1.ExecutorTypeWasm {
		return nil, nil
	}

	fsvcObj, err := wasm.throttler.RunOnce(string(fn.ObjectMeta.UID), func(ableToCreate bool) (interface{}, error) {
		if ableToCreate {
			return wasm.fnCreate(ctx, fn)
		}
		return wasm.fsCache.GetByFunctionUID(fn.ObjectMeta.UID)
	})
	if err != nil {
		e := "error creating k8s resources for function"
		wasm.logger.Error(e,
			zap.Error(err),
			zap.String("function_name", fn.ObjectMeta.Name),
			zap.String("function_namespace", fn.ObjectMeta.Namespace))
		return nil, errors.Wrapf(err, "%s %s_%s", e, fn.ObjectMeta.Name, fn.ObjectMeta.Namespace)
	}

	fsvc, ok := fsvcObj.(*fscache.FuncSvc)
	if !ok {
		wasm.logger.Panic("receive unknown object while creating function - expected pointer of function service object")
	}

	return fsvc, err
}

func (wasm *Wasm) deleteFunction(ctx context.Context, fn *fv1.Function) error {
	if fn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType != fv1.ExecutorTypeWasm {
		return nil
	}
	err := wasm.fnDelete(ctx, fn)
	if err != nil {
		err = errors.Wrapf(err, "error deleting kubernetes objects of function %v", fn.ObjectMeta)
	}
	return err
}

func (wasm *Wasm) deleteShortFunction(ctx context.Context, fn *fv1.Function) error {
	if fn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType != fv1.ExecutorTypeWasmShortA&& fn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType != fv1.ExecutorTypeWasmShortS{
		return nil
	}
	err := wasm.fnShortDelete(ctx, fn)
	if err != nil {
		err = errors.Wrapf(err, "error deleting kubernetes objects of function %v", fn.ObjectMeta)
	}
	return err
}


func (wasm *Wasm) fnShortCreate(ctx context.Context, fn *fv1.Function) (*fscache.FuncSvc, error) {
	cleanupFunc := func(ns string, name string) {
		err := wasm.cleanupShortWasm(ctx, ns, name)
		if err != nil {
			wasm.logger.Error("received error while cleaning function resources",
				zap.String("namespace", ns), zap.String("name", name))
		}
	}
	objName := wasm.getObjName(fn)
	deployLabels := wasm.getDeployLabels(fn.ObjectMeta)
	deployAnnotations := wasm.getDeployAnnotations(fn.ObjectMeta)

	ns := wasm.namespace
	if fn.ObjectMeta.Namespace != metav1.NamespaceDefault {
		ns = fn.ObjectMeta.Namespace
	}
    //通过job来部署
	job, err := wasm.createOrGetJob(ctx, fn, deployLabels, deployAnnotations, objName, ns)
	if err != nil {
		wasm.logger.Error("error creating job", zap.Error(err), zap.String("job", objName))
		go cleanupFunc(ns, objName)
		return nil, errors.Wrapf(err, "error creating job %v", objName)
	}
	svcAddress := fmt.Sprintf("%v.%v", job.Name, job.Namespace)


	kubeObjRefs := []apiv1.ObjectReference{
		
		{
			Kind:            "job",
			Name:            job.ObjectMeta.Name,
			APIVersion:      job.TypeMeta.APIVersion,
			Namespace:       job.ObjectMeta.Namespace,
			ResourceVersion: job.ObjectMeta.ResourceVersion,
			UID:             job.ObjectMeta.UID,
		},
	}

	fsvc := &fscache.FuncSvc{
		Name:              objName,
		Function:          &fn.ObjectMeta,
		Address:           svcAddress,
		KubernetesObjects: kubeObjRefs,
		Executor:          fv1.ExecutorTypeWasm,
	}

	_, err = wasm.fsCache.Add(*fsvc)
	if err != nil {
		wasm.logger.Error("error adding function to cache", zap.Error(err), zap.Any("function", fsvc.Function))
		metrics.FuncError.WithLabelValues(fn.ObjectMeta.Name, fn.ObjectMeta.Namespace).Inc()
		return fsvc, err
	}

	metrics.ColdStarts.WithLabelValues(fn.ObjectMeta.Name, fn.ObjectMeta.Namespace).Inc()

	return fsvc, nil
}


func (wasm *Wasm) fnCreate(ctx context.Context, fn *fv1.Function) (*fscache.FuncSvc, error) {
	cleanupFunc := func(ns string, name string) {
		err := wasm.cleanupWasm(ctx, ns, name)
		if err != nil {
			wasm.logger.Error("received error while cleaning function resources",
				zap.String("namespace", ns), zap.String("name", name))
		}
	}
	objName := wasm.getObjName(fn)
	deployLabels := wasm.getDeployLabels(fn.ObjectMeta)
	deployAnnotations := wasm.getDeployAnnotations(fn.ObjectMeta)

	// to support backward compatibility, if the function was created in default ns, we fall back to creating the
	// deployment of the function in fission-function ns
	ns := wasm.namespace
	if fn.ObjectMeta.Namespace != metav1.NamespaceDefault {
		ns = fn.ObjectMeta.Namespace
	}

	// Envoy(istio-proxy) returns 404 directly before istio pilot
	// propagates latest Envoy-specific configuration.
	// Since Wasm waits for pods of deployment to be ready,
	// change the order of kubeObject creation (create service first,
	// then deployment) to take advantage of waiting time.
	svc, err := wasm.createOrGetSvc(ctx, fn, deployLabels, deployAnnotations, objName, ns)
	if err != nil {
		wasm.logger.Error("error creating service", zap.Error(err), zap.String("service", objName))
		go cleanupFunc(ns, objName)
		return nil, errors.Wrapf(err, "error creating service %v", objName)
	}
	svcAddress := fmt.Sprintf("%v.%v", svc.Name, svc.Namespace)
	
	depl, err := wasm.createOrGetDeployment(ctx, fn, objName, deployLabels, deployAnnotations, ns)
	if err != nil {
		wasm.logger.Error("error creating deployment", zap.Error(err), zap.String("deployment", objName))
		go cleanupFunc(ns, objName)
		return nil, errors.Wrapf(err, "error creating deployment %v", objName)
	}

	// hpa, err := wasm.hpaops.CreateOrGetHpa(ctx, objName, &fn.Spec.InvokeStrategy.ExecutionStrategy, depl, deployLabels, deployAnnotations)
	// if err != nil {
	// 	wasm.logger.Error("error creating HPA", zap.Error(err), zap.String("hpa", objName))
	// 	go cleanupFunc(ns, objName)
	// 	return nil, errors.Wrapf(err, "error creating the HPA %v", objName)
	// }

	kubeObjRefs := []apiv1.ObjectReference{
		{
			//obj.TypeMeta.Kind does not work hence this, needs investigation and a fix
			Kind:            "deployment",
			Name:            depl.ObjectMeta.Name,
			APIVersion:      depl.TypeMeta.APIVersion,
			Namespace:       depl.ObjectMeta.Namespace,
			ResourceVersion: depl.ObjectMeta.ResourceVersion,
			UID:             depl.ObjectMeta.UID,
		},
		{
			Kind:            "service",
			Name:            svc.ObjectMeta.Name,
			APIVersion:      svc.TypeMeta.APIVersion,
			Namespace:       svc.ObjectMeta.Namespace,
			ResourceVersion: svc.ObjectMeta.ResourceVersion,
			UID:             svc.ObjectMeta.UID,
		},
		// {
		// 	Kind:            "horizontalpodautoscaler",
		// 	Name:            hpa.ObjectMeta.Name,
		// 	APIVersion:      hpa.TypeMeta.APIVersion,
		// 	Namespace:       hpa.ObjectMeta.Namespace,
		// 	ResourceVersion: hpa.ObjectMeta.ResourceVersion,
		// 	UID:             hpa.ObjectMeta.UID,
		// },
	}

	fsvc := &fscache.FuncSvc{
		Name:              objName,
		Function:          &fn.ObjectMeta,
		Address:           svcAddress,
		KubernetesObjects: kubeObjRefs,
		Executor:          fv1.ExecutorTypeWasm,
	}

	_, err = wasm.fsCache.Add(*fsvc)
	if err != nil {
		wasm.logger.Error("error adding function to cache", zap.Error(err), zap.Any("function", fsvc.Function))
		metrics.FuncError.WithLabelValues(fn.ObjectMeta.Name, fn.ObjectMeta.Namespace).Inc()
		return fsvc, err
	}

	metrics.ColdStarts.WithLabelValues(fn.ObjectMeta.Name, fn.ObjectMeta.Namespace).Inc()

	return fsvc, nil
}

func (wasm *Wasm) updateFunction(ctx context.Context, oldFn *fv1.Function, newFn *fv1.Function) error {

	if oldFn.ObjectMeta.ResourceVersion == newFn.ObjectMeta.ResourceVersion {
		return nil
	}

	// Ignoring updates to functions which are not of Wasm type
	if newFn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType != fv1.ExecutorTypeWasm &&
		oldFn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType != fv1.ExecutorTypeWasm {
		return nil
	}

	// Executor type is no longer Wasm
	if newFn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType != fv1.ExecutorTypeWasm &&
		oldFn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType == fv1.ExecutorTypeWasm {
		wasm.logger.Info("function does not use new deployment executor anymore, deleting resources",
			zap.Any("function", newFn))
		// IMP - pass the oldFn, as the new/modified function is not in cache
		return wasm.deleteFunction(ctx, oldFn)
	}

	// Executor type changed to Wasm from something else
	if oldFn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType != fv1.ExecutorTypeWasm &&
		newFn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType == fv1.ExecutorTypeWasm {
		wasm.logger.Info("function type changed to Wasm, creating resources",
			zap.Any("old_function", oldFn.ObjectMeta),
			zap.Any("new_function", newFn.ObjectMeta))
		_, err := wasm.createFunction(ctx, newFn)
		if err != nil {
			wasm.updateStatus(oldFn, err, "error changing the function's type to Wasm")
		}
		return err
	}

	// if !reflect.DeepEqual(oldFn.Spec.InvokeStrategy, newFn.Spec.InvokeStrategy) {
	// 	// to support backward compatibility, if the function was created in default ns, we fall back to creating the
	// 	// deployment of the function in fission-function ns, so cleaning up resources there
	// 	ns := wasm.namespace
	// 	if newFn.ObjectMeta.Namespace != metav1.NamespaceDefault {
	// 		ns = newFn.ObjectMeta.Namespace
	// 	}

	// 	fsvc, err := wasm.fsCache.GetByFunctionUID(newFn.ObjectMeta.UID)
	// 	if err != nil {
	// 		err = errors.Wrapf(err, "error updating function due to unable to find function service cache: %v", oldFn)
	// 		return err
	// 	}

	// 	hpa, err := wasm.hpaops.GetHpa(ctx, ns, fsvc.Name)
	// 	if err != nil {
	// 		wasm.updateStatus(oldFn, err, "error getting HPA while updating function")
	// 		return err
	// 	}

	// 	hpaChanged := false

	// 	if newFn.Spec.InvokeStrategy.ExecutionStrategy.MinScale != oldFn.Spec.InvokeStrategy.ExecutionStrategy.MinScale {
	// 		replicas := int32(newFn.Spec.InvokeStrategy.ExecutionStrategy.MinScale)
	// 		hpa.Spec.MinReplicas = &replicas
	// 		hpaChanged = true
	// 	}

	// 	if newFn.Spec.InvokeStrategy.ExecutionStrategy.MaxScale != oldFn.Spec.InvokeStrategy.ExecutionStrategy.MaxScale {
	// 		hpa.Spec.MaxReplicas = int32(newFn.Spec.InvokeStrategy.ExecutionStrategy.MaxScale)
	// 		hpaChanged = true
	// 	}

	// 	if !reflect.DeepEqual(newFn.Spec.InvokeStrategy.ExecutionStrategy.Metrics, oldFn.Spec.InvokeStrategy.ExecutionStrategy.Metrics) {
	// 		hpa.Spec.Metrics = newFn.Spec.InvokeStrategy.ExecutionStrategy.Metrics
	// 		hpaChanged = true
	// 	}

	// 	if !reflect.DeepEqual(newFn.Spec.InvokeStrategy.ExecutionStrategy.Behavior, oldFn.Spec.InvokeStrategy.ExecutionStrategy.Behavior) {
	// 		hpa.Spec.Behavior = newFn.Spec.InvokeStrategy.ExecutionStrategy.Behavior
	// 		hpaChanged = true
	// 	}

	// 	if hpaChanged {
	// 		err := wasm.hpaops.UpdateHpa(ctx, hpa)
	// 		if err != nil {
	// 			wasm.updateStatus(oldFn, err, "error updating HPA while updating function")
	// 			return err
	// 		}
	// 	}
	// }

	deployChanged := false

	// If length of slice has changed then no need to check individual elements
	if len(oldFn.Spec.Secrets) != len(newFn.Spec.Secrets) {
		deployChanged = true
	} else {
		for i, newSecret := range newFn.Spec.Secrets {
			if newSecret != oldFn.Spec.Secrets[i] {
				deployChanged = true
				break
			}
		}
	}
	if len(oldFn.Spec.ConfigMaps) != len(newFn.Spec.ConfigMaps) {
		deployChanged = true
	} else {
		for i, newConfig := range newFn.Spec.ConfigMaps {
			if newConfig != oldFn.Spec.ConfigMaps[i] {
				deployChanged = true
				break
			}
		}
	}

	if !reflect.DeepEqual(oldFn.Spec.PodSpec, newFn.Spec.PodSpec) {
		deployChanged = true
	}

	if deployChanged {
		return wasm.updateFuncDeployment(ctx, newFn)
	}

	return nil
}

func (wasm *Wasm) updateFuncDeployment(ctx context.Context, fn *fv1.Function) error {

	fsvc, err := wasm.fsCache.GetByFunctionUID(fn.ObjectMeta.UID)
	if err != nil {
		err = errors.Wrapf(err, "error updating function due to unable to find function service cache: %v", fn)
		return err
	}
	fnObjName := fsvc.Name

	deployLabels := wasm.getDeployLabels(fn.ObjectMeta)
	wasm.logger.Info("updating deployment due to function update",
		zap.String("deployment", fnObjName), zap.Any("function", fn.ObjectMeta.Name))

	// to support backward compatibility, if the function was created in default ns, we fall back to creating the
	// deployment of the function in fission-function ns
	ns := wasm.namespace
	if fn.ObjectMeta.Namespace != metav1.NamespaceDefault {
		ns = fn.ObjectMeta.Namespace
	}

	existingDepl, err := wasm.kubernetesClient.AppsV1().Deployments(ns).Get(ctx, fnObjName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	// the resource version inside function packageRef is changed,
	// so the content of fetchRequest in deployment cmd is different.
	// Therefore, the deployment update will trigger a rolling update.
	newDeployment, err := wasm.getDeploymentSpec(ctx, fn, existingDepl.Spec.Replicas, // use current replicas instead of minscale in the ExecutionStrategy.
		fnObjName, ns, deployLabels, wasm.getDeployAnnotations(fn.ObjectMeta))
	if err != nil {
		wasm.updateStatus(fn, err, "failed to get new deployment spec while updating function")
		return err
	}

	err = wasm.updateDeployment(ctx, newDeployment, ns)
	if err != nil {
		wasm.updateStatus(fn, err, "failed to update deployment while updating function")
		return err
	}

	return nil
}

func (wasm *Wasm) fnDelete(ctx context.Context, fn *fv1.Function) error {
	multierr := &multierror.Error{}

	// GetByFunction uses resource version as part of cache key, however,
	// the resource version in function metadata will be changed when a function
	// is deleted and cause Wasm backend fails to delete the entry.
	// Use GetByFunctionUID instead of GetByFunction here to find correct
	// fsvc entry.
	fsvc, err := wasm.fsCache.GetByFunctionUID(fn.ObjectMeta.UID)
	if err != nil {
		err = errors.Wrap(err, fmt.Sprintf("fsvc not found in cache: %v", fn.ObjectMeta))
		return err
	}

	objName := fsvc.Name

	_, err = wasm.fsCache.DeleteOld(fsvc, time.Second*0)
	if err != nil {
		multierr = multierror.Append(multierr,
			errors.Wrapf(err, "error deleting the function from cache"))
	}

	// to support backward compatibility, if the function was created in default ns, we fall back to creating the
	// deployment of the function in fission-function ns, so cleaning up resources there
	ns := wasm.namespace
	if fn.ObjectMeta.Namespace != metav1.NamespaceDefault {
		ns = fn.ObjectMeta.Namespace
	}

	err = wasm.cleanupWasm(ctx, ns, objName)
	multierr = multierror.Append(multierr, err)

	return multierr.ErrorOrNil()
}

func (wasm *Wasm) fnShortDelete(ctx context.Context, fn *fv1.Function) error {
	multierr := &multierror.Error{}

	// GetByFunction uses resource version as part of cache key, however,
	// the resource version in function metadata will be changed when a function
	// is deleted and cause Wasm backend fails to delete the entry.
	// Use GetByFunctionUID instead of GetByFunction here to find correct
	// fsvc entry.
	fsvc, err := wasm.fsCache.GetByFunctionUID(fn.ObjectMeta.UID)
	if err != nil {
		err = errors.Wrap(err, fmt.Sprintf("fsvc not found in cache: %v", fn.ObjectMeta))
		return err
	}

	objName := fsvc.Name

	_, err = wasm.fsCache.DeleteOld(fsvc, time.Second*0)
	if err != nil {
		multierr = multierror.Append(multierr,
			errors.Wrapf(err, "error deleting the function from cache"))
	}

	// to support backward compatibility, if the function was created in default ns, we fall back to creating the
	// deployment of the function in fission-function ns, so cleaning up resources there
	ns := wasm.namespace
	if fn.ObjectMeta.Namespace != metav1.NamespaceDefault {
		ns = fn.ObjectMeta.Namespace
	}

	err = wasm.cleanupShortWasm(ctx, ns, objName)
	multierr = multierror.Append(multierr, err)

	return multierr.ErrorOrNil()
}

// getObjName returns a unique name for kubernetes objects of function
func (wasm *Wasm) getObjName(fn *fv1.Function) string {
	// use meta uuid of function, this ensure we always get the same name for the same function.
	uid := fn.ObjectMeta.UID[len(fn.ObjectMeta.UID)-17:]
	var functionMetadata string
	if len(fn.ObjectMeta.Name)+len(fn.ObjectMeta.Namespace) < 35 {
		functionMetadata = fn.ObjectMeta.Name + "-" + fn.ObjectMeta.Namespace
	} else {
		if len(fn.ObjectMeta.Name) > 17 {
			functionMetadata = fn.ObjectMeta.Name[:17]
		} else {
			functionMetadata = fn.ObjectMeta.Name
		}
		if len(fn.ObjectMeta.Namespace) > 17 {
			functionMetadata = functionMetadata + "-" + fn.ObjectMeta.Namespace[:17]
		} else {
			functionMetadata = functionMetadata + "-" + fn.ObjectMeta.Namespace
		}
	}
	// constructed name should be 63 characters long, as it is a valid k8s name
	// functionMetadata should be 35 characters long, as we take 17 characters from functionUid
	// with newdeploy 10 character prefix
	return strings.ToLower(fmt.Sprintf("Wasm-%s-%s", functionMetadata, uid))
}

func (wasm *Wasm) getDeployLabels(fnMeta metav1.ObjectMeta) map[string]string {
	deployLabels := maps.CopyStringMap(fnMeta.Labels)
	deployLabels[fv1.EXECUTOR_TYPE] = string(fv1.ExecutorTypeWasm)
	deployLabels[fv1.FUNCTION_NAME] = fnMeta.Name
	deployLabels[fv1.FUNCTION_NAMESPACE] = fnMeta.Namespace
	deployLabels[fv1.FUNCTION_UID] = string(fnMeta.UID)
	return deployLabels
}

func (wasm *Wasm) getDeployAnnotations(fnMeta metav1.ObjectMeta) map[string]string {
	deployAnnotations := maps.CopyStringMap(fnMeta.Annotations)
	deployAnnotations[fv1.EXECUTOR_INSTANCEID_LABEL] = wasm.instanceID
	deployAnnotations[fv1.FUNCTION_RESOURCE_VERSION] = fnMeta.ResourceVersion
	return deployAnnotations
}

// updateStatus is a function which updates status of update.
// Current implementation only logs messages, in future it will update function status
func (wasm *Wasm) updateStatus(fn *fv1.Function, err error, message string) {
	wasm.logger.Error("function status update", zap.Error(err), zap.Any("function", fn), zap.String("message", message))
}

// idleObjectReaper reaps objects after certain idle time
func (wasm *Wasm) idleObjectReaper(ctx context.Context) {
	// calling function doIdleObjectReaper() repeatedly at given interval of time
	wait.UntilWithContext(ctx, wasm.doIdleObjectReaper, time.Second*5)
}

func (wasm *Wasm) doIdleObjectReaper(ctx context.Context) {
	funcSvcs, err := wasm.fsCache.ListOld(time.Second * 5)
	if err != nil {
		wasm.logger.Error("error reaping idle pods", zap.Error(err))
		return
	}

	for i := range funcSvcs {
		fsvc := funcSvcs[i]

		if fsvc.Executor != fv1.ExecutorTypeWasm {
			continue
		}

		fn, err := wasm.fissionClient.CoreV1().Functions(fsvc.Function.Namespace).Get(ctx, fsvc.Function.Name, metav1.GetOptions{})
		if err != nil {
			// wasm manager handles the function delete event and clean cache/kubeobjs itself,
			// so we ignore the not found error for functions with wasm executor type here.
			if k8sErrs.IsNotFound(err) && fsvc.Executor == fv1.ExecutorTypeWasm {
				continue
			}
			wasm.logger.Error("error getting function", zap.Error(err), zap.String("function", fsvc.Function.Name))
			continue
		}

		idlePodReapTime := wasm.defaultIdlePodReapTime
		if fn.Spec.IdleTimeout != nil {
			idlePodReapTime = time.Duration(*fn.Spec.IdleTimeout) * time.Second
		}

		if time.Since(fsvc.Atime) < idlePodReapTime {
			continue
		}

		go func() {
			deployObj := getDeploymentObj(fsvc.KubernetesObjects)
			if deployObj == nil {
				wasm.logger.Error("error finding function deployment", zap.Error(err), zap.String("function", fsvc.Function.Name))
				return
			}

			currentDeploy, err := wasm.kubernetesClient.AppsV1().
				Deployments(deployObj.Namespace).Get(ctx, deployObj.Name, metav1.GetOptions{})
			if err != nil {
				wasm.logger.Error("error getting function deployment", zap.Error(err), zap.String("function", fsvc.Function.Name))
				return
			}

			minScale := int32(fn.Spec.InvokeStrategy.ExecutionStrategy.MinScale)

			// do nothing if the current replicas is already lower than minScale
			if *currentDeploy.Spec.Replicas <= minScale {
				return
			}

			err = wasm.scaleDeployment(ctx, deployObj.Namespace, deployObj.Name, minScale)
			if err != nil {
				wasm.logger.Error("error scaling down function deployment", zap.Error(err), zap.String("function", fsvc.Function.Name))
			}
		}()
	}
}

func getDeploymentObj(kubeobjs []apiv1.ObjectReference) *apiv1.ObjectReference {
	for _, kubeobj := range kubeobjs {
		switch strings.ToLower(kubeobj.Kind) {
		case "deployment":
			return &kubeobj
		}
	}
	return nil
}
