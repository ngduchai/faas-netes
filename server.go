// Copyright (c) Alex Ellis 2017. All rights reserved.
// Licensed under the MIT license. See LICENSE file in the project root for full license information.

package main

import (
	"log"
	"os"

	"github.com/openfaas/faas-netes/handlers"
	"github.com/openfaas/faas-netes/types"
	"github.com/openfaas/faas-netes/version"
	bootstrap "github.com/openfaas/faas-provider"
	bootTypes "github.com/openfaas/faas-provider/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	log.Printf("Start faas-netes")
	// creates the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}

	functionNamespace := "default"

	if namespace, exists := os.LookupEnv("function_namespace"); exists {
		functionNamespace = namespace
	}

	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	readConfig := types.ReadConfig{}
	osEnv := types.OsEnv{}
	cfg := readConfig.Read(osEnv)

	//cfg.ReadTimeout = 10 * time.Minute
	//cfg.WriteTimeout = 10 * time.Minute

	log.Printf("HTTP Read Timeout: %s\n", cfg.ReadTimeout)
	log.Printf("HTTP Write Timeout: %s\n", cfg.WriteTimeout)
	log.Printf("HTTPProbe: %v\n", cfg.HTTPProbe)
	log.Printf("SetNonRootUser: %v\n", cfg.SetNonRootUser)

	deployConfig := &handlers.DeployHandlerConfig{
		HTTPProbe:      cfg.HTTPProbe,
		SetNonRootUser: cfg.SetNonRootUser,
		FunctionReadinessProbeConfig: &handlers.FunctionProbeConfig{
			InitialDelaySeconds: int32(cfg.ReadinessProbeInitialDelaySeconds),
			TimeoutSeconds:      int32(cfg.ReadinessProbeTimeoutSeconds),
			PeriodSeconds:       int32(cfg.ReadinessProbePeriodSeconds),
		},
		FunctionLivenessProbeConfig: &handlers.FunctionProbeConfig{
			InitialDelaySeconds: int32(cfg.LivenessProbeInitialDelaySeconds),
			TimeoutSeconds:      int32(cfg.LivenessProbeTimeoutSeconds),
			PeriodSeconds:       int32(cfg.LivenessProbePeriodSeconds),
		},
		ImagePullPolicy: cfg.ImagePullPolicy,
	}

	bootstrapHandlers := bootTypes.FaaSHandlers{
		FunctionProxy:  handlers.MakeProxy(functionNamespace, cfg.ReadTimeout),
		DeleteHandler:  handlers.MakeDeleteHandler(functionNamespace, clientset),
		DeployHandler:  handlers.MakeDeployHandler(functionNamespace, clientset, deployConfig),
		FunctionReader: handlers.MakeFunctionReader(functionNamespace, clientset),
		ReplicaReader:  handlers.MakeReplicaReader(functionNamespace, clientset),
		ReplicaUpdater: handlers.MakeReplicaUpdater(functionNamespace, clientset),
		UpdateHandler:  handlers.MakeUpdateHandler(functionNamespace, clientset, deployConfig),
		HealthHandler:  handlers.MakeHealthHandler(),
		InfoHandler:    handlers.MakeInfoHandler(version.BuildVersion(), version.GitCommit),
		SecretHandler:  handlers.MakeSecretHandler(functionNamespace, clientset),
	}

	var port int
	port = cfg.Port
	log.Printf("Port %d", port)

	bootstrapConfig := bootTypes.FaaSConfig{
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		TCPPort:      &port,
		EnableHealth: true,
	}

	bootstrap.Serve(&bootstrapHandlers, &bootstrapConfig)
}
