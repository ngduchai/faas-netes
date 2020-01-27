// Copyright 2019 OpenFaaS Author(s)
// Licensed under the MIT license. See LICENSE file in the project root for full license information.

package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"time"

	"github.com/openfaas/faas-netes/k8s"

	types "github.com/openfaas/faas-provider/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MakeUpdateHandler update specified function
func MakeUpdateHandler(defaultNamespace string, factory k8s.FunctionFactory) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			defer r.Body.Close()
		}

		body, _ := ioutil.ReadAll(r.Body)

		request := types.FunctionDeployment{}
		err := json.Unmarshal(body, &request)
		if err != nil {
			wrappedErr := fmt.Errorf("unable to unmarshal request: %s", err.Error())
			http.Error(w, wrappedErr.Error(), http.StatusBadRequest)
			return
		}

		lookupNamespace := defaultNamespace
		if len(request.Namespace) > 0 {
			lookupNamespace = request.Namespace
		}

		if lookupNamespace == "kube-system" {
			http.Error(w, "unable to list within the kube-system namespace", http.StatusUnauthorized)
			return
		}

		annotations := buildAnnotations(request)
		if err, status := updateDeploymentSpec(lookupNamespace, factory, request, annotations); err != nil {
			if !k8s.IsNotFound(err) {
				log.Printf("error updating deployment: %s.%s, error: %s\n", request.Service, lookupNamespace, err)

				return
			}

			wrappedErr := fmt.Errorf("unable update Deployment: %s.%s, error: %s", request.Service, lookupNamespace, err.Error())
			http.Error(w, wrappedErr.Error(), status)
			return
		}

		if err, status := updateService(lookupNamespace, factory, request, annotations); err != nil {
			if !k8s.IsNotFound(err) {
				log.Printf("error updating service: %s.%s, error: %s\n", request.Service, lookupNamespace, err)
			}

			wrappedErr := fmt.Errorf("unable update Service: %s.%s, error: %s", request.Service, request.Namespace, err.Error())
			http.Error(w, wrappedErr.Error(), status)
			return
		}

		w.WriteHeader(http.StatusAccepted)
	}
}

func updateDeploymentSpec(
	functionNamespace string,
	factory k8s.FunctionFactory,
	request types.FunctionDeployment,
	annotations map[string]string) (err error, httpStatus int) {

	getOpts := metav1.GetOptions{}

	deployment, findDeployErr := factory.Client.AppsV1().
		Deployments(functionNamespace).
		Get(request.Service, getOpts)

	if findDeployErr != nil {
		return findDeployErr, http.StatusNotFound
	}

	prevReplicas := deployment.Spec.Replicas
	numReplicas := *prevReplicas

	prev_realtime := deployment.Labels["realtime"]
	prev_size := deployment.Labels["functionsize"]
	prev_duration := deployment.Labels["duration"]

	if len(deployment.Spec.Template.Spec.Containers) > 0 {
		deployment.Spec.Template.Spec.Containers[0].Image = request.Image

		// Disabling update support to prevent unexpected mutations of deployed functions,
		// since imagePullPolicy is now configurable. This could be reconsidered later depending
		// on desired behavior, but will need to be updated to take config.
		//deployment.Spec.Template.Spec.Containers[0].ImagePullPolicy = v1.PullAlways

		deployment.Spec.Template.Spec.Containers[0].Env = buildEnvVars(&request)

		factory.ConfigureReadOnlyRootFilesystem(request, deployment)
		factory.ConfigureContainerUserID(deployment)

		deployment.Spec.Template.Spec.NodeSelector = createSelector(request.Constraints)

		labels := map[string]string{
			"faas_function": request.Service,
			"uid":           fmt.Sprintf("%d", time.Now().Nanosecond()),
		}

		if request.Labels != nil {
			if min := getMinReplicaCount(*request.Labels); min != nil {
				deployment.Spec.Replicas = min
			}

			for k, v := range *request.Labels {
				labels[k] = v
			}
			realtime := extractLabelRealValue((*request.Labels)["realtime"], float64(0))
			size := extractLabelRealValue((*request.Labels)["functionsize"], float64(1.0))
			duration := extractLabelValue((*request.Labels)["duration"], uint64(60))
			numReplicas = int32(math.Ceil(realtime * size * float64(duration)))
			if numReplicas > 0 {
				deployment.Spec.Replicas = &numReplicas
			}

		}

		// deployment.Labels = labels
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

		secrets := k8s.NewSecretsClient(factory.Client)
		existingSecrets, err := secrets.GetSecrets(functionNamespace, request.Secrets)
		if err != nil {
			return err, http.StatusBadRequest
		}

		err = factory.ConfigureSecrets(request, deployment, existingSecrets)
		if err != nil {
			log.Println(err)
			return err, http.StatusBadRequest
		}

		probes, err := factory.MakeProbes(request)
		if err != nil {
			return err, http.StatusBadRequest
		}

		deployment.Spec.Template.Spec.Containers[0].LivenessProbe = probes.Liveness
		deployment.Spec.Template.Spec.Containers[0].ReadinessProbe = probes.Readiness
	}

	if _, updateErr := factory.Client.AppsV1().
		Deployments(functionNamespace).
		Update(deployment); updateErr != nil {

		return updateErr, http.StatusInternalServerError
	}

	// Ensure real-time requirement
	deploy := clientset.ExtensionsV1beta1().Deployments(functionNamespace)
	getopt := metav1.GetOptions{}
	ready := false
	dep, err := deploy.Get(request.Service, getopt)
	if err == nil {
		retries := 2 * numReplicas
		nochange := 0
		prevAvailReplicas := int32(0)
		//for err == nil && retries > 0 && nochange < 40 {
		for err == nil && retries > 0 {
			availReplicas := (*dep).Status.AvailableReplicas
			log.Printf("Update [%d]: numReplicas: %d, availReplicas: %d",
				retries, numReplicas, availReplicas)
			if availReplicas == numReplicas {
				ready = true
				break
			}
			if availReplicas != *prevReplicas && availReplicas == prevAvailReplicas {
				nochange++
			} else {
				nochange = 0
			}
			prevAvailReplicas = availReplicas
			dep, err = deploy.Get(request.Service, getopt)
			time.Sleep(time.Duration(1000) * time.Millisecond)
			retries--
		}
		log.Printf("Update %s -- retries: %d Requires: %d Avail: %d",
			request.Service, retries, numReplicas,
			(*dep).Status.AvailableReplicas)
	}
	if !ready {
		deployment, findDeployErr := clientset.ExtensionsV1beta1().
			Deployments(functionNamespace).
			Get(request.Service, getOpts)
		if findDeployErr != nil {
			log.Println("Unable to get the function again.")
			return findDeployErr, http.StatusNotFound
		}
		deployment.Spec.Replicas = prevReplicas
		deployment.Labels["realtime"] = prev_realtime
		deployment.Labels["functionsize"] = prev_size
		deployment.Labels["duration"] = prev_duration
		_, err := clientset.ExtensionsV1beta1().
			Deployments(functionNamespace).Update(deployment)
		if err != nil {
			log.Println("Cannot rollback function update: " + err.Error())
		}
		err = errors.New("Fail to reserve resource for new real-time requirement")
		log.Println("Update " + request.Service + " " + err.Error())
		return err, http.StatusInternalServerError
	}

	return nil, http.StatusAccepted
}

func updateService(
	functionNamespace string,
	factory k8s.FunctionFactory,
	request types.FunctionDeployment,
	annotations map[string]string) (err error, httpStatus int) {

	getOpts := metav1.GetOptions{}

	service, findServiceErr := factory.Client.CoreV1().
		Services(functionNamespace).
		Get(request.Service, getOpts)

	if findServiceErr != nil {
		return findServiceErr, http.StatusNotFound
	}

	service.Annotations = annotations

	if _, updateErr := factory.Client.CoreV1().
		Services(functionNamespace).
		Update(service); updateErr != nil {

		return updateErr, http.StatusInternalServerError
	}

	return nil, http.StatusAccepted
}

/*
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
*/
