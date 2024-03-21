package kubedtn

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/digitalocean/go-openvswitch/ovs"
	v1 "github.com/y-young/kube-dtn/api/v1"

	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_ctxtags "github.com/grpc-ecosystem/go-grpc-middleware/tags"

	glogrus "github.com/grpc-ecosystem/go-grpc-middleware/logging/logrus"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"

	topologyclientv1 "github.com/y-young/kube-dtn/api/clientset/v1beta1"
	"github.com/y-young/kube-dtn/common"
	"github.com/y-young/kube-dtn/daemon/metrics"
	"github.com/y-young/kube-dtn/daemon/vxlan"
	k8sv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/go-redis/redis/v8"
	pb "github.com/y-young/kube-dtn/proto/v1"
)

type Config struct {
	Port        int
	GRPCOpts    []grpc.ServerOption
	TCPIPBypass bool
}

type KubeDTN struct {
	pb.UnimplementedLocalServer
	pb.UnimplementedRemoteServer
	pb.UnimplementedWireProtocolServer
	config            Config
	kClient           kubernetes.Interface
	tClient           topologyclientv1.Interface
	rCfg              *rest.Config
	s                 *grpc.Server
	lis               net.Listener
	topologyStore     cache.Store
	topologyManager   *metrics.TopologyManager
	vxlanManager      *vxlan.VxlanManager
	latencyHistograms *metrics.LatencyHistograms
	linkMutexes       *common.MutexMap
	redis             *redis.Client
	ctx               context.Context
	ovsClient         *ovs.Client
	// Key is node name, Value is node IP
	nodesInfo map[string]string
	// IP of the node on which the daemon is running.
	nodeIP string
	// VXLAN interface name.
	vxlanIntf string
}

var logger *log.Entry = nil

func InitLogger() {
	logger = log.WithFields(log.Fields{"daemon": "kubedtnd"})
}

func restConfig() (*rest.Config, error) {
	logger.Infof("Trying in-cluster configuration")
	rCfg, err := rest.InClusterConfig()
	if err != nil {
		kubecfg := filepath.Join(".kube", "config")
		if home := homedir.HomeDir(); home != "" {
			kubecfg = filepath.Join(home, kubecfg)
		}
		logger.Infof("Falling back to kubeconfig: %q", kubecfg)
		rCfg, err = clientcmd.BuildConfigFromFlags("", kubecfg)
		if err != nil {
			return nil, err
		}
	}
	return rCfg, nil
}

func GetPortID(bridge, port string) int {
	portID, err := common.GetPortID(bridge, port)
	if err != nil {
		log.Fatalf("%v", err)
	}
	return portID
}

func CreateOVSBridges(c *ovs.Client) {
	// sudo ovs-vsctl --may-exist add-br ovs-br-host
	if err := c.VSwitch.AddBridge(common.HostBridge); err != nil {
		log.Fatalf("failed to add OVS bridge %s: %v", common.HostBridge, err)
	}
	// sudo ovs-ofctl del-flows ovs-br-host
	if err := c.OpenFlow.DelFlows(common.HostBridge, &ovs.MatchFlow{}); err != nil {
		log.Fatalf("failed to del-flows when initialize in OVS bridge %s: %v", common.HostBridge, err)
	}
	// sudo ovs-vsctl set bridge ovs-br-host datapath_type=system
	cmd := exec.Command("ovs-vsctl", "set", "bridge", common.HostBridge, "datapath_type=system")
	err := cmd.Run()
	if err != nil {
		log.Fatalf("error setting OVS bridge %s datapath type: %v", common.HostBridge, err)
	}

	// sudo ovs-vsctl --may-exist add-br ovs-br-dpu
	if err := c.VSwitch.AddBridge(common.DPUBridge); err != nil {
		log.Fatalf("failed to add OVS bridge %s: %v", common.DPUBridge, err)
	}
	// sudo ovs-ofctl del-flows ovs-br-dpu
	if err := c.OpenFlow.DelFlows(common.DPUBridge, &ovs.MatchFlow{}); err != nil {
		log.Fatalf("failed to del-flows when initialize in OVS bridge %s: %v", common.DPUBridge, err)
	}

	// sudo ovs-vsctl add-port ovs-br-host patch-to-dpu -- set interface patch-to-dpu type=patch options:peer=patch-to-host
	if err := c.VSwitch.AddPort(common.HostBridge, common.ToDPUPort); err != nil {
		log.Fatalf("failed to add port %s on OVS bridge %s: %v", common.ToDPUPort, common.HostBridge, err)
	}
	if err := c.VSwitch.Set.Interface(common.ToDPUPort, ovs.InterfaceOptions{
		Type: ovs.InterfaceTypePatch,
		Peer: common.ToHostPort,
	}); err != nil {
		log.Fatalf("failed to set port %s interface: %v", common.ToDPUPort, err)
	}

	// sudo ovs-vsctl add-port ovs-br-dpu patch-to-host -- set interface patch-to-host type=patch options:peer=patch-to-dpu
	if err := c.VSwitch.AddPort(common.DPUBridge, common.ToHostPort); err != nil {
		log.Fatalf("failed to add port %s on OVS bridge %s: %v", common.ToHostPort, common.DPUBridge, err)
	}
	if err := c.VSwitch.Set.Interface(common.ToHostPort, ovs.InterfaceOptions{
		Type: ovs.InterfaceTypePatch,
		Peer: common.ToDPUPort,
	}); err != nil {
		log.Fatalf("failed to set port %s interface: %v", common.ToHostPort, err)
	}

	// sudo ovs-ofctl add-flow ovs-br-host in_port=patch-to-dpu,actions=normal
	flow := &ovs.Flow{
		InPort:  GetPortID(common.HostBridge, common.ToDPUPort),
		Actions: []ovs.Action{ovs.Normal()},
	}
	if err := c.OpenFlow.AddFlow(common.HostBridge, flow); err != nil {
		log.Fatalf("failed to add flow on OVS bridge %s: %v", common.HostBridge, err)
	}
}

// IsNodeReady checks if a node is in Ready state
func IsNodeReady(node *k8sv1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == k8sv1.NodeReady && condition.Status == k8sv1.ConditionTrue {
			return true
		}
	}
	return false
}

func GetNodesInfo(kClient kubernetes.Interface) (map[string]string, error) {

	nodes, err := kClient.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		log.Fatalf("Error getting nodes: %v\n", err)
	}

	nodeInfoMap := make(map[string]string)

	for _, node := range nodes.Items {
		if IsNodeReady(&node) {
			// Extracting node internal IP address
			var internalIP string
			for _, address := range node.Status.Addresses {
				if address.Type == k8sv1.NodeInternalIP {
					internalIP = address.Address
					break
				}
			}

			// Adding node name and internal IP to the map
			nodeInfoMap[node.ObjectMeta.Name] = internalIP
		}
	}

	return nodeInfoMap, err
}

func ConnectBridgesBetweenNodes(c *ovs.Client, remoteName string, remoteIP string) {
	// sudo ovs-vsctl add-port ovs-br-dpu vxlan-out-remote-name -- set interface vxlan-out-remote-name type=vxlan options:remote_ip=remoteIP options:dst_port=8472
	portName := common.GetVxlanOutPortName(remoteIP)
	if err := c.VSwitch.AddPort(common.DPUBridge, portName); err != nil {
		log.Fatalf("failed to add port %s (remoteName %s, remoteIP %s) on OVS bridge %s: %v", portName, remoteName, remoteIP, common.DPUBridge, err)
	}
	log.Infof("added port %s (remoteName %s, remoteIP %s) on OVS bridge %s", portName, remoteName, remoteIP, common.DPUBridge)

	if err := c.VSwitch.Set.Interface(portName, ovs.InterfaceOptions{
		Type:     ovs.InterfaceTypeVXLAN,
		RemoteIP: remoteIP,
	}); err != nil {
		log.Fatalf("failed to set port %s interface: %v", portName, err)
	}

	// sudo ovs-ofctl add-flow ovs-br-dpu in_port=vxlan-13,actions=output:patch-to-host
	flow := &ovs.Flow{
		InPort:  GetPortID(common.DPUBridge, portName),
		Actions: []ovs.Action{ovs.Output(GetPortID(common.DPUBridge, common.ToHostPort))},
	}
	if err := c.OpenFlow.AddFlow(common.DPUBridge, flow); err != nil {
		log.Fatalf("failed to add flow on OVS bridge %s: %v", common.DPUBridge, err)
	}

}

func CleanOVSBridges(c *ovs.Client) {
	// sudo ovs-ofctl del-flows br-name
	if err := c.OpenFlow.DelFlows(common.HostBridge, nil); err != nil {
		log.Infof("failed to delete OVS bridge %s flow at beginning: %v", common.HostBridge, err)
	}
	if err := c.OpenFlow.DelFlows(common.DPUBridge, nil); err != nil {
		log.Infof("failed to delete OVS bridge %s flow at beginning: %v", common.DPUBridge, err)
	}
	// sudo ovs-vsctl del-br br-name
	if err := c.VSwitch.DeleteBridge(common.HostBridge); err != nil {
		log.Infof("failed to clean OVS bridge %s at beginning: %v", common.HostBridge, err)
	}
	if err := c.VSwitch.DeleteBridge(common.DPUBridge); err != nil {
		log.Infof("failed to clean OVS bridge %s at beginning: %v", common.DPUBridge, err)
	}
}

func InitOVSBridges(c *ovs.Client, nodesInfo map[string]string) {

	CreateOVSBridges(c)

	// TODO: Use CRD to store and get ips of all nodes in etcd
	for name, ip := range nodesInfo {
		ConnectBridgesBetweenNodes(c, name, ip)
	}
}

func New(cfg Config, topologyManager *metrics.TopologyManager, latencyHistograms *metrics.LatencyHistograms) (*KubeDTN, error) {
	rCfg, err := restConfig()
	if err != nil {
		return nil, err
	}
	kClient, err := kubernetes.NewForConfig(rCfg)
	if err != nil {
		return nil, err
	}
	tClient, err := topologyclientv1.NewForConfig(rCfg)
	if err != nil {
		return nil, err
	}
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.Port))
	if err != nil {
		return nil, err
	}

	nodeIP := os.Getenv("HOST_IP")

	ctx := context.Background()
	topologies, err := tClient.Topology("").List(ctx, metav1.ListOptions{})
	logger.Infof("Found %d local topologies", len(topologies.Items))
	if err != nil {
		return nil, fmt.Errorf("failed to list topologies: %v", err)
	}
	localTopologies := filterLocalTopologies(topologies, &nodeIP)

	err = topologyManager.Init(localTopologies)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize topology manager: %v", err)
	}

	vxlanManager := vxlan.NewVxlanManager()
	vxlanManager.Init(localTopologies)

	_, vxlanIntf, err := vxlan.GetVxlanSource(nodeIP)
	if err != nil {
		return nil, fmt.Errorf("failed to get vxlan source: %v", err)
	}
	logger.Infof("Node IP: %s, VXLAN interface: %s", nodeIP, vxlanIntf)

	store, controller := cache.NewInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return tClient.Topology("").List(ctx, options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return tClient.Topology("").Watch(ctx, options)
			},
		},
		&v1.Topology{},
		0,
		cache.ResourceEventHandlerFuncs{},
	)

	go controller.Run(wait.NeverStop)

	redisClient := redis.NewClient(&redis.Options{
		Addr:     "10.0.0.11:6379",
		Password: "sail123456",
		DB:       0,
	})
	linkMutexes := common.NewMutexMap()

	// Do not prepend sudo here
	ovsClient := ovs.New()

	nodesInfoMap, err := GetNodesInfo(kClient)
	if err != nil {
		log.Fatalf("failed to get node info: %v", err)
	}

	// Clean existing bridges created by kubedtn before
	CleanOVSBridges(ovsClient)
	// Init two OVS bridges on node before starting cni plugin
	InitOVSBridges(ovsClient, nodesInfoMap)
	log.Infof("OVS Bridges init finished")

	m := &KubeDTN{
		config:            cfg,
		rCfg:              rCfg,
		kClient:           kClient,
		tClient:           tClient,
		lis:               lis,
		s:                 newServerWithLogging(cfg.GRPCOpts...),
		topologyStore:     store,
		topologyManager:   topologyManager,
		vxlanManager:      vxlanManager,
		latencyHistograms: latencyHistograms,
		linkMutexes:       &linkMutexes,
		nodeIP:            nodeIP,
		vxlanIntf:         vxlanIntf,
		redis:             redisClient,
		ovsClient:         ovsClient,
		nodesInfo:         nodesInfoMap,
		ctx:               context.Background(),
	}
	pb.RegisterLocalServer(m.s, m)
	pb.RegisterRemoteServer(m.s, m)
	pb.RegisterWireProtocolServer(m.s, m)
	reflection.Register(m.s)
	return m, nil
}

func (m *KubeDTN) Serve() error {
	defer CleanOVSBridges(m.ovsClient)
	m.StartAddFlowListener()
	logger.Infof("GRPC server has started on port: %d", m.config.Port)
	return m.s.Serve(m.lis)
}

func (m *KubeDTN) Stop() {
	m.s.Stop()
}

func newServerWithLogging(opts ...grpc.ServerOption) *grpc.Server {
	lEntry := log.NewEntry(log.StandardLogger())
	lOpts := []glogrus.Option{}
	glogrus.ReplaceGrpcLogger(lEntry)
	opts = append(opts,
		grpc_middleware.WithUnaryServerChain(
			grpc_ctxtags.UnaryServerInterceptor(grpc_ctxtags.WithFieldExtractor(grpc_ctxtags.CodeGenRequestFieldExtractor)),
			glogrus.UnaryServerInterceptor(lEntry, lOpts...),
		),
		grpc_middleware.WithStreamServerChain(
			grpc_ctxtags.StreamServerInterceptor(grpc_ctxtags.WithFieldExtractor(grpc_ctxtags.CodeGenRequestFieldExtractor)),
			glogrus.StreamServerInterceptor(lEntry, lOpts...),
		))
	return grpc.NewServer(opts...)
}

func filterLocalTopologies(topologies *v1.TopologyList, nodeIP *string) *v1.TopologyList {
	filtered := &v1.TopologyList{}
	for _, topology := range topologies.Items {
		topology := topology
		if topology.Status.SrcIP == *nodeIP {
			filtered.Items = append(filtered.Items, topology)
		}
	}
	return filtered
}
