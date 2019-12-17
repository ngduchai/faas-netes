// Copyright (c) Alex Ellis 2017. All rights reserved.
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

// MakeDeployHandler creates a handler to create new functions in the cluster
func MakeDeployHandler(
	functionNamespace string,
	ac *realtime.AdmissionControl,
	clientset *kubernetes.Clientset,
	config *DeployHandlerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()

		body, _ := ioutil.ReadAll(r.Body)

		request := requests.CreateFunctionRequest{}
		err := json.Unmarshal(body, &request)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		code, err := ac.Register(request, functionNamespace, clientset, config)
		if err != nil {
			log.Println("Unable to deploy function - " + request.Service + "Error: " + err.Error())
			w.WriteHeader(code)
			w.Write([]byte(err.Error()))
			return
		}
		log.Println("Deployed function - " + request.Service)

		w.WriteHeader(http.StatusAccepted)

	}
}
