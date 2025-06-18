/*
Copyright 2024.

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

package controller

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	tfv1 "github.com/NexusGPU/tensor-fusion/api/v1"
	"github.com/NexusGPU/tensor-fusion/internal/config"
	"github.com/NexusGPU/tensor-fusion/internal/constants"
	"github.com/NexusGPU/tensor-fusion/internal/gpuallocator"
	"github.com/NexusGPU/tensor-fusion/internal/metrics"
	"github.com/NexusGPU/tensor-fusion/internal/portallocator"
	"github.com/NexusGPU/tensor-fusion/internal/utils"
	"github.com/NexusGPU/tensor-fusion/internal/worker"
	"github.com/lithammer/shortuuid/v4"
	"github.com/samber/lo"
)

// TensorFusionWorkloadReconciler reconciles a TensorFusionWorkload object
type TensorFusionWorkloadReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	Allocator     *gpuallocator.GpuAllocator
	Recorder      record.EventRecorder
	GpuInfos      *[]config.GpuInfo
	PortAllocator *portallocator.PortAllocator
}

// +kubebuilder:rbac:groups=tensor-fusion.ai,resources=tensorfusionworkloads,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=tensor-fusion.ai,resources=tensorfusionworkloads/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=tensor-fusion.ai,resources=tensorfusionworkloads/finalizers,verbs=update

// TensorFusionWorkload Reconciler
//
//nolint:gocyclo
func (r *TensorFusionWorkloadReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.Info("Reconciling TensorFusionWorkload", "request", req)

	// Fetch the TensorFusionWorkload instance
	workload := &tfv1.TensorFusionWorkload{}
	if err := r.Get(ctx, req.NamespacedName, workload); err != nil {
		if errors.IsNotFound(err) {
			// Object not found, return without error
			return ctrl.Result{}, nil
		}
		// Error reading the object
		return ctrl.Result{}, err
	}

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(req.Namespace),
		client.MatchingLabels{constants.WorkloadKey: workload.Name}); err != nil {
		return ctrl.Result{}, fmt.Errorf("list pods: %w", err)
	}

	shouldReturn, err := utils.HandleFinalizer(ctx, workload, r.Client, func(ctx context.Context, _ *tfv1.TensorFusionWorkload) (bool, error) {
		// delete all pods
		existsPods := lo.Filter(podList.Items, func(pod corev1.Pod, _ int) bool {
			return pod.DeletionTimestamp == nil
		})
		if len(existsPods) > 0 {
			if err := r.DeleteAllOf(ctx, &corev1.Pod{},
				client.InNamespace(req.Namespace),
				client.MatchingLabels{constants.WorkloadKey: workload.Name}); err != nil {
				return false, fmt.Errorf("delete pods: %w", err)
			}
		}
		// check if all pods are deleted
		return len(podList.Items) == 0, nil
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("handle finalizer: %w", err)
	}
	if shouldReturn {
		return ctrl.Result{}, nil
	}
	// Handle pods with finalizers that need GPU resource cleanup
	hasDeletion := false
	// Process pods with our finalizer
	for i := range podList.Items {
		pod := &podList.Items[i]
		deleted := !pod.DeletionTimestamp.IsZero()

		if deleted {
			metrics.RemoveWorkerMetrics(pod.Name, pod.DeletionTimestamp.Time)
			podPort, _ := strconv.Atoi(pod.Annotations[constants.GenPortNumberAnnotation])
			_ = r.PortAllocator.ReleaseHostPort(pod.Spec.NodeName, pod.Name, podPort, false)
		}

		// Handle our GPU resource cleanup finalizer
		_, err := utils.HandleFinalizer(ctx, pod, r.Client, func(ctx context.Context, obj *corev1.Pod) (bool, error) {
			return r.handlePodGPUCleanup(ctx, pod, workload)
		})

		if err != nil {
			return ctrl.Result{}, err
		}
		hasDeletion = hasDeletion || deleted
	}

	if hasDeletion {
		return ctrl.Result{RequeueAfter: constants.PendingRequeueDuration}, nil
	}

	if !workload.DeletionTimestamp.IsZero() {
		log.Info("Workload is being deleted, skipping pod creation", "name", workload.Name, "namespace", workload.Namespace)
		return ctrl.Result{}, nil
	}

	// init metrics map if needed
	handleMetricsRecorder(podList, workload)

	// Fetch the GPUPool
	pool := &tfv1.GPUPool{}
	if err := r.Get(ctx, client.ObjectKey{Name: workload.Spec.PoolName}, pool); err != nil {
		return ctrl.Result{}, fmt.Errorf("gpu pool(%s) does not exist", workload.Spec.PoolName)
	}

	// Create worker generator
	workerGenerator := &worker.WorkerGenerator{WorkerConfig: pool.Spec.ComponentConfig.Worker, GpuInfos: r.GpuInfos}

	podTemplateHash, err := workerGenerator.PodTemplateHash(workload.Spec)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get pod template hash: %w", err)
	}

	if workload.Status.PodTemplateHash != podTemplateHash {
		workload.Status.PodTemplateHash = podTemplateHash
		if err := r.Status().Update(ctx, workload); err != nil {
			return ctrl.Result{}, fmt.Errorf("update status: %w", err)
		}
	}

	result, err := r.reconcileScaling(ctx, workload, podList, workerGenerator, podTemplateHash)
	if err != nil || !result.IsZero() {
		return result, err
	}

	if err := r.updateStatus(ctx, workload, podList.Items, workerGenerator); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// reconcileScaling handles scaling up and down of worker pods and updates replica status
func (r *TensorFusionWorkloadReconciler) reconcileScaling(
	ctx context.Context,
	workload *tfv1.TensorFusionWorkload,
	podList *corev1.PodList,
	workerGenerator *worker.WorkerGenerator,
	podTemplateHash string,
) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	// Check if there are any Pods using the old podTemplateHash and delete them if any
	if len(podList.Items) > 0 {
		var outdatedPods []corev1.Pod
		for i := range podList.Items {
			pod := &podList.Items[i]
			if pod.Labels[constants.LabelKeyPodTemplateHash] != podTemplateHash {
				outdatedPods = append(outdatedPods, *pod)
			}
		}

		if len(outdatedPods) > 0 {
			log.Info("Found outdated pods with different template hash", "count", len(outdatedPods))
			if err := r.scaleDownWorkers(ctx, workload, outdatedPods); err != nil {
				return ctrl.Result{}, err
			}
			// After deletion, requeue, and the next reconcile will create a new pod
			return ctrl.Result{Requeue: true}, nil
		}
	}

	// Determine the number of replicas
	desiredReplicas := int32(1)
	if workload.Spec.Replicas != nil {
		desiredReplicas = *workload.Spec.Replicas
	}

	// Count current replicas
	currentReplicas := int32(len(podList.Items))
	log.Info("Current replicas", "count", currentReplicas, "desired", desiredReplicas)

	// Update workload status
	if workload.Status.Replicas != currentReplicas {
		workload.Status.Replicas = currentReplicas
		if err := r.Status().Update(ctx, workload); err != nil {
			return ctrl.Result{}, fmt.Errorf("update status: %w", err)
		}
	}

	// Scale up if needed
	if currentReplicas < desiredReplicas {
		log.Info("Scaling up workers", "from", currentReplicas, "to", desiredReplicas)

		// Calculate how many pods need to be added
		podsToAdd := int(desiredReplicas - currentReplicas)
		result, err := r.scaleUpWorkers(ctx, workerGenerator, workload, podsToAdd, podTemplateHash)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("scale up workers: %w", err)
		}
		if !result.IsZero() {
			return result, nil
		}
	} else if currentReplicas > desiredReplicas {
		log.Info("Scaling down workers", "from", currentReplicas, "to", desiredReplicas)

		// Sort pods by creation time (oldest first)
		sort.Slice(podList.Items, func(i, j int) bool {
			return podList.Items[i].CreationTimestamp.Before(&podList.Items[j].CreationTimestamp)
		})

		// Calculate how many pods need to be removed
		podsToRemove := int(currentReplicas - desiredReplicas)
		if err := r.scaleDownWorkers(ctx, workload, podList.Items[:podsToRemove]); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func handleMetricsRecorder(podList *corev1.PodList, workload *tfv1.TensorFusionWorkload) {
	now := time.Now()
	for i := range podList.Items {
		pod := &podList.Items[i]
		metrics.SetWorkerMetricsByWorkload(pod, workload, now)
	}
}

// TODO for is-local-gpu mode, should not start worker, and workload replicas must be dynamic, to support worker-as-lib design
// create special tensorfusion connection instead with finalizer, based on workload status's pending worker count, when connection is removed, should Dealloc GPU resources
func (r *TensorFusionWorkloadReconciler) tryStartWorker(
	ctx context.Context,
	workerGenerator *worker.WorkerGenerator,
	gpus []*tfv1.GPU,
	workload *tfv1.TensorFusionWorkload,
	hash string,
) (*corev1.Pod, error) {
	if len(gpus) == 0 || gpus[0].Labels == nil {
		return nil, fmt.Errorf("no gpus or no labels, can not assign host port for worker")
	}
	port, err := r.PortAllocator.AssignHostPort(gpus[0].Status.NodeSelector[constants.KubernetesHostNameLabel])
	if err != nil {
		return nil, fmt.Errorf("get host port %w", err)
	}
	pod, hash, err := workerGenerator.GenerateWorkerPod(gpus, workload.Name, workload.Namespace, port, workload.Spec.Resources.Requests, workload.Spec.Resources.Limits, hash)
	if err != nil {
		return nil, fmt.Errorf("generate worker pod %w", err)
	}

	// Add labels to identify this pod as part of the workload
	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}

	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}

	gpuNames := lo.Map(gpus, func(gpu *tfv1.GPU, _ int) string {
		return gpu.Name
	})

	pod.Labels[constants.WorkloadKey] = workload.Name
	pod.Labels[constants.LabelKeyPodTemplateHash] = hash
	pod.Annotations[constants.GpuKey] = strings.Join(gpuNames, ",")

	// Add finalizer for GPU resource cleanup
	pod.Finalizers = append(pod.Finalizers, constants.Finalizer)

	if err := ctrl.SetControllerReference(workload, pod, r.Scheme); err != nil {
		return nil, fmt.Errorf("set owner reference %w", err)
	}
	if err := r.Create(ctx, pod); err != nil {
		return nil, fmt.Errorf("create pod %w", err)
	}
	return pod, nil
}

// scaleDownWorkers handles the scaling down of worker pods
func (r *TensorFusionWorkloadReconciler) scaleDownWorkers(ctx context.Context, workload *tfv1.TensorFusionWorkload, pods []corev1.Pod) error {
	log := log.FromContext(ctx)

	for i := range pods {
		podToDelete := &pods[i]
		log.Info("Scaling down worker pod", "name", podToDelete.Name, "workload", workload.Name)
		// Delete the pod with foreground deletion policy
		// The finalizer will handle GPU resource cleanup
		if err := r.deletePod(ctx, podToDelete); err != nil {
			return err
		}
	}
	return nil
}

// handlePodGPUCleanup handles the cleanup of GPU resources when a pod is being deleted
func (r *TensorFusionWorkloadReconciler) handlePodGPUCleanup(ctx context.Context, pod *corev1.Pod, workload *tfv1.TensorFusionWorkload) (bool, error) {
	log := log.FromContext(ctx)

	log.Info("Processing pod with GPU resource cleanup finalizer", "pod", pod.Name)

	pod.Annotations[constants.GpuReleasedAnnotation] = shortuuid.New()

	// Update the annotation of the Pod to mark that GPU cleanup has been successfully processed.
	// This is a key part of ensuring idempotency for the handlePodGPUCleanup function.
	// If this function is called again for the same Pod instance (e.g., due to the client cache
	// not yet reflecting the finalizer's removal), Then this r.Update pod will fail.
	// Will not cause duplicate releases
	if err := r.Update(ctx, pod); err != nil {
		log.Error(err, "Failed to mark that GPU cleanup of pod")
		return false, err
	}

	// read the GPU names from the pod annotations
	gpuNamesStr, ok := pod.Annotations[constants.GpuKey]
	if !ok {
		log.Info("Pod has finalizer but no GPU label", "pod", pod.Name)
		return true, nil
	}

	// Split GPU names by comma
	gpuNames := strings.Split(gpuNamesStr, ",")
	gpus := lo.Map(gpuNames, func(gpuName string, _ int) types.NamespacedName {
		return types.NamespacedName{Name: gpuName}
	})
	// Release GPU resources
	r.Allocator.Dealloc(ctx, tfv1.NameNamespace{Name: workload.Name, Namespace: workload.Namespace}, workload.Spec.Resources.Requests, gpus)
	log.Info("Released GPU resources via finalizer", "gpus", gpus, "pod", pod.Name)

	return true, nil
}

// deletePod deletes a pod
func (r *TensorFusionWorkloadReconciler) deletePod(ctx context.Context, pod *corev1.Pod) error {
	log := log.FromContext(ctx)

	if err := r.Delete(ctx, pod); err != nil {
		log.Error(err, "Failed to delete worker pod", "name", pod.Name)
		return fmt.Errorf("delete worker pod: %w", err)
	}

	log.Info("Deleted worker pod", "name", pod.Name)
	return nil
}

// scaleUpWorkers handles the scaling up of worker pods
func (r *TensorFusionWorkloadReconciler) scaleUpWorkers(ctx context.Context, workerGenerator *worker.WorkerGenerator, workload *tfv1.TensorFusionWorkload, count int, hash string) (ctrl.Result, error) {
	workloadNameNs := tfv1.NameNamespace{Namespace: workload.Namespace, Name: workload.Name}
	// Create worker pods
	for range count {
		// Schedule GPU for the worker
		gpus, err := r.Allocator.Alloc(ctx, gpuallocator.AllocRequest{
			PoolName:              workload.Spec.PoolName,
			WorkloadNameNamespace: workloadNameNs,
			Request:               workload.Spec.Resources.Requests,
			Count:                 workload.Spec.GPUCount,
			GPUModel:              workload.Spec.GPUModel,
			NodeAffinity:          workload.Spec.NodeAffinity,
		})
		if err != nil {
			metrics.SetSchedulerMetrics(workload.Spec.PoolName, false)
			r.Recorder.Eventf(workload, corev1.EventTypeWarning, "ScheduleGPUFailed", "Failed to schedule GPU: %v", err)
			return ctrl.Result{RequeueAfter: constants.PendingRequeueDuration}, nil
		}

		metrics.SetSchedulerMetrics(workload.Spec.PoolName, true)
		_, err = r.tryStartWorker(ctx, workerGenerator, gpus, workload, hash)
		if err != nil {
			// Try to release all allocated GPUs if pod creation fails
			gpus := lo.Map(gpus, func(gpu *tfv1.GPU, _ int) types.NamespacedName {
				return client.ObjectKeyFromObject(gpu)
			})
			r.Allocator.Dealloc(ctx, workloadNameNs, workload.Spec.Resources.Requests, gpus)
			return ctrl.Result{}, fmt.Errorf("create worker pod: %w", err)
		}

	}

	return ctrl.Result{}, nil
}

// updateStatus updates the status of a TensorFusionWorkload
func (r *TensorFusionWorkloadReconciler) updateStatus(
	ctx context.Context,
	workload *tfv1.TensorFusionWorkload,
	pods []corev1.Pod,
	workerGenerator *worker.WorkerGenerator,
) error {
	log := log.FromContext(ctx)
	readyReplicas := int32(0)
	failedWorkers := 0

	// Create a worker statuses slice to hold all worker status information
	workerStatuses := []tfv1.WorkerStatus{}

	for _, pod := range pods {
		// Skip pods that are being deleted
		if pod.DeletionTimestamp != nil {
			continue
		}

		var workerPhase tfv1.WorkerPhase
		switch pod.Status.Phase {
		case corev1.PodPending:
			workerPhase = tfv1.WorkerPending
		case corev1.PodRunning:
			if utils.IsPodConditionTrue(pod.Status.Conditions, corev1.PodReady) {
				workerPhase = tfv1.WorkerRunning
				readyReplicas++
			} else {
				workerPhase = tfv1.WorkerPending
			}
		case corev1.PodFailed:
			workerPhase = tfv1.WorkerFailed
			failedWorkers++
		default:
			workerPhase = tfv1.WorkerPending
		}

		// Get worker IP and port information from pod
		ip := pod.Status.PodIP
		port, err := workerGenerator.WorkerPort(&pod)
		if err != nil {
			log.Error(err, "can not get worker port", "pod", pod.Name, "error", err)
			continue
		}

		// Create and append worker status
		workerStatus := tfv1.WorkerStatus{
			WorkerPhase:     workerPhase,
			WorkerName:      pod.Name,
			WorkerIp:        ip,
			WorkerPort:      port,
			NodeSelector:    pod.Spec.NodeSelector,
			ResourceVersion: pod.ResourceVersion,
		}

		workerStatuses = append(workerStatuses, workerStatus)
	}

	// Determine workload phase
	var phase tfv1.TensorFusionWorkloadPhase
	var conditions []metav1.Condition

	// Update Ready condition based on readyReplicas and desired replicas
	readyCondition := metav1.Condition{
		Type:               constants.ConditionStatusTypeReady,
		LastTransitionTime: metav1.Now(),
	}

	if workload.Spec.Replicas != nil && readyReplicas == *workload.Spec.Replicas {
		phase = tfv1.TensorFusionWorkloadPhaseRunning
		readyCondition.Status = metav1.ConditionTrue
		readyCondition.Reason = "WorkloadReady"
		readyCondition.Message = "All workers are running"
	} else if failedWorkers > 0 {
		phase = tfv1.TensorFusionWorkloadPhaseFailed
		readyCondition.Status = metav1.ConditionFalse
		readyCondition.Reason = "WorkerFailed"
		readyCondition.Message = fmt.Sprintf("Failed workers: %d", failedWorkers)
		r.Recorder.Eventf(workload, corev1.EventTypeWarning, "WorkerFailed", "Failed workers: %d", failedWorkers)
	} else {
		phase = tfv1.TensorFusionWorkloadPhasePending
		readyCondition.Status = metav1.ConditionFalse
		readyCondition.Reason = "WaitingForWorkers"
		readyCondition.Message = fmt.Sprintf("Ready replicas: %d/%d", readyReplicas, *workload.Spec.Replicas)
	}
	conditions = append(conditions, readyCondition)

	// Check if we need to update status
	statusChanged := workload.Status.ReadyReplicas != readyReplicas ||
		!equality.Semantic.DeepEqual(workload.Status.WorkerStatuses, workerStatuses) ||
		workload.Status.Phase != phase ||
		!equality.Semantic.DeepEqual(workload.Status.Conditions, conditions)

	if statusChanged {
		log.Info("Updating workload status", "phase", phase, "readyReplicas", readyReplicas, "workerCount", len(workerStatuses))
		workload.Status.Phase = phase
		workload.Status.Conditions = conditions
		workload.Status.ReadyReplicas = readyReplicas
		workload.Status.WorkerStatuses = workerStatuses
		if err := r.Status().Update(ctx, workload); err != nil {
			return fmt.Errorf("update workload status: %w", err)
		}
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *TensorFusionWorkloadReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&tfv1.TensorFusionWorkload{}).
		Owns(&corev1.Pod{}).
		Named("tensorfusionworkload").
		Complete(r)
}
