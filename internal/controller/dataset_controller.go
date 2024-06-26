package controller

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	kbatch "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgo "k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	windtunnelv1alpha1 "github.com/CarnegieMellon-PlantD/PlantD-operator/api/v1alpha1"
	"github.com/CarnegieMellon-PlantD/PlantD-operator/pkg/datagen"
	"github.com/CarnegieMellon-PlantD/PlantD-operator/pkg/utils"
)

const (
	dataSetLogsTimeout     = 30 * time.Second
	dataSetPollingInterval = 2 * time.Second
)

// DataSetReconciler reconciles a DataSet object
type DataSetReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	CGClient *clientgo.Clientset
}

//+kubebuilder:rbac:groups=windtunnel.plantd.org,resources=datasets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=windtunnel.plantd.org,resources=datasets/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=windtunnel.plantd.org,resources=datasets/finalizers,verbs=update
//+kubebuilder:rbac:groups=windtunnel.plantd.org,resources=schemas,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=pods/log,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.15.0/pkg/reconcile
func (r *DataSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the requested DataSet
	dataSet := &windtunnelv1alpha1.DataSet{}
	if err := r.Get(ctx, req.NamespacedName, dataSet); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Unable to fetch DataSet")
		return ctrl.Result{}, err
	}

	// Create or update PVC and Job
	// dataSet.Generation is used to track the change in dataSet.Spec
	// Once the spec is created/updated, we create new PVC & Job, delete the old PVC & Job
	if dataSet.Generation != dataSet.Status.LastGeneration {
		return r.reconcileCreatedOrUpdated(ctx, dataSet)
	}

	// Fetch the current PVC & Job, check the Job status, and update the DataSet status
	if dataSet.Status.JobStatus == windtunnelv1alpha1.DataSetJobRunning {
		return r.reconcileRunning(ctx, dataSet)
	}

	// DataSet is not created/updated, and it is not running, no action needed
	return ctrl.Result{}, nil
}

// reconcileCreatedOrUpdated reconciles the DataSet when it is created or updated.
func (r *DataSetReconciler) reconcileCreatedOrUpdated(ctx context.Context, dataSet *windtunnelv1alpha1.DataSet) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Reset all the status fields
	dataSet.Status.JobStatus = ""
	dataSet.Status.PVCStatus = ""
	dataSet.Status.StartTime = nil
	dataSet.Status.CompletionTime = nil
	dataSet.Status.ErrorCount = 0
	dataSet.Status.Errors = nil

	// Get all Schemas
	schemaMap := make(map[string]*windtunnelv1alpha1.Schema, len(dataSet.Spec.Schemas))
	for _, schema := range dataSet.Spec.Schemas {
		s := &windtunnelv1alpha1.Schema{}
		schemaName := types.NamespacedName{Namespace: dataSet.Namespace, Name: schema.Name}
		if err := r.Get(ctx, schemaName, s); err != nil {
			logger.Error(err, fmt.Sprintf("Cannot get Schema: %s", schemaName))
			dataSet.Status.JobStatus = windtunnelv1alpha1.DataSetJobFailed
			dataSet.Status.ErrorCount = 1
			dataSet.Status.Errors = map[windtunnelv1alpha1.DataSetErrorType][]string{
				windtunnelv1alpha1.DataSetControllerError: {
					fmt.Sprintf("Cannot find Schema: %s", err),
				},
			}
			if err := r.Status().Update(ctx, dataSet); err != nil {
				logger.Error(err, "Cannot update the status")
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		schemaMap[schema.Name] = s
	}

	// Delete the Job from last generation if exists
	lastJobName := utils.GetDataSetJobName(dataSet.Name, dataSet.Status.LastGeneration)
	lastJob := &kbatch.Job{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: dataSet.Namespace, Name: lastJobName}, lastJob); err == nil {
		// By default, the Pod of the Job will be reserved after the Job is deleted,
		// and Kubernetes will raise a warning.
		// Set the propagation policy to "Background" to avoid the warning and delete the Pod.
		propagationPolicy := metav1.DeletePropagationBackground
		if err := r.Delete(ctx, lastJob, &client.DeleteOptions{PropagationPolicy: &propagationPolicy}); err != nil {
			logger.Error(err, "Cannot delete Job from last generation")
			return ctrl.Result{}, err
		}
		logger.Info(fmt.Sprintf("Job deleted: %s", lastJobName))
	}

	// Delete the PVC from last generation if exists, it will delete the PV as well
	lastPVCName := utils.GetDataSetPVCName(dataSet.Name, dataSet.Status.LastGeneration)
	lastPVC := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: dataSet.Namespace, Name: lastPVCName}, lastPVC); err == nil {
		if err := r.Delete(ctx, lastPVC); err != nil {
			logger.Error(err, "Cannot delete PVC from last generation")
			return ctrl.Result{}, err
		}
		logger.Info(fmt.Sprintf("PVC deleted: %s", lastPVCName))
	}

	// Create a new PVC
	newPVCName := utils.GetDataSetPVCName(dataSet.Name, dataSet.Generation)
	newPVC := datagen.CreatePVC(newPVCName, dataSet)
	if err := ctrl.SetControllerReference(dataSet, newPVC, r.Scheme); err != nil {
		logger.Error(err, "Cannot set controller reference for PVC")
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, newPVC); client.IgnoreAlreadyExists(err) != nil {
		logger.Error(err, "Cannot create PVC")
		return ctrl.Result{}, err
	} else if err == nil {
		logger.Info(fmt.Sprintf("PVC created: %s", newPVCName))
	}

	// Create a new job
	newJobName := utils.GetDataSetJobName(dataSet.Name, dataSet.Generation)
	newJob, err := datagen.CreateJob(newJobName, newPVCName, dataSet, schemaMap)
	if err != nil {
		logger.Error(err, "Cannot prepare Job object to create")
		return ctrl.Result{}, err
	}
	if err := ctrl.SetControllerReference(dataSet, newJob, r.Scheme); err != nil {
		logger.Error(err, "Cannot set controller reference for Job")
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, newJob); client.IgnoreAlreadyExists(err) != nil {
		logger.Error(err, "Cannot create Job")
		return ctrl.Result{}, err
	} else if err == nil {
		logger.Info(fmt.Sprintf("Job created: %s", newJobName))
	}

	// Update the last generation and Job status
	dataSet.Status.LastGeneration = dataSet.Generation
	dataSet.Status.JobStatus = windtunnelv1alpha1.DataSetJobRunning
	if err := r.Status().Update(ctx, dataSet); err != nil {
		logger.Error(err, "Cannot update the status")
		return ctrl.Result{}, err
	}

	// Requeue the request to check the Job status
	return ctrl.Result{RequeueAfter: dataSetPollingInterval}, nil
}

// reconcileRunning reconciles the DataSet when it is running.
func (r *DataSetReconciler) reconcileRunning(ctx context.Context, dataSet *windtunnelv1alpha1.DataSet) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Get the Job
	jobName := utils.GetDataSetJobName(dataSet.Name, dataSet.Generation)
	job := &kbatch.Job{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: dataSet.Namespace, Name: jobName}, job); err != nil {
		logger.Error(err, fmt.Sprintf("Lost Job: %s", jobName))
		return ctrl.Result{}, err
	}

	// Get the PVC
	pvcName := utils.GetDataSetPVCName(dataSet.Name, dataSet.Generation)
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: dataSet.Namespace, Name: pvcName}, pvc); err != nil {
		logger.Error(err, fmt.Sprintf("Lost PVC: %s", pvcName))
		return ctrl.Result{}, err
	}

	// Update the PVC status
	dataSet.Status.PVCStatus = pvc.Status.Phase

	// Update start time and completion time
	dataSet.Status.StartTime = job.Status.StartTime
	dataSet.Status.CompletionTime = job.Status.CompletionTime

	// Check if the Job is finished, and update the status accordingly
	jobFinished, jobConditionType := isJobFinished(job)
	if jobFinished {
		logger.Info(fmt.Sprintf("Job is finished: %s", jobName))
		switch jobConditionType {
		case kbatch.JobComplete:
			dataSet.Status.JobStatus = windtunnelv1alpha1.DataSetJobSuccess
		case kbatch.JobFailed:
			// Get logs from the Job
			jobLogs, err := r.getJobLogs(ctx, job)
			if err != nil {
				logger.Error(err, "Cannot get Job logs")
				dataSet.Status.JobStatus = windtunnelv1alpha1.DataSetJobFailed
				dataSet.Status.ErrorCount = 1
				dataSet.Status.Errors = map[windtunnelv1alpha1.DataSetErrorType][]string{
					windtunnelv1alpha1.DataSetControllerError: {
						fmt.Sprintf("Job finished but cannot get Job logs: %s", err),
					},
				}
			} else {
				dataSet.Status.JobStatus = windtunnelv1alpha1.DataSetJobFailed
				dataSet.Status.ErrorCount = int32(len(jobLogs))
				dataSet.Status.Errors = map[windtunnelv1alpha1.DataSetErrorType][]string{
					windtunnelv1alpha1.DataSetJobError: jobLogs,
				}
			}
		}

	}

	if err := r.Status().Update(ctx, dataSet); err != nil {
		logger.Error(err, "Cannot update the status")
		return ctrl.Result{}, err
	}

	if jobFinished {
		// Job is finished, no need to requeue
		return ctrl.Result{}, nil
	} else {
		// Job is still running, requeue the request
		return ctrl.Result{RequeueAfter: dataSetPollingInterval}, nil
	}
}

// isJobFinished checks if the Job is finished, and returns the condition type if it is finished.
func isJobFinished(job *kbatch.Job) (bool, kbatch.JobConditionType) {
	for _, c := range job.Status.Conditions {
		if (c.Type == kbatch.JobComplete || c.Type == kbatch.JobFailed) && c.Status == corev1.ConditionTrue {
			return true, c.Type
		}
	}
	return false, ""
}

// getContainerLogs gets the logs of a container in a Pod.
func (r *DataSetReconciler) getContainerLogs(ctx context.Context, pod *corev1.Pod, containerName string) (string, error) {
	// Open a stream for the Pod logs
	req := r.CGClient.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: containerName,
	})
	podLogs, err := req.Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to open stream: %w", err)
	}
	defer podLogs.Close()

	// Read the logs from the stream
	buf := new(bytes.Buffer)
	_, err = io.Copy(buf, podLogs)
	if err != nil {
		return "", fmt.Errorf("failed to read from stream: %w", err)
	}

	return buf.String(), nil
}

// getPodLogs gets the logs of all containers in a Pod.
func (r *DataSetReconciler) getPodLogs(ctx context.Context, pod *corev1.Pod) ([]string, error) {
	result := make([]string, 0)
	for _, container := range pod.Spec.Containers {
		containerLog, err := r.getContainerLogs(ctx, pod, container.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to get container logs: %w", err)
		}
		result = append(result, containerLog)
	}
	return result, nil
}

// getJobLogs gets the logs of all Pods in a Job.
func (r *DataSetReconciler) getJobLogs(ctx context.Context, job *kbatch.Job) ([]string, error) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(job.Namespace)); err != nil {
		return nil, fmt.Errorf("failed to list Pods: %w", err)
	}

	// Create a new context to ensure it is not affected by the parent context cancellation
	// This is to avoid any rate limiting issues from Kubernetes API
	jobLogsCtx, cancel := context.WithTimeout(context.Background(), dataSetLogsTimeout)
	defer cancel()

	result := make([]string, 0)
	for _, pod := range podList.Items {
		// Skip if the Pod does not belong to the Job
		if !metav1.IsControlledBy(&pod, job) {
			continue
		}
		podLogs, err := r.getPodLogs(jobLogsCtx, &pod)
		if err != nil {
			return nil, fmt.Errorf("failed to get Pod logs: %w", err)
		}
		result = append(result, podLogs...)
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("no logs found")
	}
	return result, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DataSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&windtunnelv1alpha1.DataSet{}).
		Complete(r)
}
