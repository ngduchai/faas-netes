package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"realtime"
	"time"

	"github.com/openfaas/faas/gateway/requests"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// MakeUpdateHandler update specified function
func MakeUpdateHandler(
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

		status, err := ac.Update(request, functionNamespace, clientset, config)
		if err != nil {
			log.Println("Unable to update function - " + request.Service + "Error: " + err.Error())
			w.WriteHeader(status)
			w.Write([]byte(err.Error()))
		}
		log.Println("Deployed function - " + request.Service)

		w.WriteHeader(http.StatusAccepted)
	}
}
