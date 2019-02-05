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

package infrastructure

import (
	"fmt"

	"github.com/projectcalico/dockerrun/pkg/containers"
	"github.com/projectcalico/dockerrun/pkg/utils"
)

func RunK8sApiserver(etcdIp string) (*containers.Container, error) {
	return containers.Run("apiserver",
		containers.RunOpts{AutoRemove: true},
		utils.Config.K8sImage,
		"/hyperkube", "apiserver",
		"--service-cluster-ip-range=10.101.0.0/16",
		"--authorization-mode=RBAC",
		"--insecure-port=8080", // allow insecure connection from controller manager.
		"--insecure-bind-address=0.0.0.0",
		fmt.Sprintf("--etcd-servers=http://%s:2379", etcdIp),
	)
}

func RunK8sControllerManager(apiserverIp string) (*containers.Container, error) {
	return containers.Run("controller-manager",
		containers.RunOpts{AutoRemove: true},
		utils.Config.K8sImage,
		"/hyperkube", "controller-manager",
		fmt.Sprintf("--master=%v:8080", apiserverIp),
		"--min-resync-period=3m",
		"--allocate-node-cidrs=true",
		"--cluster-cidr=192.168.0.0/16",
		"--v=5",
	)
}
