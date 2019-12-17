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

type ReserveAdmissionControl struct {
}

func (ac ReserveAdmissionControl) Register(
	request *CreateFunctionRequest,
	functionNamespace string,
	clientset *kubernetes.Clientset,
	config *DeployHandlerConfig) (int, error) {

	rm := ResourceManager{}

	// Create sufficient reservation
	realtime, size, duration = rm.GetRealtimeParams(request)
	numReplicas = int32(math.Ceil(realtime * size * float64(duration)))
	if numReplicas < 1 {
		numReplicas = 1
	}
	// Create function image aka Deployment+Service
	code, error := rm.createImage(request, functionNamespace, clientset, config, numReplicas)
	if error != nil {
		return code, error
	}

	// Wait until we have sufficient replicas
	// Make sure reserved replicas are running
	error = checkAvailReplicas(request.Service, clientset, rm, numReplicas)
	if error != nil {
		req := requests.DeleteFunctionRequest{}
		req.FunctionName = request.Service
		_, err := rm.removeImage(req, functionNamespace, clientset)
		if err != nil {
			log.Printf("Unable to rollback function %s deployment", req.FunctionName)
		}
		return http.StatusInternalServerError, error
	}

	return http.StatusAccepted, nil

}

func (ac ReserveAdmissionControl) Update(
	request *CreateFunctionRequest,
	functionNamespace string,
	clientset *kubernetes.Clientset,
	config *DeployHandlerConfig) (int, error) {

	rm = ResourceManager{}

	// Calculate reservation
	realtime, size, duration = rm.GetRealtimeParams(request)
	numReplicas = int32(math.Ceil(realtime * size * float64(duration)))
	if numReplicas < 1 {
		numReplicas = 1
	}

	// Get the current reservation
	prevRealtime, prevSize, prevDuration, prevReplicas, error := rm.DeploymentRealtimeParams(request.Service, clientset)

	// update function image aka Deployment+Service
	code, error := rm.updateImage(request, functionNamespace, clientset, config, numReplicas)
	if error != nil {
		return code, error
	}

	// Wait until we have sufficient replicas
	// Make sure reserved replicas are running
	error = checkAvailReplicas(request.Service, clientset, rm, numReplicas)
	if error != nil {
		request.Labels["realtime"] = strconv.Itoa(preRealtime)
		request.Labels["size"] = strconv.Itoa(preSize)
		request.Labels["duration"] = strconv.Itoa(preDuration)
		_, err := rm.updateImage(request, functionNamespace, clientset, config, prevReplicas)
		if err != nil {
			log.Printf("Unable to rollback function %s update", request.Service)
		}
		return http.StatusInternalError, error
	}

	return http.StatusAccepted, nil

}

func (ac ReserveAdmissionControl) Unregister(
	request *DeleteFunctionRequest,
	functionnamespace string,
	clientset *kubernetes.Clientset) (int, error) {

	// Simply remove the image
	rm = ResourceManager{}
	return rm.removeImage(request, functionNamespace, clientset)
}

func checkAvailReplicas(
	functionName string,
	clientset *kubernetes.Clientset,
	rm *ResourceManager,
	numReplicas int) error {

	retries := 2 * numReplicas
	nochange := 0
	ready = false
	_, _, _, prevReplicas, error := rm.AvailableReplicas(request.Service, clientset)
	for error == nil && retries > 0 {
		_, _, _, availReplicas, error := rm.AvailableReplicas(request.Service, clientset)
		log.Printf("Deployment [%d]: numReplicas: %d, availReplicas: %d",
			retries, numReplicas, availReplicas)
		if availReplicas == numReplicas {
			ready = true
			break
		}
		if availReplicas > 0 && availReplicas == prevReplicas {
			nochange++
		} else {
			nochange = 0
		}
		prevReplicas = availReplicas
		time.Sleep(time.Duration(1000) * time.Millisecond)
		retries--
	}
	log.Printf("Deploy %s -- retries: %d Requires: %d Avail: %d",
		request.Service, retries, numReplicas, AvailableReplicas)
	if error != nil {
		return error
	}
	if !ready {
		return errors.New("Fail to reserve resource")
	}
	return nil
}

func rollBack(
	functionName string,
	clientset *kubernetes.Clientset,
	rm *ResourceManager) error {

	deploy := clientset.Extensions().Deployments(functionNamespace)
	foregroundPolicy := metav1.DeletePropagationForeground
	delopt := metav1.DeleteOptions{
		//GracePeriodSeconds: &period,
		PropagationPolicy: &foregroundPolicy,
	}
	return deploy.Delete(request.Service, &delopt)
}
