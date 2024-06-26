package cost

import (
	"strconv"
	"time"

	windtunnelv1alpha1 "github.com/CarnegieMellon-PlantD/PlantD-operator/api/v1alpha1"
	"github.com/CarnegieMellon-PlantD/PlantD-operator/pkg/config"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	image     = config.GetViper().GetString("costService.image")
	redisHost = config.GetViper().GetString("database.redis.host")
	redisPort = config.GetViper().GetInt64("database.redis.port")
)

func init() {
	image = config.GetViper().GetString("costService.image")
}

// CreateJobByCostService creates a Kubernetes Job based on the Cost Service configuration.
func CreateJobByCostService(costService *windtunnelv1alpha1.CostExporter, jobName string, earliestTime time.Time) (*corev1.Pod, error) {

	// Create the Kubernetes Job object
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: costService.Namespace,
			Name:      jobName,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,

			Containers: []corev1.Container{
				{
					Name:            jobName,
					Image:           image,
					ImagePullPolicy: corev1.PullAlways,
					Env: []corev1.EnvVar{
						{
							Name:  "EXPERIMENT_TAGS",
							Value: costService.Status.Tags,
						},
						{
							Name:  "S3_BUCKET_NAME",
							Value: costService.Spec.S3Bucket,
						},
						{
							Name:  "REDIS_HOST",
							Value: redisHost,
						},
						{
							Name:  "REDIS_PORT",
							Value: strconv.FormatInt(redisPort, 10),
						},
						{
							Name:  "CLOUD_SERVICE_PROVIDER",
							Value: costService.Spec.CloudServiceProvider,
						},
						{
							Name:  "EARLIEST_EXPERIMENT",
							Value: earliestTime.Format("2006-01-02 15:04:05"),
						},
						{
							Name: "CSP_CREDENTIALS",
							ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "csp-credentials"},
									Key:                  "CSP_CREDENTIALS",
								},
							},
						},
					},
				},
			},
		},
	}
	return pod, nil
}
