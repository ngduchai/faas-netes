// Copyright (c) Alex Ellis 2017. All rights reserved.
// Licensed under the MIT license. See LICENSE file in the project root for full license information.

package handlers

import (
	"encoding/json"
	"io/ioutil"
	"net/http"
	"realtime"

	"github.com/openfaas/faas/gateway/requests"
	v1beta1 "k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// MakeDeleteHandler delete a function
func MakeDeleteHandler(
	functionNamespace string,
	ac *realtime.AdmisionControl,
	clientset *kubernetes.Clientset) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()

		body, _ := ioutil.ReadAll(r.Body)

		request := requests.DeleteFunctionRequest{}
		err := json.Unmarshal(body, &request)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if len(request.FunctionName) == 0 {
			w.WriteHeader(http.StatusBadRequest)
		}

		status, err := ac.Unregister(request, functionNamespace, clientset)
		if err != nil {
			log.Println("Unable to delete function - " + request.Service + "Error: " + err.Error())
			w.WriteHeader(status)
			w.Write(err.Error())
			return
		}
		log.Println("Deployed function - " + request.Service)

		w.WriteHeader(http.StatusAccepted)
	}
}
