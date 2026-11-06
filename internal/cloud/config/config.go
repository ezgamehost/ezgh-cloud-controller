// Package config reads /etc/ezgh/cloud.json, the cluster facts k8sagent writes from each desired
// state (D8). Controllers read it on each use, so a change needs no restart.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
)

// Path is where k8sagent writes the file.
const Path = "/etc/ezgh/cloud.json"

// Config is the cluster's facts. Cluster is the kcl_ slug; ClusterUUID is the value of every
// ezgh:kubernetes-cluster tag. Subnet is the cluster's sbn_, where load balancers go
// unless a Service names another.
type Config struct {
	Cluster                 string        `json:"cluster"`
	ClusterUUID             string        `json:"clusterUuid"`
	Org                     string        `json:"org"`
	Project                 string        `json:"project"`
	Region                  string        `json:"region"`
	VPC                     string        `json:"vpc"`
	Subnet                  string        `json:"subnet"`
	APIEndpoint             string        `json:"apiEndpoint"`
	CABundle                string        `json:"caBundle"`
	NodeGroups              []NodeGroup   `json:"nodeGroups"`
	PodIdentityAssociations []Association `json:"podIdentityAssociations"`
}

// NodeGroup is one node group and the autoscaling group it owns.
type NodeGroup struct {
	ID               string            `json:"id"`
	AutoscalingGroup string            `json:"autoscalingGroup"`
	InstanceType     string            `json:"instanceType"`
	MinSize          int               `json:"minSize"`
	MaxSize          int               `json:"maxSize"`
	Labels           map[string]string `json:"labels"`
	Taints           []corev1.Taint    `json:"taints"`
}

// Association is a pod identity association's service account.
type Association struct {
	Namespace      string `json:"namespace"`
	ServiceAccount string `json:"serviceAccount"`
}

// Load reads and checks the file. Unknown fields are ignored: k8sagent's writer may grow first.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if c.Cluster == "" || c.ClusterUUID == "" || c.Org == "" || c.Project == "" {
		return Config{}, errors.New(path + ": cluster, clusterUuid, org and project are required")
	}
	if c.Region == "" {
		c.Region = "us-west-1"
	}
	return c, nil
}

// ClusterTags are the tags every resource the cluster's controllers create carries.
func (c Config) ClusterTags() map[string]string {
	return map[string]string{"ezgh:kubernetes-cluster": c.ClusterUUID}
}

// NodeGroup returns the node group with id kng.
func (c Config) NodeGroup(kng string) (NodeGroup, bool) {
	for _, g := range c.NodeGroups {
		if g.ID == kng {
			return g, true
		}
	}
	return NodeGroup{}, false
}
