package kubedtn

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/digitalocean/go-openvswitch/ovs"
	"net"
	"time"

	"github.com/go-redis/redis/v8"
	v1 "github.com/y-young/kube-dtn/api/v1"
	fastlink "github.com/y-young/kube-dtn/daemon/fastlink"
	"github.com/y-young/kube-dtn/daemon/grpcwire"
	"github.com/y-young/kube-dtn/daemon/vxlan"

	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/google/gopacket/pcap"
	"github.com/y-young/kube-dtn/common"
	pb "github.com/y-young/kube-dtn/proto/v1"
)

// var interNodeLinkType = common.INTER_NODE_LINK_VXLAN

func (m *KubeDTN) getTopoStatusFromRedis(name string) (common.RedisTopologyStatus, error) {
	redisTopoStatus := &common.RedisTopologyStatus{}

	statusJSON, err := m.redis.Get(m.ctx, "cni_"+name+"_status").Result()
	if err != redis.Nil {
		if err = json.Unmarshal([]byte(statusJSON), redisTopoStatus); err == nil {
			return *redisTopoStatus, nil
		}
	}

	return *redisTopoStatus, fmt.Errorf("failed to get pod %s status from redis", name)
}

func (m *KubeDTN) getTopoSpecFromRedis(name string) (common.RedisTopologySpec, error) {
	redisTopoSpec := &common.RedisTopologySpec{}

	specJSON, err := m.redis.Get(m.ctx, "cni_"+name+"_spec").Result()
	if err != redis.Nil {
		if err = json.Unmarshal([]byte(specJSON), redisTopoSpec); err == nil {
			return *redisTopoSpec, nil
		}
	}

	return *redisTopoSpec, fmt.Errorf("failed to get pod %s spec from redis", name)
}

func (m *KubeDTN) getPod(ctx context.Context, name, ns string) (*v1.Topology, error) {
	logger := common.GetLogger(ctx)
	if ns == "" {
		ns = "default"
	}
	pod := fmt.Sprintf("%s/%s", ns, name)
	logger.Infof("Reading pod %s from informer", pod)

	obj, exists, err := m.topologyStore.GetByKey(pod)
	if err != nil || !exists {
		logger.Infof("Failed to read pod %s from informer, trying from K8s", name)
		return m.tClient.Topology(ns).Get(ctx, name, metav1.GetOptions{})
	}
	return obj.(*v1.Topology), nil
}

func (m *KubeDTN) updateStatus(ctx context.Context, topology *v1.Topology, ns string) error {
	logger := common.GetLogger(ctx)
	logger.Infof("Update pod status %s from K8s", topology.Name)
	_, err := m.tClient.Topology(ns).UpdateStatus(ctx, topology, metav1.UpdateOptions{})
	return err
}

func (m *KubeDTN) Get(ctx context.Context, pod *pb.PodQuery) (*pb.Pod, error) {
	logger := common.GetLogger(ctx)

	topology, err := m.getPod(ctx, pod.Name, pod.KubeNs)
	if err != nil {
		logger.Errorf("Failed to read pod %s", pod.Name)
		return nil, err
	}

	return m.ToProtoPod(ctx, topology)
}

func (m *KubeDTN) ToProtoPod(ctx context.Context, topology *v1.Topology) (*pb.Pod, error) {
	logger := common.GetLogger(ctx)

	remoteLinks := topology.Spec.Links
	if remoteLinks == nil {
		logger.Errorf("Could not find 'Link' array in pod's spec")
		return nil, fmt.Errorf("could not find 'Link' array in pod's spec")
	}

	links := make([]*pb.Link, len(remoteLinks))
	for i := range links {
		remoteLink := remoteLinks[i]
		newLink := remoteLink.ToProto()
		links[i] = newLink
	}

	srcIP := topology.Status.SrcIP
	netNs := topology.Status.NetNs

	return &pb.Pod{
		Name:   topology.Name,
		SrcIp:  srcIP,
		NetNs:  netNs,
		KubeNs: topology.Namespace,
		Links:  links,
	}, nil
}

func (m *KubeDTN) SetAlive(ctx context.Context, pod *pb.Pod) (*pb.BoolResponse, error) {
	logger := common.GetLogger(ctx).WithFields(log.Fields{
		"pod":    pod.Name,
		"ns":     pod.KubeNs,
		"action": "setAlive",
	})

	logger.Infof("Setting SrcIp=%s and NetNs=%s", pod.SrcIp, pod.NetNs)
	alive := pod.SrcIp != "" && pod.NetNs != ""

	redisTopoStatus := &common.RedisTopologyStatus{
		NetNs: "",
		SrcIP: "",
	}

	if alive {
		redisTopoStatus.NetNs = pod.NetNs
		redisTopoStatus.SrcIP = pod.SrcIp
		statusJSON, err := json.Marshal(redisTopoStatus)
		if err != nil {
			log.Error(err, "Failed to marshal topology status")
		}
		err = m.redis.Set(m.ctx, "cni_"+pod.Name+"_status", statusJSON, 0).Err()
		if err != nil {
			logger.Errorf("Failed to update pod alive status: %v", err)
		}
		logger.Infof("Successfully updated pod alive status")
	} else {
		redisTopoStatus, err := m.getTopoStatusFromRedis(pod.Name)
		if err != nil {
			logger.Errorf("Failed to retrieve peer pod %s/%s topology", pod.KubeNs, pod.Name)
			return &pb.BoolResponse{Response: false}, err
		}
		pod.NetNs = redisTopoStatus.NetNs
		pod.SrcIp = redisTopoStatus.SrcIP

		err = m.redis.Del(m.ctx, "cni_"+pod.Name+"_status").Err()
		if err != nil {
			logger.Errorf("Failed to delete pod alive status: %v", err)
		}
		logger.Infof("Successfully deleted pod alive status")
	}

	return &pb.BoolResponse{Response: true}, nil
}

func (m *KubeDTN) Update(ctx context.Context, pod *pb.RemotePod) (*pb.BoolResponse, error) {
	uid := common.GetUidFromVni(pod.Vni)
	logger := common.GetLogger(ctx).WithFields(log.Fields{
		"pod":    pod.Name,
		"ns":     pod.KubeNs,
		"link":   uid,
		"action": "remoteUpdate",
	})
	ctx = common.WithLogger(ctx, logger)
	logger.Infof("Updating pod from remote")
	startTime := time.Now()

	var err error
	vxlanSpec := &vxlan.VxlanSpec{
		NetNs:    pod.NetNs,
		IntfName: pod.IntfName,
		IntfIp:   pod.IntfIp,
		PeerVtep: pod.PeerVtep,
		Vni:      pod.Vni,
		SrcIp:    m.nodeIP,
		SrcIntf:  m.vxlanIntf,
	}

	mutex_start := time.Now()
	mutex := m.linkMutexes.Get(uid)
	mutex.Lock()
	defer mutex.Unlock()
	mutex_elapsed := time.Since(mutex_start)
	m.latencyHistograms.Observe("remoteUpdate_mutex", mutex_elapsed.Milliseconds())

	// Check if there's a vxlan link with the same VNI but in different namespace
	// netns := m.vxlanManager.Get(pod.Vni)
	// Ensure link is in different namespace since we might have set it up locally
	// if netns != nil && *netns != pod.NetNs {
	// 	logger.Infof("VXLAN with the same VNI already exists, removing it")
	// 	err = vxlan.RemoveLinkWithVni(ctx, pod.Vni, *netns)
	// 	if err != nil {
	// 		logger.Errorf("Failed to remove existing VXLAN link: %v", err)
	// 	}
	// }

	err = vxlan.SetupVxLan(ctx, vxlanSpec, pod.Properties, m.latencyHistograms, true)
	if err != nil {
		logger.Errorf("Failed to handle remote update: %v", err)
		return &pb.BoolResponse{Response: false}, err
	}

	elapsed := time.Since(startTime)
	m.latencyHistograms.Observe("remoteUpdate", elapsed.Milliseconds())
	logger.Infof("Successfully handled remote update in %v", elapsed)
	return &pb.BoolResponse{Response: true}, nil
}

// ------------------------------------------------------------------------------------------------------
func (m *KubeDTN) RemGRPCWire(ctx context.Context, wireDef *pb.WireDef) (*pb.BoolResponse, error) {
	if err := grpcwire.DeleteWiresByPod(wireDef.KubeNs, wireDef.LocalPodName); err != nil {
		return &pb.BoolResponse{Response: false}, err
	}
	return &pb.BoolResponse{Response: true}, nil
}

// ------------------------------------------------------------------------------------------------------
func (m *KubeDTN) AddGRPCWireLocal(ctx context.Context, wireDef *pb.WireDef) (*pb.BoolResponse, error) {
	logger := logger.WithFields(log.Fields{
		"overlay": "gRPC",
	})
	locInf, err := net.InterfaceByName(wireDef.VethNameLocalHost)
	if err != nil {
		logger.Errorf("[ADD-WIRE:LOCAL-END]For pod %s failed to retrieve interface ID for interface %v. error:%v", wireDef.LocalPodName, wireDef.VethNameLocalHost, err)
		return &pb.BoolResponse{Response: false}, err
	}

	//Using google gopacket for packet receive. An alternative could be using socket. Not sure it it provides any advantage over gopacket.
	wrHandle, err := pcap.OpenLive(wireDef.VethNameLocalHost, 65365, true, pcap.BlockForever)
	if err != nil {
		logger.Fatalf("[ADD-WIRE:LOCAL-END]Could not open interface for send/recv packets for containers. error:%v", err)
		return &pb.BoolResponse{Response: false}, err
	}

	aWire := grpcwire.GRPCWire{
		UID: int(wireDef.LinkUid),

		LocalNodeIfaceID:   int64(locInf.Index),
		LocalNodeIfaceName: wireDef.VethNameLocalHost,
		LocalPodIP:         wireDef.LocalPodIp,
		LocalPodIfaceName:  wireDef.IntfNameInPod,
		LocalPodName:       wireDef.LocalPodName,
		LocalPodNetNS:      wireDef.LocalPodNetNs,

		PeerIfaceID: wireDef.PeerIntfId,
		PeerNodeIP:  wireDef.PeerIp,

		Originator:   grpcwire.HOST_CREATED_WIRE,
		OriginatorIP: "unknown", /*+++todo retrieve host ip and set it here. Needed only for debugging */

		StopC:     make(chan struct{}),
		Namespace: wireDef.KubeNs,
	}

	grpcwire.AddWire(&aWire, wrHandle)

	logger.Infof("[ADD-WIRE:LOCAL-END]For pod %s@%s starting the local packet receive thread", wireDef.LocalPodName, wireDef.IntfNameInPod)
	// TODO: handle error here
	go grpcwire.RecvFrmLocalPodThread(&aWire)

	return &pb.BoolResponse{Response: true}, nil
}

// ------------------------------------------------------------------------------------------------------
func (m *KubeDTN) SendToOnce(ctx context.Context, pkt *pb.Packet) (*pb.BoolResponse, error) {
	logger := logger.WithFields(log.Fields{
		"overlay": "gRPC",
	})
	wrHandle, err := grpcwire.GetHostIntfHndl(pkt.RemotIntfId)
	if err != nil {
		logger.Errorf("SendToOnce (wire id - %v): Could not find local handle. err:%v", pkt.RemotIntfId, err)
		return &pb.BoolResponse{Response: false}, err
	}

	// In case any per packet log need to be generated.
	// pktType := grpcwire.DecodePkt(pkt.Frame)
	// logger.Printf("Daemon(SendToOnce): Received [pkt: %s, bytes: %d, for local interface id: %d]. Sending it to local container", pktType, len(pkt.Frame), pkt.RemotIntfId)
	// logger.Printf("Daemon(SendToOnce): Received [bytes: %d, for local interface id: %d]. Sending it to local container", len(pkt.Frame), pkt.RemotIntfId)

	err = wrHandle.WritePacketData(pkt.Frame)
	if err != nil {
		logger.Errorf("SendToOnce (wire id - %v): Could not write packet(%d bytes) to local interface. err:%v", pkt.RemotIntfId, len(pkt.Frame), err)
		return &pb.BoolResponse{Response: false}, err
	}

	return &pb.BoolResponse{Response: true}, nil
}

// ---------------------------------------------------------------------------------------------------------------
func (m *KubeDTN) AddGRPCWireRemote(ctx context.Context, wireDef *pb.WireDef) (*pb.WireCreateResponse, error) {
	stopC := make(chan struct{})
	wire, err := grpcwire.CreateGRPCWireRemoteTriggered(wireDef, stopC)

	if err == nil {
		logger.Infof("[ADD-WIRE:REMOTE-END]For pod %s@%s starting the local packet receive thread", wireDef.LocalPodName, wireDef.IntfNameInPod)
		go grpcwire.RecvFrmLocalPodThread(wire)

		return &pb.WireCreateResponse{Response: true, PeerIntfId: wire.LocalNodeIfaceID}, nil
	}
	logger.Errorf("[ADD-WIRE:REMOTE-END] err: %v", err)
	return &pb.WireCreateResponse{Response: false, PeerIntfId: wireDef.PeerIntfId}, err
}

// ---------------------------------------------------------------------------------------------------------------
// GRPCWireExists will return the wire if it exists.
func (m *KubeDTN) GRPCWireExists(ctx context.Context, wireDef *pb.WireDef) (*pb.WireCreateResponse, error) {
	wire, ok := grpcwire.GetWireByUID(wireDef.LocalPodNetNs, int(wireDef.LinkUid))
	if !ok || wire == nil {
		return &pb.WireCreateResponse{Response: false, PeerIntfId: wireDef.PeerIntfId}, nil
	}
	return &pb.WireCreateResponse{Response: ok, PeerIntfId: wire.PeerIfaceID}, nil
}

// ---------------------------------------------------------------------------------------------------------------
// Given the pod name and the pod interface, GenerateNodeInterfaceName generates the corresponding interface name in the node.
// This pod interface and the node interface later become the two end of a veth-pair
func (m *KubeDTN) GenerateNodeInterfaceName(ctx context.Context, in *pb.GenerateNodeInterfaceNameRequest) (*pb.GenerateNodeInterfaceNameResponse, error) {
	locIfNm, err := grpcwire.GenNodeIfaceName(in.PodName, in.PodIntfName)
	if err != nil {
		return &pb.GenerateNodeInterfaceNameResponse{Ok: false, NodeIntfName: ""}, err
	}
	return &pb.GenerateNodeInterfaceNameResponse{Ok: true, NodeIntfName: locIfNm}, nil
}

func (m *KubeDTN) GetPortID(bridge, port string) int {
	portID, err := common.GetPortID(bridge, port)
	if err != nil {
		log.Fatalf("%v", err)
	}
	return portID
}

func (m *KubeDTN) PrintOVS() {
	res, err := common.PrintOVSInfo()
	if err != nil {
		log.Fatalf("failed to print ovs info: %v", err)
	}
	log.Infof(res)
}

func (m *KubeDTN) addLink(ctx context.Context, localPod *pb.Pod, link *pb.Link) error {
	logger := common.GetLogger(ctx).WithFields(log.Fields{
		"link": link.Uid,
	})
	ctx = common.WithLogger(ctx, logger)
	ctx = common.WithCtxValue(ctx, common.TCPIP_BYPASS, m.config.TCPIPBypass)

	logger.Infof("Adding link: %v", link)
	startTime := time.Now()

	// Build koko's veth struct for local intf
	logger.Infof("localPod.NetNs: %s", localPod.NetNs)
	myVeth, err := common.MakeVeth(ctx, localPod.NetNs, link.LocalIntf, link.LocalIp, link.LocalMac)
	if err != nil {
		return err
	}
	// Create veth pair and connect pod side to host bridge
	err = common.ConnectVethToBridge(myVeth, m.ovsClient)

	// Virtual-virtual link
	peerPod := &pb.Pod{
		Name: link.PeerPod,
	}

	redisTopoStatus, err := m.getTopoStatusFromRedis(link.PeerPod)

	// We believe that the peer daemon will handle when the peer pod comes up
	if err != nil {
		logger.Infof("Failed to retrieve peer pod %s/%s status", localPod.KubeNs, link.PeerPod)
		return nil
	}

	peerPod.NetNs = redisTopoStatus.NetNs
	peerPod.SrcIp = redisTopoStatus.SrcIP

	// This means we're coming up AFTER our peer so things are pretty easy

	// This flow table item may be added multiple times
	// sudo ovs-ofctl add-flow br-dpu-12 "dl_dst=00:00:00:00:01:01, actions=output:patch-tohost"
	flow := &ovs.Flow{
		Matches: []ovs.Match{
			ovs.DataLinkDestination(link.LocalMac),
		},
		Actions: []ovs.Action{ovs.Output(m.GetPortID(common.DPUBridge, common.ToHostPort))},
	}
	if err := m.ovsClient.OpenFlow.AddFlow(common.DPUBridge, flow); err != nil {
		log.Fatalf("failed to add flow on OVS bridge %s: %v", common.DPUBridge, err)
	}

	if peerPod.SrcIp == localPod.SrcIp { // This means we're on the same host
		// add flow table actions=output:patch-tohost
		logger.Infof("%s and %s are on the same host", localPod.Name, peerPod.Name)
		logger.Infof("peerPod.NetNs: %s", peerPod.NetNs)

		// This flow table item may be added multiple times
		// sudo ovs-ofctl add-flow br-dpu-12 "dl_dst=00:00:00:00:02:01, actions=output:patch-tohost"
		flow := &ovs.Flow{
			Matches: []ovs.Match{
				ovs.DataLinkDestination(link.PeerMac),
			},
			Actions: []ovs.Action{ovs.Output(m.GetPortID(common.DPUBridge, common.ToHostPort))},
		}
		if err := m.ovsClient.OpenFlow.AddFlow(common.DPUBridge, flow); err != nil {
			log.Fatalf("failed to add flow on OVS bridge %s: %v", common.DPUBridge, err)
		}

		// Only add flows from local pod to peer pod
		// sudo ovs-ofctl add-flow br-12 "dl_src=00:00:00:00:01:01, dl_dst=00:00:00:00:02:01, actions=normal"
		flow = &ovs.Flow{
			Matches: []ovs.Match{
				ovs.DataLinkSource(link.LocalMac),
				ovs.DataLinkDestination(link.PeerMac),
			},
			Actions: []ovs.Action{ovs.Normal()},
		}
		if err := m.ovsClient.OpenFlow.AddFlow(common.HostBridge, flow); err != nil {
			log.Fatalf("failed to add flow on OVS bridge %s: %v", common.HostBridge, err)
		}

		elapsed := time.Since(startTime)
		m.latencyHistograms.Observe("add_flow_same_host", elapsed.Milliseconds())
		logger.Infof("Successfully added flow on same host in %v", elapsed)
	} else { // This means we're on different hosts

		// The remote will handle its own vxlan creation
		logger.Infof("%s@%s and %s@%s are on different hosts", localPod.Name, localPod.SrcIp, peerPod.Name, peerPod.SrcIp)

		// This flow table item may be added multiple times
		// sudo ovs-ofctl add-flow br-dpu-12 "dl_dst=00:00:00:00:03:01, actions=output:vxlan-13"
		flow := &ovs.Flow{
			Matches: []ovs.Match{
				ovs.DataLinkDestination(link.PeerMac),
			},
			Actions: []ovs.Action{ovs.Output(m.GetPortID(common.DPUBridge, common.GetVxlanOutPortName(peerPod.SrcIp)))},
		}
		if err := m.ovsClient.OpenFlow.AddFlow(common.DPUBridge, flow); err != nil {
			log.Fatalf("failed to add flow on OVS bridge %s: %v", common.DPUBridge, err)
		}

		// Only add flows from local pod to peer pod
		// sudo ovs-ofctl add-flow br-12 "dl_src=00:00:00:00:01:02, dl_dst=00:00:00:00:03:01, actions=output:patch-todpu"
		flow = &ovs.Flow{
			Matches: []ovs.Match{
				ovs.DataLinkSource(link.LocalMac),
				ovs.DataLinkDestination(link.PeerMac),
			},
			Actions: []ovs.Action{ovs.Output(m.GetPortID(common.HostBridge, common.ToDPUPort))},
		}
		if err := m.ovsClient.OpenFlow.AddFlow(common.HostBridge, flow); err != nil {
			log.Fatalf("failed to add flow on OVS bridge %s: %v", common.HostBridge, err)
		}

		elapsed := time.Since(startTime)
		m.latencyHistograms.Observe("add_flow_diff_host", elapsed.Milliseconds())
		logger.Infof("Successfully added flow on different host in %v", elapsed)

	}

	// Multicast
	dstMac, err := net.ParseMAC(link.PeerMac)
	if err != nil {
		log.Fatalf("failed to parse mac %s: %v", link.PeerMac, err)
	}
	// sudo ovs-ofctl add-flow br-12 "dl_src=00:00:00:00:01:01, dl_dst=ff:ff:ff:ff:ff:ff, actions=mod_dl_dst=00:00:00:00:02:01, resubmit(,0)"
	flow = &ovs.Flow{
		Matches: []ovs.Match{
			ovs.DataLinkSource(link.LocalMac),
			ovs.DataLinkDestination(common.ALL_ONE_MAC),
		},
		Actions: []ovs.Action{ovs.ModDataLinkDestination(dstMac), ovs.Resubmit(0, 0)},
	}
	if err := m.ovsClient.OpenFlow.AddFlow(common.HostBridge, flow); err != nil {
		log.Fatalf("failed to add flow on OVS bridge %s: %v", common.HostBridge, err)
	}
	// sudo ovs-ofctl add-flow br-12 "dl_src=00:00:00:00:01:01, dl_dst=00:00:00:00:00:00, actions=mod_dl_dst=00:00:00:00:02:01, resubmit(,0)"
	flow = &ovs.Flow{
		Matches: []ovs.Match{
			ovs.DataLinkSource(link.LocalMac),
			ovs.DataLinkDestination(common.ALL_ZERO_MAC),
		},
		Actions: []ovs.Action{ovs.ModDataLinkDestination(dstMac), ovs.Resubmit(0, 0)},
	}
	if err := m.ovsClient.OpenFlow.AddFlow(common.HostBridge, flow); err != nil {
		log.Fatalf("failed to add flow on OVS bridge %s: %v", common.HostBridge, err)
	}

	return nil
}

func (m *KubeDTN) delLink(ctx context.Context, localPod *pb.Pod, link *pb.Link) error {
	logger := common.GetLogger(ctx).WithFields(log.Fields{
		"link": link.Uid,
	})
	// ctx = common.WithLogger(ctx, logger)
	logger.Infof("Deleting link: %v", link)
	startTime := time.Now()

	// Creating koko's Veth struct for local intf
	myVeth, err := common.MakeVeth(ctx, localPod.NetNs, link.LocalIntf, link.LocalIp, link.LocalMac)
	if err != nil {
		logger.Infof("Failed to construct koko Veth struct")
		return err
	}

	// API call to koko to remove local Veth link
	// go func() {
	// if err = myVeth.RemoveVethLink(); err != nil {
	// 	// instead of failing, just log the error and move on
	// 	logger.Infof("Failed to remove veth link: %s", err)
	// }

	if err := fastlink.RemoveVethLink(ctx, myVeth, m.latencyHistograms); err != nil {
		logger.Infof("Failed to remove veth link: %s", err)
	}

	elapsed := time.Since(startTime)
	m.latencyHistograms.Observe("del", elapsed.Milliseconds())
	logger.Infof("Successfully deleted link in %v", elapsed)
	// }()

	// vni := common.GetVniFromUid(link.Uid)
	// netns := m.vxlanManager.Get(vni)
	// if netns != nil && *netns == localPod.NetNs {
	// 	m.vxlanManager.Delete(vni)
	// }

	return nil
}

// Setup a pod, create veth and connect to ovs bridge
func (m *KubeDTN) SetupPod(ctx context.Context, pod *pb.SetupPodQuery) (*pb.BoolResponse, error) {
	logger := common.GetLogger(ctx).WithFields(log.Fields{
		"pod":    pod.Name,
		"ns":     pod.KubeNs,
		"action": "setup",
	})
	ctx = common.WithLogger(ctx, logger)
	logger.Infof("SetupPod: Setting up pod")

	redisTopoSpec, err := m.getTopoSpecFromRedis(pod.Name)
	if err != nil {
		logger.Infof("Failed to retrieve peer pod %s/%s topology", pod.KubeNs, pod.Name)
	}

	// Marking pod as "alive" by setting its srcIP and NetNS
	localPod := &pb.Pod{
		Name:   pod.Name,
		KubeNs: pod.KubeNs,
		SrcIp:  m.nodeIP,
		NetNs:  pod.NetNs,
		Links:  common.Map(redisTopoSpec.Links, func(link v1.Link) *pb.Link { return link.ToProto() }),
		Safe:   true,
	}

	logger.Infof("SetupPod: Setting pod alive status")
	ok, err := m.SetAlive(ctx, localPod)
	if err != nil || !ok.Response {
		logger.Infof("SetupPod: Failed to set pod alive status: %v", err)
		return &pb.BoolResponse{Response: false}, err
	}

	// Clean up any links
	for _, link := range localPod.Links {
		link.Detect = true
	}

	m.PrintOVS()
	logger.Infof("SetupPod: start add links")

	response, err := m.AddLinks(ctx, &pb.LinksBatchQuery{
		LocalPod: localPod,
		Links:    localPod.Links,
	})

	logger.Infof("SetupPod: finish add links")

	m.PrintOVS()

	if err != nil || !response.Response {
		logger.Infof("SetupPod: Failed to setup links: %v", err)
		return &pb.BoolResponse{Response: false}, err
	}

	logger.Infof("SetupPod: Successfully set up pod")
	return &pb.BoolResponse{Response: true}, nil
}

// Destroy a pod, removing all its GRPC wires and links, the reverse process of SetupPod
func (m *KubeDTN) DestroyPod(ctx context.Context, pod *pb.PodQuery) (*pb.BoolResponse, error) {
	logger := common.GetLogger(ctx).WithFields(log.Fields{
		"pod":    pod.Name,
		"ns":     pod.KubeNs,
		"action": "destroy",
	})
	ctx = common.WithLogger(ctx, logger)
	logger.Infof("DestroyPod: Destroying pod")

	// Close the grpc tunnel for this pod netns (if any)
	wireDef := pb.WireDef{
		KubeNs:       string(pod.KubeNs),
		LocalPodName: string(pod.Name),
	}

	removResp, err := m.RemGRPCWire(ctx, &wireDef)
	if err != nil || !removResp.Response {
		return &pb.BoolResponse{Response: false}, fmt.Errorf("DestroyPod: could not remove grpc wire: %v", err)
	}

	localPod := &pb.Pod{
		Name: pod.Name,
		Safe: true,
	}

	redisTopoSpec, err := m.getTopoSpecFromRedis(pod.Name)
	if err != nil {
		logger.Infof("Failed to retrieve peer pod %s/%s topology, topology has may already be removed", pod.KubeNs, pod.Name)
	}

	localPod.Links = common.Map(redisTopoSpec.Links, func(link v1.Link) *pb.Link { return link.ToProto() })

	logger.Infof("DestroyPod: Set the pod as dead")
	// By setting srcIP and NetNS to "" we're marking this POD as dead
	localPod.NetNs = ""
	localPod.SrcIp = ""
	_, err = m.SetAlive(ctx, localPod)
	// If status is not in redis, no links need to be deleted
	if err != nil {
		return &pb.BoolResponse{Response: true}, nil
	}

	response, err := m.DelLinks(ctx, &pb.LinksBatchQuery{
		LocalPod: localPod,
		Links:    localPod.Links,
	})
	if err != nil || !response.Response {
		logger.Infof("DestroyPod: Failed to cleanup all links: %v", err)
	} else {
		logger.Infof("DestroyPod: Successfully destroyed pod")
	}

	return &pb.BoolResponse{Response: true}, nil
}

func (m *KubeDTN) AddLinks(ctx context.Context, query *pb.LinksBatchQuery) (*pb.BoolResponse, error) {
	localPod := query.LocalPod
	logger := common.GetLogger(ctx).WithFields(log.Fields{
		"pod":    localPod.Name,
		"ns":     localPod.KubeNs,
		"action": "add",
	})
	ctx = common.WithLogger(ctx, logger)

	for _, link := range query.Links {
		err := m.addLink(ctx, localPod, link)
		if err != nil {
			logger.WithField("link", link.Uid).Errorf("Failed to add link: %v", err)
			return &pb.BoolResponse{Response: false}, err
		}
	}

	logger.Infof("Successfully added links")
	return &pb.BoolResponse{Response: true}, nil
}

func (m *KubeDTN) DelLinks(ctx context.Context, query *pb.LinksBatchQuery) (*pb.BoolResponse, error) {
	var latest_err error

	localPod := query.LocalPod
	logger := logger.WithFields(log.Fields{
		"pod":    localPod.Name,
		"ns":     localPod.KubeNs,
		"action": "delete",
	})
	ctx = common.WithLogger(ctx, logger)

	logger.Infof("Deleting links in netns %s, ip %s", localPod.NetNs, localPod.SrcIp)

	for _, link := range query.Links {
		latest_err = m.delLink(ctx, localPod, link)
		if latest_err != nil {
			break
		}
	}

	if latest_err != nil {
		log.Errorf("DelLinks: Failed to delete link")
		return &pb.BoolResponse{Response: false}, latest_err
	} else {
		log.Infof("DelLinks: Successfully deleted links")
		return &pb.BoolResponse{Response: true}, nil
	}

}

func (m *KubeDTN) UpdateLinks(ctx context.Context, query *pb.LinksBatchQuery) (*pb.BoolResponse, error) {
	localPod := query.LocalPod
	logger := common.GetLogger(ctx).WithFields(log.Fields{
		"pod":    localPod.Name,
		"ns":     localPod.KubeNs,
		"action": "update",
	})
	ctx = common.WithLogger(ctx, logger)
	ctx = common.WithCtxValue(ctx, common.TCPIP_BYPASS, m.config.TCPIPBypass)

	for _, link := range query.Links {
		logger := logger.WithField("link", link.Uid)
		logger.Infof("Updating link")
		startTime := time.Now()

		myVeth, err := common.MakeVeth(ctx, localPod.NetNs, link.LocalIntf, link.LocalIp, link.LocalMac)
		if err != nil {
			return &pb.BoolResponse{Response: false}, err
		}
		qdiscs, err := common.MakeQdiscs(ctx, link.Properties)
		if err != nil {
			logger.Errorf("Failed to construct qdiscs: %s", err)
			return &pb.BoolResponse{Response: false}, err
		}
		err = common.SetVethQdiscs(ctx, myVeth, qdiscs)
		if err != nil {
			logger.Errorf("Failed to update qdiscs on self veth %s: %v", myVeth, err)
			return &pb.BoolResponse{Response: false}, err
		}

		elapsed := time.Since(startTime)
		m.latencyHistograms.Observe("update", elapsed.Milliseconds())
		logger.Infof("Successfully updated link in %v", elapsed)
	}

	logger.Infof("Successfully updated links")
	return &pb.BoolResponse{Response: true}, nil
}
