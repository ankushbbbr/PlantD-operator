package datagen

import (
	"encoding/json"
	"strconv"

	kbatch "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	windtunnelv1alpha1 "github.com/CarnegieMellon-PlantD/PlantD-operator/api/v1alpha1"
	"github.com/CarnegieMellon-PlantD/PlantD-operator/pkg/config"
)

var (
	defaultImage       = config.GetViper().GetString("dataGenerator.defaultImage")
	defaultParallelism = config.GetViper().GetInt32("dataGenerator.defaultParallelism")
	backoffLimit       = config.GetViper().GetInt32("dataGenerator.backoffLimit")
	defaultStorageSize = config.GetViper().GetString("dataGenerator.defaultStorageSize")
	path               = config.GetViper().GetString("dataGenerator.path")
)

// CreateJob creates a Job based on the DataSet configuration.
func CreateJob(jobName string, pvcName string, dataSet *windtunnelv1alpha1.DataSet, schemaMap map[string]*windtunnelv1alpha1.Schema) (*kbatch.Job, error) {
	// Calculate the number of parallel jobs and step size
	parallelism := dataSet.Spec.Parallelism
	if parallelism == 0 {
		parallelism = defaultParallelism
	}
	stepSize := dataSet.Spec.NumberOfFiles / parallelism

	image := dataSet.Spec.Image
	if image == "" {
		image = defaultImage
	}

	// Marshal dataset and schema map to JSON
	datasetBytes, err := json.Marshal(dataSet)
	if err != nil {
		return nil, err
	}

	schemaMapBytes, err := json.Marshal(schemaMap)
	if err != nil {
		return nil, err
	}

	// Create the Kubernetes Job object
	job := &kbatch.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: dataSet.Namespace,
			Name:      jobName,
		},
		Spec: kbatch.JobSpec{
			CompletionMode: ptr.To(kbatch.IndexedCompletion),
			Completions:    &parallelism,
			Parallelism:    &parallelism,
			BackoffLimit:   &backoffLimit,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:  "data-generator",
							Image: image,
							Env: []corev1.EnvVar{
								{
									Name:  "JOB_STEP_SIZE",
									Value: strconv.FormatInt(int64(stepSize), 10),
								},
								{
									Name:  "TOTAL_REPEAT",
									Value: strconv.FormatInt(int64(dataSet.Spec.NumberOfFiles), 10),
								},
								{
									Name:  "DATASET",
									Value: string(datasetBytes),
								},
								{
									Name:  "SCHEMA_MAP",
									Value: string(schemaMapBytes),
								},
								{
									Name:  "OUTPUT_PATH",
									Value: path,
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "data-volume",
									MountPath: path,
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "data-volume",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: pvcName,
								},
							},
						},
					},
				},
			},
		},
	}
	return job, nil
}

// CreatePVC creates a PersistentVolumeClaim for the data generator job.
func CreatePVC(pvcName string, dataSet *windtunnelv1alpha1.DataSet) *corev1.PersistentVolumeClaim {
	storageSize := dataSet.Spec.StorageSize
	if storageSize.IsZero() {
		storageSize = resource.MustParse(defaultStorageSize)
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: dataSet.Namespace,
			Name:      pvcName,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: storageSize,
				},
			},
		},
	}

	return pvc
}
