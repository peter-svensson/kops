/*
Copyright 2022 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package scalewaymodel

import (
	"k8s.io/kops/pkg/model"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/scalewaytasks"
)

type ScwModelContext struct {
	*model.KopsModelContext

	// LBBackendsCP holds the API LB backends used by control plane scaling groups.
	// These have health checks on the kube-apiserver and kops-controller ports.
	LBBackendsCP []*scalewaytasks.LBBackend

	// LBBackendsWorker holds the LB backends used by worker scaling groups.
	// These have a TCP health check on port 22 (SSH) which is always
	// available, preventing the autoscaling service from constantly
	// replacing instances it sees as "unhealthy".
	LBBackendsWorker []*scalewaytasks.LBBackend
}

// LinkToScalewayLoadBalancer returns a reference to the API load balancer task.
func (b *ScwModelContext) LinkToScalewayLoadBalancer() *scalewaytasks.LoadBalancer {
	return &scalewaytasks.LoadBalancer{
		Name: fi.PtrTo("api." + b.ClusterName()),
	}
}
