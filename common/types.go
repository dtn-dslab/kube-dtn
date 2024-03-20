package common

import (
	"github.com/containernetworking/cni/pkg/types"
	pb "github.com/y-young/kube-dtn/api/v1"
)

type NetConf struct {
	types.NetConf
	Delegate map[string]interface{} `json:"delegate"`
}

type K8sArgs struct {
	types.CommonArgs
	K8S_POD_NAME               types.UnmarshallableString
	K8S_POD_NAMESPACE          types.UnmarshallableString
	K8S_POD_INFRA_CONTAINER_ID types.UnmarshallableString
}

type RedisTopologyStatus struct {
	SrcIP string `json:"src_ip"`
	NetNs string `json:"net_ns"`
}

type OVSFlowBetweenNodesSpec struct {
	RemoteIP  string `json:"remote_ip"`
	RemoteMac string `json:"remote_mac"`
}

type RedisTopologySpec struct {
	Links []pb.Link `json:"links"`
}
