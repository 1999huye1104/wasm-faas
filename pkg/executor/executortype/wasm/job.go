package wasm

import (
	"context"
	"fmt"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"go.uber.org/zap"
	apiv1 "k8s.io/api/core/v1"
	k8s_err "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"github.com/fission/fission/pkg/executor/util"

	fv1 "github.com/fission/fission/pkg/apis/core/v1"
	otelUtils "github.com/fission/fission/pkg/utils/otel"
)
func (wasm *Wasm) createOrGetJob(ctx context.Context, fn *fv1.Function, deployLabels map[string]string, deployAnnotations map[string]string, jobName string, jobNamespace string) (*batchv1.Job, error) {
	
	logger := otelUtils.LoggerWithTraceID(ctx, wasm.logger)
	
	gracePeriodSeconds := int64(6 * 60)
	if *fn.Spec.PodSpec.TerminationGracePeriodSeconds >= 0 {
		gracePeriodSeconds = *fn.Spec.PodSpec.TerminationGracePeriodSeconds
	}


	runtimeClassName := "wasm"

	if fn.Spec.PodSpec == nil {
		return nil, fmt.Errorf("podSpec is not set for function %s", fn.ObjectMeta.Name)
	}

	container := &apiv1.Container{
		Name:                   fn.ObjectMeta.Name,
		ImagePullPolicy:        wasm.runtimeImagePullPolicy,
		TerminationMessagePath: "/dev/termination-log",
	}

	podSpec, err := util.MergePodSpec(&apiv1.PodSpec{
		RestartPolicy:    corev1.RestartPolicyNever,
		RuntimeClassName: &runtimeClassName,
		Containers:                    []apiv1.Container{*container},
		TerminationGracePeriodSeconds: &gracePeriodSeconds,
	}, fn.Spec.PodSpec)

	pod := apiv1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      deployLabels,
			Annotations: deployAnnotations,
		},
		Spec: *podSpec,
	}
	pod.Spec = *(util.ApplyImagePullSecret("", pod.Spec))

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: jobName,
			Labels:      deployLabels,
			Annotations: deployAnnotations,
		},
		Spec: batchv1.JobSpec{
			// 在这里设置 Job 的规范和参数，例如设置容器模板等
			Template: pod,
		},
	}
	wasm.logger.Info("******k8s创建job********")
	existingJob, err := wasm.kubernetesClient.BatchV1().Jobs(jobNamespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil && !k8s_err.IsNotFound(err) {
		return nil, err
	}

	// Create new job if one does not previously exist
	if k8s_err.IsNotFound(err) {
		job, err := wasm.kubernetesClient.BatchV1().Jobs(jobNamespace).Create(ctx, job, metav1.CreateOptions{})
		if err != nil {
		    if k8s_err.IsAlreadyExists(err) {
				job, err = wasm.kubernetesClient.BatchV1().Jobs(jobNamespace).Get(ctx, jobName, metav1.GetOptions{})
			}
			if err != nil {
				logger.Error("error while creating function job",
					zap.Error(err),
					zap.String("function", fn.ObjectMeta.Name),
					zap.String("job_name", jobName),
					zap.String("job_namespace", jobNamespace))
				return nil, err
			}
		}
		
		return job, err
	}
	// _, err = wasm.kubernetesClient.BatchV1().Jobs(jobNamespace).Create(context.TODO(), job, metav1.CreateOptions{})
	// if err != nil {
	// 	fmt.Println("failed to create.")
	return existingJob, err
}


func (wasm *Wasm) deleteJob(ctx context.Context, ns string, name string) error {

	deletePropagation := metav1.DeletePropagationBackground
	// return cn.kubernetesClient.AppsV1().Deployments(ns).Delete(ctx, name, metav1.DeleteOptions{
	// 	PropagationPolicy: &deletePropagation,
	// })
    return wasm.kubernetesClient.BatchV1().Jobs(ns).Delete(ctx, name, metav1.DeleteOptions{
		PropagationPolicy: &deletePropagation,
	})
}