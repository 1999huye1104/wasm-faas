package wasm

import (
	"context"
	"strconv"

	multierror "github.com/hashicorp/go-multierror"
	"go.uber.org/zap"
	apiv1 "k8s.io/api/core/v1"
	k8s_err "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
)

// getResources gets the resources(CPU, memory) set for the function
func (wasm *Wasm) getResources(fn *fv1.Function) apiv1.ResourceRequirements {
	resources := fn.Spec.Resources
	if resources.Requests == nil {
		resources.Requests = make(map[apiv1.ResourceName]resource.Quantity)
	}
	if resources.Limits == nil {
		resources.Limits = make(map[apiv1.ResourceName]resource.Quantity)
	}

	val, ok := fn.Spec.Resources.Requests[apiv1.ResourceCPU]
	if ok && !val.IsZero() {
		resources.Requests[apiv1.ResourceCPU] = fn.Spec.Resources.Requests[apiv1.ResourceCPU]
	}

	val, ok = fn.Spec.Resources.Requests[apiv1.ResourceMemory]
	if ok && !val.IsZero() {
		resources.Requests[apiv1.ResourceMemory] = fn.Spec.Resources.Requests[apiv1.ResourceMemory]
	}

	val, ok = fn.Spec.Resources.Limits[apiv1.ResourceCPU]
	if ok && !val.IsZero() {
		resources.Limits[apiv1.ResourceCPU] = fn.Spec.Resources.Limits[apiv1.ResourceCPU]
	}

	val, ok = fn.Spec.Resources.Limits[apiv1.ResourceMemory]
	if ok && !val.IsZero() {
		resources.Limits[apiv1.ResourceMemory] = fn.Spec.Resources.Limits[apiv1.ResourceMemory]
	}

	return resources
}

// cleanupWasm cleans all kubernetes objects related to function
func (wasm *Wasm) cleanupWasm(ctx context.Context, ns string, name string) error {
	result := &multierror.Error{}

	err := wasm.deleteSvc(ctx, ns, name)
	if err != nil && !k8s_err.IsNotFound(err) {
		wasm.logger.Error("error deleting service for Wasm function",
			zap.Error(err),
			zap.String("function_name", name),
			zap.String("function_namespace", ns))
		result = multierror.Append(result, err)
	}

	// err = wasm.hpaops.DeleteHpa(ctx, ns, name)
	// if err != nil && !k8s_err.IsNotFound(err) {
	// 	wasm.logger.Error("error deleting HPA for Wasm function",
	// 		zap.Error(err),
	// 		zap.String("function_name", name),
	// 		zap.String("function_namespace", ns))
	// 	result = multierror.Append(result, err)
	// }

	err = wasm.deleteDeployment(ctx, ns, name)
	if err != nil && !k8s_err.IsNotFound(err) {
		wasm.logger.Error("error deleting deployment for Wasm function",
			zap.Error(err),
			zap.String("function_name", name),
			zap.String("function_namespace", ns))
		result = multierror.Append(result, err)
	}

	return result.ErrorOrNil()
}


func (wasm *Wasm) cleanupShortWasm(ctx context.Context, ns string, name string) error {
	result := &multierror.Error{}
	err := wasm.deleteJob(ctx, ns, name)
	if err != nil && !k8s_err.IsNotFound(err) {
		wasm.logger.Error("error deleting service for Wasm function",
			zap.Error(err),
			zap.String("function_name", name),
			zap.String("function_namespace", ns))
		result = multierror.Append(result, err)
	}
	return result.ErrorOrNil()
}
// referencedResourcesRVSum returns the sum of resource version of all resources the function references to.
// We used to update timestamp in the deployment environment field in order to trigger a rolling update when
// the function referenced resources get updated. However, use timestamp means we are not able to avoid tri-
// ggering a rolling update when executor tries to adopt orphaned deployment due to timestamp changed which
// is unwanted. In order to let executor adopt deployment without triggering a rolling update, we need an
// identical way to get a value that can reflect resources changed without affecting by the time.
// To achieve this goal, the sum of the resource version of all referenced resources is a good fit for our
// scenario since the sum of the resource version is always the same as long as no resources changed.
func referencedResourcesRVSum(ctx context.Context, client kubernetes.Interface, namespace string, secrets []fv1.SecretReference, cfgmaps []fv1.ConfigMapReference) (int, error) {
	rvCount := 0

	if len(secrets) > 0 {
		list, err := client.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return 0, err
		}

		objmap := make(map[string]apiv1.Secret)
		for _, secret := range list.Items {
			objmap[secret.Namespace+"/"+secret.Name] = secret
		}

		for _, ref := range secrets {
			s, ok := objmap[ref.Namespace+"/"+ref.Name]
			if ok {
				rv, _ := strconv.ParseInt(s.ResourceVersion, 10, 32)
				rvCount += int(rv)
			}
		}
	}

	if len(cfgmaps) > 0 {
		list, err := client.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return 0, err
		}

		objmap := make(map[string]apiv1.ConfigMap)
		for _, cfg := range list.Items {
			objmap[cfg.Namespace+"/"+cfg.Name] = cfg
		}

		for _, ref := range cfgmaps {
			s, ok := objmap[ref.Namespace+"/"+ref.Name]
			if ok {
				rv, _ := strconv.ParseInt(s.ResourceVersion, 10, 32)
				rvCount += int(rv)
			}
		}
	}

	return rvCount, nil
}
