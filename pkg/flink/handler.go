package flink

import (
	"context"
	"strconv"
	"time"

	"github.com/lyft/flyteplugins/go/tasks/pluginmachinery/flytek8s"
	"github.com/lyft/flyteplugins/go/tasks/pluginmachinery/flytek8s/config"

	"github.com/lyft/flyteplugins/go/tasks/errors"
	pluginsCore "github.com/lyft/flyteplugins/go/tasks/pluginmachinery/core"

	"github.com/lyft/flyteplugins/go/tasks/pluginmachinery/k8s"
	"github.com/lyft/flyteplugins/go/tasks/pluginmachinery/utils"
	corev1 "k8s.io/api/core/v1"

	flinkOp "github.com/regadas/flink-on-k8s-operator/api/v1beta1"

	flink "github.com/spotify/flyte-flink-plugin/gen/pb-go/flyteidl-flink"
	"github.com/lyft/flyteidl/gen/pb-go/flyteidl/core"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lyft/flytestdlib/logger"
)

type flinkResourceHandler struct{}

func (flinkResourceHandler) GetProperties() pluginsCore.PluginProperties {
	return pluginsCore.PluginProperties{}
}

// Creates a new Job that will execute the main container as well as any generated types the result from the execution.
func (flinkResourceHandler) BuildResource(ctx context.Context, taskCtx pluginsCore.TaskExecutionContext) (k8s.Resource, error) {
	taskTemplate, err := taskCtx.TaskReader().Read(ctx)
	if err != nil {
		return nil, errors.Errorf(errors.BadTaskSpecification, "unable to fetch task specification [%v]", err.Error())
	} else if taskTemplate == nil {
		return nil, errors.Errorf(errors.BadTaskSpecification, "nil task specification")
	}

	flinkJob := flink.FlinkJob{}
	err = utils.UnmarshalStruct(taskTemplate.GetCustom(), &flinkJob)
	if err != nil {
		return nil, errors.Wrapf(errors.BadTaskSpecification, err, "invalid TaskSpecification [%v], failed to unmarshal", taskTemplate.GetCustom())
	}

	annotations := utils.UnionMaps(
		config.GetK8sPluginConfig().DefaultAnnotations,
		utils.CopyMap(taskCtx.TaskExecutionMetadata().GetAnnotations()),
	)
	labels := utils.UnionMaps(
		config.GetK8sPluginConfig().DefaultLabels,
		utils.CopyMap(taskCtx.TaskExecutionMetadata().GetLabels()),
	)
	container := taskTemplate.GetContainer()
	envVars := flytek8s.DecorateEnvVars(
		ctx,
		flytek8s.ToK8sEnvVar(container.GetEnv()),
		taskCtx.TaskExecutionMetadata().GetTaskExecutionID(),
	)

	flinkEnvVars := make(map[string]string)
	for _, envVar := range envVars {
		flinkEnvVars[envVar.Name] = envVar.Value
	}
	flinkEnvVars["FLYTE_MAX_ATTEMPTS"] = strconv.Itoa(int(taskCtx.TaskExecutionMetadata().GetMaxAttempts()))

	logger.Debugf(ctx, "FlinkEnvVars: %#v", flinkEnvVars)
	logger.Debugf(ctx, "Container %+v", container)

	jobManager := flinkOp.JobManagerSpec{
		PodAnnotations: annotations,
		PodLabels:      labels,
		Ports: flinkOp.JobManagerPorts{
			UI: &jobManagerUIPort,
		},
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("3.5"),
				corev1.ResourceMemory: resource.MustParse("11Gi"),
			},
		},
		Volumes:      cacheVolumes,
		VolumeMounts: cacheVolumeMounts,
	}

	taskManager := flinkOp.TaskManagerSpec{
		PodAnnotations: annotations,
		PodLabels:      labels,
		Replicas:       taskManagerReplicas,
		Volumes:        cacheVolumes,
		VolumeMounts:   cacheVolumeMounts,
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("3.5"),
				corev1.ResourceMemory: resource.MustParse("11Gi"),
			},
		},
	}

	job := flinkOp.JobSpec{
		JarFile:      "/cache/job.jar",
		ClassName:    &flinkJob.MainClass,
		Args:         flinkJob.Args,
		Parallelism:  &jobParallelism,
		Volumes:      cacheVolumes,
		VolumeMounts: cacheVolumeMounts,
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
		},
		InitContainers: []corev1.Container{
			{
				Name:    "gcs-downloader",
				Image:   "google/cloud-sdk",
				Command: []string{"gsutil"},
				Args: []string{
					"cp",
					flinkJob.JarFile,
					"/cache/job.jar",
				},
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("1"),
						corev1.ResourceMemory: resource.MustParse("1Gi"),
					},
				},
			},
		},
	}

	// Start with default config values.
	flinkProperties := make(map[string]string)
	for k, v := range GetFlinkConfig().DefaultFlinkConfig {
		flinkProperties[k] = v
	}

	for k, v := range flinkJob.GetFlinkProperties() {
		flinkProperties[k] = v
	}

	fc := &flinkOp.FlinkCluster{
		TypeMeta: metav1.TypeMeta{
			Kind:       KindFlinkCluster,
			APIVersion: flinkOp.GroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Annotations: annotations,
			Labels:      labels,
		},
		Spec: flinkOp.FlinkClusterSpec{
			ServiceAccountName: &serviceAccount,
			Image: flinkOp.ImageSpec{
				Name:       flinkImage,
				PullPolicy: corev1.PullAlways,
			},
			JobManager:      jobManager,
			TaskManager:     taskManager,
			Job:             &job,
			FlinkProperties: flinkProperties,
		},
	}

	return fc, nil
}

func (flinkResourceHandler) BuildIdentityResource(ctx context.Context, taskCtx pluginsCore.TaskExecutionMetadata) (k8s.Resource, error) {
	return &flinkOp.FlinkCluster{
		TypeMeta: metav1.TypeMeta{
			Kind:       KindFlinkCluster,
			APIVersion: flinkOp.GroupVersion.String(),
		},
	}, nil
}

func getEventInfoForFlink(fc *flinkOp.FlinkCluster) (*pluginsCore.TaskInfo, error) {
	var taskLogs []*core.TaskLog
	customInfoMap := make(map[string]string)

	customInfo, err := utils.MarshalObjToStruct(customInfoMap)
	if err != nil {
		return nil, err
	}

	return &pluginsCore.TaskInfo{
		Logs:       taskLogs,
		CustomInfo: customInfo,
	}, nil
}

func (r flinkResourceHandler) GetTaskPhase(ctx context.Context, pluginContext k8s.PluginContext, resource k8s.Resource) (pluginsCore.PhaseInfo, error) {
	app := resource.(*flinkOp.FlinkCluster)
	info, err := getEventInfoForFlink(app)
	if err != nil {
		return pluginsCore.PhaseInfoUndefined, err
	}

	occurredAt := time.Now()

	logger.Infof(ctx, "cluster_state: %s", app.Status.State)

	switch app.Status.State {
	case flinkOp.ClusterStateCreating, flinkOp.ClusterStateReconciling, flinkOp.ClusterStateUpdating:
		return pluginsCore.PhaseInfoWaitingForResources(occurredAt, pluginsCore.DefaultPhaseVersion, "cluster starting"), nil
	case flinkOp.ClusterStateRunning:
		jobStatus := app.Status.Components.Job
		logger.Infof(ctx, "job_state: %s", jobStatus.State)

		switch jobStatus.State {
		case flinkOp.JobStateFailed:
			return pluginsCore.PhaseInfoFailure(errors.DownstreamSystemError, "job failed", info), nil
		case flinkOp.JobStateRunning:
			return pluginsCore.PhaseInfoInitializing(occurredAt, pluginsCore.DefaultPhaseVersion, "job submitted", info), nil
		case flinkOp.JobStateSucceeded:
			return pluginsCore.PhaseInfoSuccess(info), nil
		}

		return pluginsCore.PhaseInfoInitializing(occurredAt, pluginsCore.DefaultPhaseVersion, "job submitted", info), nil
	case flinkOp.ClusterStateStopped, flinkOp.ClusterStateStopping, flinkOp.ClusterStatePartiallyStopped:
		return pluginsCore.PhaseInfoSuccess(info), nil
	}

	return pluginsCore.PhaseInfoRunning(pluginsCore.DefaultPhaseVersion, info), nil
}
