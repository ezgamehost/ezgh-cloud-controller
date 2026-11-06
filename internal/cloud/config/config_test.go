package config

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cloud.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	c, err := Load(write(t, `{"cluster":"kcl_a","clusterUuid":"0199","org":"o","project":"p","vpc":"vpc_a","subnet":"sbn_a","futureField":1,
		"nodeGroups":[{"id":"kng_a","autoscalingGroup":"asg_a","instanceType":"gp-1.large","minSize":1,"maxSize":3,"labels":{"a":"b"},"taints":[{"key":"k","value":"v","effect":"NoSchedule"}]}],
		"podIdentityAssociations":[{"namespace":"ns","serviceAccount":"sa"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	g, ok := c.NodeGroup("kng_a")
	if !ok || g.Taints[0].Effect != "NoSchedule" || c.Region != "us-west-1" || c.PodIdentityAssociations[0].ServiceAccount != "sa" {
		t.Fatalf("%+v", c)
	}
	if c.ClusterTags()["ezgh:kubernetes-cluster"] != "0199" {
		t.Fatal("the cluster tag's value is the cluster UUID")
	}
}

func TestLoadRequiresClusterOrgProject(t *testing.T) {
	for _, body := range []string{`{}`, `{"cluster":"kcl_a","clusterUuid":"u","org":"o"}`, `{"cluster":"kcl_a","org":"o","project":"p"}`, `not json`} {
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("%s: want an error", body)
		}
	}
}
