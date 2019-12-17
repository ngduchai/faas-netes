package realtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"time"

	"github.com/openfaas/faas/gateway/requests"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type AdmissionControl interface {
	Register(
		request *CreateFunctionRequest,
		functionNamespace string,
		clientset *kubernetes.Clientset,
		config *DeployHandlerConfig) (int, error)
	Update(
		request *CreateFunctionRequest,
		functionNamespace string,
		clientset *kubernetes.Clientset,
		config *DeployHandlerConfig) (int, error)
	Unregister(
		request *DeleteFunctionRequest,
		functionNamespace string,
		clientset *kubernetes.Clientset) (int, error)
}
