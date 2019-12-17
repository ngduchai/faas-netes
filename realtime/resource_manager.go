package realtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"realtime"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/openfaas/faas/gateway/requests"
	apiv1 "k8s.io/api/core/v1"
	corev1 "k8s.io/api/core/v1"
	v1beta1 "k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
)

// watchdogPort for the OpenFaaS function watchdog
const watchdogPort = 8080

// initialReplicasCount how many replicas to start of creating for a function
const initialReplicasCount = 1

// nonRootFunctionuserID is the user id that is set when DeployHandlerConfig.SetNonRootUser is true.
// value >10000 per the suggestion from https://kubesec.io/basics/containers-securitycontext-runasuser/
const nonRootFunctionuserID = 12000

// Regex for RFC-1123 validation:
// 	k8s.io/kubernetes/pkg/util/validation/validation.go
var validDNS = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ValidateDeployRequest validates that the service name is valid for Kubernetes
func ValidateDeployRequest(request *requests.CreateFunctionRequest) error {
	matched := validDNS.MatchString(request.Service)
	if matched {
		return nil
	}

	return fmt.Errorf("(%s) must be a valid DNS entry for service name", request.Service)
}

// FunctionProbeConfig specify options for Liveliness and Readiness checks
type FunctionProbeConfig struct {
	InitialDelaySeconds int32
	TimeoutSeconds      int32
	PeriodSeconds       int32
}

// DeployHandlerConfig specify options for Deployments
type DeployHandlerConfig struct {
	HTTPProbe                    bool
	FunctionReadinessProbeConfig *FunctionProbeConfig
	FunctionLivenessProbeConfig  *FunctionProbeConfig
	ImagePullPolicy              string
	// SetNonRootUser will override the function image user to ensure that it is not root. When
	// true, the user will set to 12000 for all functions.
	SetNonRootUser bool
}

type ResourceManager struct{}

func (rm ResourceManager) createImage(
	request *requests.CreateFunctionRequest,
	functionNamespace string,
	clientset *kubernetes.Clientset,
	config *DeployHandlerConfig,
	int32 numReplicas) (int, error) {

	// Validate the request
	if err := ValidateDeployRequest(&request); err != nil {
		return http.StatusBadRequest, err
	}

	// Get secrets
	existingSecrets, err := getSecrets(clientset, functionNamespace, request.Secrets)
	if err != nil {
		return http.StatusBadRequest, err
	}

	// Create deployment
	deploymentSpec, specErr := makeDeploymentSpec(request, numReplicas, existingSecrets, config)

	if specErr != nil {
		return http.StatusBadRequest, err
	}
	_, err = deploy.Create(deploymentSpec)
	if err != nil {
		log.Println(err)
		return http.StatusInternalServerError, err
	}

	// create service
	service := clientset.Core().Services(functionNamespace)
	serviceSpec := makeServiceSpec(request)
	_, err = service.Create(serviceSpec)

	if err != nil {
		return http.StatusInternalServerError, err
	}

	return http.StatusAccepted, nil

}

func (rm ResourceManager) removeImage(
	request *requests.DeleteFunctionRequest,
	functionNamespace string,
	clientset *kubernetes.Clientset,
) (int, error) {

	getOpts := metav1.GetOptions{}

	// This makes sure we don't delete non-labelled deployments
	deployment, findDeployErr := clientset.ExtensionsV1beta1().
		Deployments(functionNamespace).
		Get(request.FunctionName, getOpts)

	if findDeployErr != nil {
		code := http.StatusInternalServerError
		if errors.IsNotFound(findDeployErr) {
			code = http.StatusNotFound
		}
		return code, findDeployErr.Error()
	}

	if isFunction(deployment) {
		foregroundPolicy := metav1.DeletePropagationForeground
		opts := &metav1.DeleteOptions{PropagationPolicy: &foregroundPolicy}

		if deployErr := clientset.ExtensionsV1beta1().
			Deployments(functionNamespace).
			Delete(request.FunctionName, opts); deployErr != nil {

			code := http.StatusInternalServerError
			if errors.IsNotFound(deployErr) {
				code = http.StatusNotFound
			}
			return code, deployErr
		}

		if svcErr := clientset.CoreV1().
			Services(functionNamespace).
			Delete(request.FunctionName, opts); svcErr != nil {

			code := http.StatusInternalServerError
			if errors.IsNotFound(svcErr) {
				code = http.StatusNotFound
			}
			return code, svcErr
		}
		return http.StatusAccepted, nil
	} else {
		return http.StatusBadRequest,
			errors.New("Not a function: " + request.FunctionName)
	}
}

func (rm ResourceManager) updateImage(
	request *requests.CreateFunctionRequest,
	functionNamespace string,
	clientset *kubernetes.Clientset,
	config *DeployHandlerConfig,
	int32 numReplicas) (int, error) {

	annotations := buildAnnotations(request)
	if err, status := updateDeploymentSpec(functionNamespace, clientset,
		request, annotations, numReplicas, config); err != nil {
		return status, err
	}

	if err, status := updateService(functionNamespace, clientset,
		request, annotations); err != nil {
		return status, err
	}

	return http.StatusAccepted, nil
}

// Return realtime, functionsize (= cpus), and duration
func (rm ResourceManager) RequestRealtimeParams(
	request *CreateFunctionRequest) (float64, float64, uint64) {
	if request.Labels == nil {
		// Return default value
		return 0.0, 1.0, 60
	}
	realtime := extractLabelRealValue((*request.Labels)["realtime"], float64(0))
	size := extractLabelRealValue((*request.Labels)["functionsize"], float64(1.0))
	duration := extractLabelValue((*request.Labels)["duration"], uint64(60))
	return realtime, size, duration

}

func (rm ResourceManager) Scale(request *CreateFunctionRequest, n int) error {

}

type FunctionProbes struct {
	Liveness  *apiv1.Probe
	Readiness *apiv1.Probe
}

func (rm ResourceManager) DeploymentRealtimeParams(
	functionName string,
	clientset *kubernetes.Clientset) (float64, float64, uint64, int, error) {
	deploy := clientset.Extensions().Deployments(functionNamespace)

	getopt := metav1.GetOptions{}
	dep, err := deploy.Get(functionName, getopt)
	if err != nil {
		return 0.0, 0.0, 0, 0, err
	}
	realtime := deployment.Labels["realtime"]
	size := deployment.Labels["functionsize"]
	duration := deployment.Labels["duration"]
	availReplicas := *(*dep).Status.AvailableReplicas

	return realtime, size, duration, availReplicas, nil
}

func makeProbes(config *DeployHandlerConfig) *FunctionProbes {
	var handler apiv1.Handler

	if config.HTTPProbe {
		handler = apiv1.Handler{
			HTTPGet: &apiv1.HTTPGetAction{
				Path: "/_/health",
				Port: intstr.IntOrString{
					Type:   intstr.Int,
					IntVal: int32(watchdogPort),
				},
			},
		}
	} else {
		path := filepath.Join(os.TempDir(), ".lock")
		handler = apiv1.Handler{
			Exec: &apiv1.ExecAction{
				Command: []string{"cat", path},
			},
		}
	}

	probes := FunctionProbes{}
	probes.Readiness = &apiv1.Probe{
		Handler:             handler,
		InitialDelaySeconds: config.FunctionReadinessProbeConfig.InitialDelaySeconds,
		TimeoutSeconds:      config.FunctionReadinessProbeConfig.TimeoutSeconds,
		PeriodSeconds:       config.FunctionReadinessProbeConfig.PeriodSeconds,
		SuccessThreshold:    1,
		FailureThreshold:    3,
	}

	probes.Liveness = &apiv1.Probe{
		Handler:             handler,
		InitialDelaySeconds: config.FunctionLivenessProbeConfig.InitialDelaySeconds,
		TimeoutSeconds:      config.FunctionLivenessProbeConfig.TimeoutSeconds,
		PeriodSeconds:       config.FunctionLivenessProbeConfig.PeriodSeconds,
		SuccessThreshold:    1,
		FailureThreshold:    3,
	}

	return &probes
}

func makeDeploymentSpec(
	request requests.CreateFunctionRequest,
	numReplicas int32,
	existingSecrets map[string]*apiv1.Secret,
	config *DeployHandlerConfig) (*v1beta1.Deployment, error) {
	envVars := buildEnvVars(&request)

	initialReplicas := int32p(numReplicas)
	labels := map[string]string{
		"faas_function": request.Service,
	}

	if request.Labels != nil {
		/*
			    if min := getMinReplicaCount(*request.Labels); min != nil {
					initialReplicas = min
				}
		*/
		for k, v := range *request.Labels {
			labels[k] = v
		}
	}

	nodeSelector := createSelector(request.Constraints)

	resources, resourceErr := createResources(request)

	if resourceErr != nil {
		return nil, resourceErr
	}

	var imagePullPolicy apiv1.PullPolicy
	switch config.ImagePullPolicy {
	case "Never":
		imagePullPolicy = apiv1.PullNever
	case "IfNotPresent":
		imagePullPolicy = apiv1.PullIfNotPresent
	default:
		imagePullPolicy = apiv1.PullAlways
	}

	annotations := buildAnnotations(request)

	var serviceAccount string

	if request.Annotations != nil {
		annotations := *request.Annotations
		if val, ok := annotations["com.openfaas.serviceaccount"]; ok && len(val) > 0 {
			serviceAccount = val
		}
	}

	probes := makeProbes(config)

	deploymentSpec := &v1beta1.Deployment{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Deployment",
			APIVersion: "extensions/v1beta1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        request.Service,
			Annotations: annotations,
		},
		Spec: v1beta1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"faas_function": request.Service,
				},
			},
			Replicas: initialReplicas,
			Strategy: v1beta1.DeploymentStrategy{
				Type: v1beta1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &v1beta1.RollingUpdateDeployment{
					MaxUnavailable: &intstr.IntOrString{
						Type:   intstr.Int,
						IntVal: int32(0),
					},
					MaxSurge: &intstr.IntOrString{
						Type:   intstr.Int,
						IntVal: int32(1),
					},
				},
			},
			RevisionHistoryLimit: int32p(10),
			Template: apiv1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Name:        request.Service,
					Labels:      labels,
					Annotations: annotations,
				},
				Spec: apiv1.PodSpec{
					NodeSelector: nodeSelector,
					Containers: []apiv1.Container{
						{
							Name:  request.Service,
							Image: request.Image,
							Ports: []apiv1.ContainerPort{
								{ContainerPort: int32(watchdogPort), Protocol: corev1.ProtocolTCP},
							},
							Env:             envVars,
							Resources:       *resources,
							ImagePullPolicy: imagePullPolicy,
							LivenessProbe:   probes.Liveness,
							ReadinessProbe:  probes.Readiness,
							SecurityContext: &corev1.SecurityContext{
								ReadOnlyRootFilesystem: &request.ReadOnlyRootFilesystem,
							},
						},
					},
					ServiceAccountName: serviceAccount,
					RestartPolicy:      corev1.RestartPolicyAlways,
					DNSPolicy:          corev1.DNSClusterFirst,
				},
			},
		},
	}

	configureReadOnlyRootFilesystem(request, deploymentSpec)
	configureContainerUserID(deploymentSpec, nonRootFunctionuserID, config)

	if err := UpdateSecrets(request, deploymentSpec, existingSecrets); err != nil {
		return nil, err
	}

	return deploymentSpec, nil
}

func makeServiceSpec(request requests.CreateFunctionRequest) *corev1.Service {

	serviceSpec := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        request.Service,
			Annotations: buildAnnotations(request),
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Selector: map[string]string{
				"faas_function": request.Service,
			},
			Ports: []corev1.ServicePort{
				{
					Name:     "http",
					Protocol: corev1.ProtocolTCP,
					Port:     watchdogPort,
					TargetPort: intstr.IntOrString{
						Type:   intstr.Int,
						IntVal: int32(watchdogPort),
					},
				},
			},
		},
	}

	return serviceSpec
}

func buildAnnotations(request requests.CreateFunctionRequest) map[string]string {
	var annotations map[string]string
	if request.Annotations != nil {
		annotations = *request.Annotations
	} else {
		annotations = map[string]string{}
	}

	annotations["prometheus.io.scrape"] = "false"
	return annotations
}

func updateDeploymentSpec(
	functionNamespace string,
	clientset *kubernetes.Clientset,
	request requests.CreateFunctionRequest,
	numReplicas int,
	annotations map[string]string,
	config *DeployHandlerConfig) (err error, httpStatus int) {

	getOpts := metav1.GetOptions{}

	deployment, findDeployErr := clientset.ExtensionsV1beta1().
		Deployments(functionNamespace).
		Get(request.Service, getOpts)

	if findDeployErr != nil {
		return findDeployErr, http.StatusNotFound
	}

	if len(deployment.Spec.Template.Spec.Containers) > 0 {
		deployment.Spec.Template.Spec.Containers[0].Image = request.Image

		// Disabling update support to prevent unexpected mutations of deployed functions,
		// since imagePullPolicy is now configurable. This could be reconsidered later depending
		// on desired behavior, but will need to be updated to take config.
		//deployment.Spec.Template.Spec.Containers[0].ImagePullPolicy = v1.PullAlways

		deployment.Spec.Template.Spec.Containers[0].Env = buildEnvVars(&request)

		configureReadOnlyRootFilesystem(request, deployment)
		configureContainerUserID(deployment, nonRootFunctionuserID, config)

		deployment.Spec.Template.Spec.NodeSelector = createSelector(request.Constraints)

		labels := map[string]string{
			"faas_function": request.Service,
			"uid":           fmt.Sprintf("%d", time.Now().Nanosecond()),
		}

		if request.Labels != nil {
			/*
				if min := getMinReplicaCount(*request.Labels); min != nil {
					deployment.Spec.Replicas = min
				}
			*/
			for k, v := range *request.Labels {
				labels[k] = v
			}
			deployment.Spec.Replicas = &numReplicas
		}

		deployment.Labels = labels
		deployment.Spec.Template.ObjectMeta.Labels = labels

		deployment.Annotations = annotations
		deployment.Spec.Template.Annotations = annotations
		deployment.Spec.Template.ObjectMeta.Annotations = annotations

		resources, resourceErr := createResources(request)
		if resourceErr != nil {
			return resourceErr, http.StatusBadRequest
		}

		deployment.Spec.Template.Spec.Containers[0].Resources = *resources

		var serviceAccount string

		if request.Annotations != nil {
			annotations := *request.Annotations
			if val, ok := annotations["com.openfaas.serviceaccount"]; ok && len(val) > 0 {
				serviceAccount = val
			}
		}

		deployment.Spec.Template.Spec.ServiceAccountName = serviceAccount

		existingSecrets, err := getSecrets(clientset, functionNamespace, request.Secrets)
		if err != nil {
			return err, http.StatusBadRequest
		}

		err = UpdateSecrets(request, deployment, existingSecrets)
		if err != nil {
			log.Println(err)
			return err, http.StatusBadRequest
		}

		probes := makeProbes(config)
		deployment.Spec.Template.Spec.Containers[0].LivenessProbe = probes.Liveness
		deployment.Spec.Template.Spec.Containers[0].ReadinessProbe = probes.Readiness
	}

	if _, updateErr := clientset.ExtensionsV1beta1().
		Deployments(functionNamespace).
		Update(deployment); updateErr != nil {

		return updateErr, http.StatusInternalServerError
	}
}

func updateService(
	functionNamespace string,
	clientset *kubernetes.Clientset,
	request requests.CreateFunctionRequest,
	annotations map[string]string) (err error, httpStatus int) {

	getOpts := metav1.GetOptions{}

	service, findServiceErr := clientset.CoreV1().
		Services(functionNamespace).
		Get(request.Service, getOpts)

	if findServiceErr != nil {
		return findServiceErr, http.StatusNotFound
	}

	service.Annotations = annotations

	if _, updateErr := clientset.CoreV1().
		Services(functionNamespace).
		Update(service); updateErr != nil {

		return updateErr, http.StatusInternalServerError
	}

	return nil, http.StatusAccepted
}

func buildEnvVars(request *requests.CreateFunctionRequest) []corev1.EnvVar {
	envVars := []corev1.EnvVar{}

	if len(request.EnvProcess) > 0 {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "fprocess",
			Value: request.EnvProcess,
		})
	}

	for k, v := range request.EnvVars {
		envVars = append(envVars, corev1.EnvVar{
			Name:  k,
			Value: v,
		})
	}

	return envVars
}

func int32p(i int32) *int32 {
	return &i
}

func createSelector(constraints []string) map[string]string {
	selector := make(map[string]string)

	log.Println(constraints)
	if len(constraints) > 0 {
		for _, constraint := range constraints {
			parts := strings.Split(constraint, "=")

			if len(parts) == 2 {
				selector[parts[0]] = parts[1]
			}
		}
	}

	// log.Println("selector: ", selector)
	return selector
}

func createResources(request requests.CreateFunctionRequest) (*apiv1.ResourceRequirements, error) {
	resources := &apiv1.ResourceRequirements{
		Limits:   apiv1.ResourceList{},
		Requests: apiv1.ResourceList{},
	}

	// Set Memory limits
	if request.Limits != nil && len(request.Limits.Memory) > 0 {
		qty, err := resource.ParseQuantity(request.Limits.Memory)
		if err != nil {
			return resources, err
		}
		resources.Limits[apiv1.ResourceMemory] = qty
	}

	if request.Requests != nil && len(request.Requests.Memory) > 0 {
		qty, err := resource.ParseQuantity(request.Requests.Memory)
		if err != nil {
			return resources, err
		}
		resources.Requests[apiv1.ResourceMemory] = qty
	}

	// Set CPU limits
	if request.Limits != nil && len(request.Limits.CPU) > 0 {
		qty, err := resource.ParseQuantity(request.Limits.CPU)
		if err != nil {
			return resources, err
		}
		resources.Limits[apiv1.ResourceCPU] = qty
	}

	if request.Requests != nil && len(request.Requests.CPU) > 0 {
		qty, err := resource.ParseQuantity(request.Requests.CPU)
		if err != nil {
			return resources, err
		}
		resources.Requests[apiv1.ResourceCPU] = qty
	}

	return resources, nil
}

func getMinReplicaCount(labels map[string]string) *int32 {
	if value, exists := labels["com.openfaas.scale.min"]; exists {
		minReplicas, err := strconv.Atoi(value)
		if err == nil && minReplicas > 0 {
			return int32p(int32(minReplicas))
		}

		log.Println(err)
	}

	return nil
}

func extractLabelValue(rawLabelValue string, fallback uint64) uint64 {
	if len(rawLabelValue) <= 0 {
		return fallback
	}

	value, err := strconv.Atoi(rawLabelValue)

	if err != nil {
		log.Printf("Provided label value %s should be of type uint", rawLabelValue)
		return fallback
	}

	return uint64(value)
}

func extractLabelRealValue(rawLabelValue string, fallback float64) float64 {
	if len(rawLabelValue) <= 0 {
		return fallback
	}

	value, err := strconv.ParseFloat(rawLabelValue, 64)

	if err != nil {
		log.Printf("Provided label value %s should be of type float", rawLabelValue)
		return fallback
	}

	return float64(value)
}

// configureReadOnlyRootFilesystem will create or update the required settings and mounts to ensure
// that the ReadOnlyRootFilesystem setting works as expected, meaning:
// 1. when ReadOnlyRootFilesystem is true, the security context of the container will have ReadOnlyRootFilesystem also
//    marked as true and a new `/tmp` folder mount will be added to the deployment spec
// 2. when ReadOnlyRootFilesystem is false, the security context of the container will also have ReadOnlyRootFilesystem set
//    to false and there will be no mount for the `/tmp` folder
//
// This method is safe for both create and update operations.
func configureReadOnlyRootFilesystem(request requests.CreateFunctionRequest, deployment *v1beta1.Deployment) {
	if deployment.Spec.Template.Spec.Containers[0].SecurityContext != nil {
		deployment.Spec.Template.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem = &request.ReadOnlyRootFilesystem
	} else {
		deployment.Spec.Template.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{
			ReadOnlyRootFilesystem: &request.ReadOnlyRootFilesystem,
		}
	}

	existingVolumes := removeVolume("temp", deployment.Spec.Template.Spec.Volumes)
	deployment.Spec.Template.Spec.Volumes = existingVolumes

	existingMounts := removeVolumeMount("temp", deployment.Spec.Template.Spec.Containers[0].VolumeMounts)
	deployment.Spec.Template.Spec.Containers[0].VolumeMounts = existingMounts

	if request.ReadOnlyRootFilesystem {
		deployment.Spec.Template.Spec.Volumes = append(
			existingVolumes,
			corev1.Volume{
				Name: "temp",
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			},
		)

		deployment.Spec.Template.Spec.Containers[0].VolumeMounts = append(
			existingMounts,
			corev1.VolumeMount{
				Name:      "temp",
				MountPath: "/tmp",
				ReadOnly:  false},
		)
	}
}

// configureContainerUserID set the UID for all containers in the function Container.  Defaults to user
// specified in image metadata if `SetNonRootUser` is `false`. Root == 0.
func configureContainerUserID(deployment *v1beta1.Deployment, userID int64, config *DeployHandlerConfig) {
	var functionUser *int64

	if config.SetNonRootUser {
		functionUser = &userID
	}

	if deployment.Spec.Template.Spec.Containers[0].SecurityContext == nil {
		deployment.Spec.Template.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{}
	}

	deployment.Spec.Template.Spec.Containers[0].SecurityContext.RunAsUser = functionUser
}

func isFunction(deployment *v1beta1.Deployment) bool {
	if deployment != nil {
		if _, found := deployment.Labels["faas_function"]; found {
			return true
		}
	}
	return false
}
