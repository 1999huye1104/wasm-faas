package wasm

import (
	"context"

	"go.uber.org/zap"
	k8sCache "k8s.io/client-go/tools/cache"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
)

func (wasm *Wasm) FuncInformerHandler(ctx context.Context) k8sCache.ResourceEventHandlerFuncs {
	return k8sCache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			// fn := obj.(*fv1.Function)
			// fnExecutorType := fn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType
			// if fnExecutorType != "" && fnExecutorType != fv1.ExecutorTypeWasm {
			// 	return
			// }
			// // TODO: A workaround to process items in parallel. We should use workqueue ("k8s.io/client-go/util/workqueue")
			// // and worker pattern to process items instead of moving process to another goroutine.
			// // example: https://github.com/kubernetes/kubernetes/blob/master/pkg/controller/job/job_controller.go
			// go func() {
			// 	log := wasm.logger.With(zap.String("function_name", fn.ObjectMeta.Name), zap.String("function_namespace", fn.ObjectMeta.Namespace))
			// 	log.Debug("start function create handler")
			// 	_, err := wasm.createFunction(ctx, fn)
			// 	if err != nil {
			// 		log.Error("error eager creating function", zap.Error(err))
			// 	}
			// 	log.Debug("end function create handler")
			// }()
		},
		DeleteFunc: func(obj interface{}) {
			fn := obj.(*fv1.Function)
			fnExecutorType := fn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType
			if fnExecutorType != "" && fnExecutorType != fv1.ExecutorTypeWasm &&fnExecutorType != fv1.ExecutorTypeWasmShortA && fnExecutorType != fv1.ExecutorTypeWasmShortS{
				return
			}
			go func() {
				log := wasm.logger.With(zap.String("function_name", fn.ObjectMeta.Name), zap.String("function_namespace", fn.ObjectMeta.Namespace))
				log.Debug("start function delete handler")
				if fnExecutorType == fv1.ExecutorTypeWasm{
					err := wasm.deleteFunction(ctx, fn)
					if err != nil {
						log.Error("error deleting function", zap.Error(err))
					}
				}else{
					err := wasm.deleteShortFunction(ctx, fn)
					if err != nil {
						log.Error("error deleting function", zap.Error(err))
					}
				}
				
				log.Debug("end function delete handler")
			}()
		},
		UpdateFunc: func(oldObj interface{}, newObj interface{}) {
			oldFn := oldObj.(*fv1.Function)
			newFn := newObj.(*fv1.Function)
			fnExecutorType := oldFn.Spec.InvokeStrategy.ExecutionStrategy.ExecutorType
			if fnExecutorType != "" && fnExecutorType != fv1.ExecutorTypeWasm {
				return
			}
			go func() {
				log := wasm.logger.With(zap.String("function_name", newFn.ObjectMeta.Name),
					zap.String("function_namespace", newFn.ObjectMeta.Namespace),
					zap.String("old_function_name", oldFn.ObjectMeta.Name))
				log.Debug("start function update handler")
				err := wasm.updateFunction(ctx, oldFn, newFn)
				if err != nil {
					log.Error("error updating function",
						zap.Error(err))
				}
				log.Debug("end function update handler")

			}()
		},
	}
}
