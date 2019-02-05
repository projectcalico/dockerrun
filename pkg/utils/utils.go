// Copyright (c) 2017-2018 Tigera, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package utils

import (
	"context"
	"os/exec"
	"strings"

	"github.com/kelseyhightower/envconfig"
	log "github.com/sirupsen/logrus"
)

type EnvConfig struct {
	FelixImage   string `default:"calico/felix:latest"`
	EtcdImage    string `default:"quay.io/coreos/etcd"`
	K8sImage     string `default:"gcr.io/google_containers/hyperkube-amd64:v1.10.4"`
	TyphaImage   string `default:"calico/typha:latest"` // Note: this is overridden in the Makefile!
	BusyboxImage string `default:"busybox:latest"`
}

var Config EnvConfig

func init() {
	err := envconfig.Process("fv", &Config)
	if err != nil {
		panic(err)
	}
	log.WithField("config", Config).Info("Loaded config")
}

var Ctx = context.Background()

func Run(command string, args ...string) error {
	outputBytes, err := Command(command, args...).CombinedOutput()
	if err != nil {
		log.WithFields(log.Fields{
			"command": command,
			"args":    args,
			"output":  string(outputBytes)}).WithError(err).Warning("Command failed")
	}

	return err
}

func RunCommand(command string, args ...string) error {
	cmd := Command(command, args...)
	log.Infof("Running '%s %s'", cmd.Path, strings.Join(cmd.Args, " "))
	output, err := cmd.CombinedOutput()
	log.Infof("output: %v", string(output))
	return err
}

func Command(name string, args ...string) *exec.Cmd {
	log.WithFields(log.Fields{
		"command":     name,
		"commandArgs": args,
	}).Info("Creating Command.")

	return exec.Command(name, args...)
}
