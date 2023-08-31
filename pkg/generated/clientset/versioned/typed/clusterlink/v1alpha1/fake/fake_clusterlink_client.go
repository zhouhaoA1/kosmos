// Code generated by client-gen. DO NOT EDIT.

package fake

import (
	v1alpha1 "github.com/kosmos.io/kosmos/pkg/generated/clientset/versioned/typed/clusterlink/v1alpha1"
	rest "k8s.io/client-go/rest"
	testing "k8s.io/client-go/testing"
)

type FakeClusterlinkV1alpha1 struct {
	*testing.Fake
}

func (c *FakeClusterlinkV1alpha1) Clusters() v1alpha1.ClusterInterface {
	return &FakeClusters{c}
}

func (c *FakeClusterlinkV1alpha1) ClusterNodes() v1alpha1.ClusterNodeInterface {
	return &FakeClusterNodes{c}
}

func (c *FakeClusterlinkV1alpha1) NodeConfigs() v1alpha1.NodeConfigInterface {
	return &FakeNodeConfigs{c}
}

// RESTClient returns a RESTClient that is used to communicate
// with API server by this client implementation.
func (c *FakeClusterlinkV1alpha1) RESTClient() rest.Interface {
	var ret *rest.RESTClient
	return ret
}
