package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/digitalocean/go-openvswitch/ovs"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/cni/pkg/version"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/y-young/kube-dtn/common"
	pb "github.com/y-young/kube-dtn/proto/v1"
)

var interNodeLinkType = common.INTER_NODE_LINK_VXLAN

// -------------------------------------------------------------------------------------------------
func init() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
}

// -------------------------------------------------------------------------------------------------
// loadConf loads information from cni.conf
func loadConf(bytes []byte) (*common.NetConf, *current.Result, error) {
	n := &common.NetConf{}
	if err := json.Unmarshal(bytes, n); err != nil {
		return nil, nil, fmt.Errorf("failed to load netconf: %v", err)
	}

	// Parse previous result.
	if n.RawPrevResult == nil {
		// return early if there was no previous result, which is allowed for DEL calls
		return n, &current.Result{}, nil
	}

	// Parse previous result.
	var result *current.Result
	var err error
	if err = version.ParsePrevResult(&n.NetConf); err != nil {
		return nil, nil, fmt.Errorf("could not parse prevResult: %v", err)
	}

	result, err = current.NewResultFromResult(n.PrevResult)
	if err != nil {
		return nil, nil, fmt.Errorf("could not convert result to current version: %v", err)
	}
	return n, result, nil
}

// -------------------------------------------------------------------------------------------------
// Adds interfaces to a POD
func cmdAdd(args *skel.CmdArgs) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log.Info("Parsing cni .conf file")
	n, result, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}

	log.Info("Parsing CNI_ARGS environment variable")
	cniArgs := common.K8sArgs{}
	if err := types.LoadArgs(args.Args, &cniArgs); err != nil {
		return err
	}
	log.Infof("Processing ADD POD %s in namespace %s", cniArgs.K8S_POD_NAME, cniArgs.K8S_POD_NAMESPACE)

	log.Infof("Attempting to connect to local kubedtn daemon")
	conn, err := grpc.Dial(common.LocalDaemon, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Infof("Failed to connect to local kubedtnd on %s", common.LocalDaemon)
		return err
	}
	defer conn.Close()

	client := pb.NewLocalClient(conn)
	ok, err := client.SetupPod(ctx, &pb.SetupPodQuery{
		Name:   string(cniArgs.K8S_POD_NAME),
		KubeNs: string(cniArgs.K8S_POD_NAMESPACE),
		NetNs:  args.Netns,
	})

	if err != nil || !ok.Response {
		log.Infof("Failed to setup pod %s/%s, err: %v", string(cniArgs.K8S_POD_NAMESPACE), string(cniArgs.K8S_POD_NAME), err)
		return err
	}

	return types.PrintResult(result, n.CNIVersion)
}

// Deletes interfaces from a POD
func cmdDel(args *skel.CmdArgs) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cniArgs := common.K8sArgs{}
	if err := types.LoadArgs(args.Args, &cniArgs); err != nil {
		return err
	}
	log.Infof("Processing DEL request: %s", cniArgs.K8S_POD_NAME)

	log.Info("Parsing cni .conf file")
	n, result, err := loadConf(args.StdinData)
	if err != nil {
		return err
	}

	conn, err := grpc.Dial(common.LocalDaemon, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Infof("Failed to connect to local kubedtnd on %s", common.LocalDaemon)
		return err
	}
	defer conn.Close()

	client := pb.NewLocalClient(conn)
	ok, err := client.DestroyPod(ctx, &pb.PodQuery{
		Name:   string(cniArgs.K8S_POD_NAME),
		KubeNs: string(cniArgs.K8S_POD_NAMESPACE),
	})

	if !ok.Response {
		podName := fmt.Sprintf("%s/%s", string(cniArgs.K8S_POD_NAMESPACE), string(cniArgs.K8S_POD_NAME))
		if err != nil {
			log.Infof("Failed to remove pod %s, err: %v", podName, err)
			return err
		}
		// Response = false but no error, meaning that the pod was not a topology pod,
		// we should delegate the action to the next plugin
		log.Infof("Pod %s is not in topology returning", podName)
		return types.PrintResult(result, n.CNIVersion)
	}
	return nil
}

func SetInterNodeLinkType() {
	// TODO: Find a more appropriate (if any) way to figure out intended link type
	// As of today, daemon gets the intended link type from env INTER_NODE_LINK_TYPE
	// which is set by deployment file. The daemon further propagates this to plugin
	// via means of file on host (which is read below) containing the value GRPC or VXLAN
	b, err := os.ReadFile("/etc/cni/net.d/kubedtn-inter-node-link-type")
	if err != nil {
		log.Warningf("Could not read iner node link type: %v", err)
		// use the default value
		return
	}

	interNodeLinkType = string(b)
}

func CreateOVSBridges(c *ovs.Client) {
	// sudo ovs-vsctl --may-exist add-br ovs-br-host
	if err := c.VSwitch.AddBridge(common.HostBridge); err != nil {
		log.Fatalf("failed to add OVS bridge %s: %v", common.HostBridge, err)
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
}

func kubeConfig() (*rest.Config, error) {
	log.Infof("Trying in-cluster configuration")
	rCfg, err := rest.InClusterConfig()
	if err != nil {
		kubecfg := filepath.Join(".kube", "config")
		if home := homedir.HomeDir(); home != "" {
			kubecfg = filepath.Join(home, kubecfg)
		}
		log.Infof("Falling back to kubeconfig: %q", kubecfg)
		rCfg, err = clientcmd.BuildConfigFromFlags("", kubecfg)
		if err != nil {
			return nil, err
		}
	}
	return rCfg, nil
}

// IsNodeReady checks if a node is in Ready state
func IsNodeReady(node *v1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == v1.NodeReady && condition.Status == v1.ConditionTrue {
			return true
		}
	}
	return false
}

func GetNodesInfo() (map[string]string, error) {

	config, err := kubeConfig()
	if err != nil {
		log.Fatalf("Error building kubeconfig: %v\n", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Error creating Kubernetes client: %v\n", err)
	}

	nodes, err := clientset.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		log.Fatalf("Error getting nodes: %v\n", err)
	}

	nodeInfoMap := make(map[string]string)

	for _, node := range nodes.Items {
		if IsNodeReady(&node) {
			// Extracting node internal IP address
			var internalIP string
			for _, address := range node.Status.Addresses {
				if address.Type == v1.NodeInternalIP {
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
	portName := common.VxlanOutPortPrefix + "-" + remoteName
	if err := c.VSwitch.AddPort(common.DPUBridge, portName); err != nil {
		log.Fatalf("failed to add port %s (remoteName %s, remoteIP %s) on OVS bridge %s: %v", portName, remoteName, remoteIP, common.DPUBridge, err)
	}
	if err := c.VSwitch.Set.Interface(portName, ovs.InterfaceOptions{
		Type:     ovs.InterfaceTypeVXLAN,
		RemoteIP: remoteIP,
	}); err != nil {
		log.Fatalf("failed to set port %s interface: %v", portName, err)
	}
}

func InitOVSBridges() {

	// Create a *ovs.Client
	c := ovs.New(
		// Prepend "sudo" to all commands.
		ovs.Sudo(),
	)

	CreateOVSBridges(c)

	// TODO: Use CRD to store and get ips of all nodes in etcd
	nodesInfo, err := GetNodesInfo()
	if err != nil {
		log.Fatalf("failed to get node info: %v", err)
	}
	for name, ip := range nodesInfo {
		fmt.Printf("%s\t%s\n", name, ip)
		ConnectBridgesBetweenNodes(c, name, ip)
	}

}

// -------------------------------------------------------------------------------------------------
func main() {
	fp, err := os.OpenFile("/var/log/kubedtn-cni.log", os.O_APPEND|os.O_CREATE|os.O_RDWR, 0666)
	if err == nil {
		log.SetOutput(fp)
	}

	SetInterNodeLinkType()
	_ = interNodeLinkType
	// log.Infof("INTER_NODE_LINK_TYPE: %v", interNodeLinkType)

	// Init two OVS bridges on node before starting cni plugin
	InitOVSBridges()
	log.Infof("OVS Bridges init finished")

	retCode := 0
	e := skel.PluginMainWithError(cmdAdd, cmdCheck, cmdDel, version.All, "CNI plugin kubedtn v0.3.0")
	if e != nil {
		log.Errorf("failed to run kubedtn cni: %v", e.Print())
		retCode = 1
	}
	fp.Close()
	os.Exit(retCode)
}

func cmdCheck(args *skel.CmdArgs) error {
	log.Infof("cmdCheck called: %+v", args)
	return fmt.Errorf("not implemented")
}

//-------------------------------------------------------------------------------------------------
