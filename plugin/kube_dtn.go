package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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

	kubedtnClient := pb.NewLocalClient(conn)

	log.Infof("Retrieving local pod information from kubedtn daemon")
	localPod, err := kubedtnClient.Get(ctx, &pb.PodQuery{
		Name:   string(cniArgs.K8S_POD_NAME),
		KubeNs: string(cniArgs.K8S_POD_NAMESPACE),
	})
	if err != nil {
		log.Infof("Pod %s:%s was not a topology pod returning", string(cniArgs.K8S_POD_NAMESPACE), string(cniArgs.K8S_POD_NAME))
		return types.PrintResult(result, n.CNIVersion)
	}

	// Finding the source IP and interface for VXLAN VTEP
	srcIP, srcIntf, err := common.GetVxlanSource(localPod.NodeIp)
	if err != nil {
		return err
	}
	log.Infof("VxLan route is via %s@%s", srcIP, srcIntf)

	// Marking pod as "alive" by setting its srcIP and NetNS
	localPod.NetNs = args.Netns
	localPod.SrcIp = srcIP
	log.Infof("Setting pod alive status on kubedtn daemon")
	ok, err := kubedtnClient.SetAlive(ctx, localPod)
	if err != nil || !ok.Response {
		log.Info("Failed to set pod alive status")
		return err
	}

	log.Info("Starting to traverse all links")
	for _, link := range localPod.Links { // Iterate over each link of the local pod
		// Call daemon to add link
		ok, err = kubedtnClient.AddLink(ctx, &pb.AddLinkQuery{
			LocalPod: localPod,
			Link:     link,
		})
		if err != nil || !ok.Response {
			log.Infof("Failed to add link %s: %s", link.String(), err)
			return err
		}
	}

	return types.PrintResult(result, n.CNIVersion)
}

// -------------------------------------------------------------------------------------------------
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

	kubedtnClient := pb.NewLocalClient(conn)

	/* Tell daemon to close the grpc tunnel for this pod netns (if any) */
	log.Infof("Retrieving pod's metadata from kubedtn daemon")
	wireDef := pb.WireDef{
		KubeNs:       string(cniArgs.K8S_POD_NAMESPACE),
		LocalPodName: string(cniArgs.K8S_POD_NAME),
	}

	removResp, err := kubedtnClient.RemGRPCWire(ctx, &wireDef)
	if err != nil || !removResp.Response {
		return fmt.Errorf("could not remove grpc wire: %v", err)
	}

	log.Infof("Retrieving pod's (%s@%s) metadata from kubedtn daemon", string(cniArgs.K8S_POD_NAME), string(cniArgs.K8S_POD_NAMESPACE))
	localPod, err := kubedtnClient.Get(ctx, &pb.PodQuery{
		Name:   string(cniArgs.K8S_POD_NAME),
		KubeNs: string(cniArgs.K8S_POD_NAMESPACE),
	})
	if err != nil {
		log.Infof("Pod %s:%s is not in topology returning. err:%v", string(cniArgs.K8S_POD_NAMESPACE), string(cniArgs.K8S_POD_NAME), err)
		return types.PrintResult(result, n.CNIVersion)
	}

	log.Infof("Topology data still exists in CRs, cleaning up it's status")
	// By setting srcIP and NetNS to "" we're marking this POD as dead
	localPod.NetNs = ""
	localPod.SrcIp = ""
	_, err = kubedtnClient.SetAlive(ctx, localPod)
	if err != nil {
		return fmt.Errorf("could not set alive: %v", err)
	}

	log.Infof("Iterating over each link for clean-up")
	for _, link := range localPod.Links { // Iterate over each link of the local pod
		// Call daemon to remove link
		ok, err := kubedtnClient.DelLink(ctx, &pb.DelLinkQuery{
			LocalPod: localPod,
			Link:     link,
		})
		if err != nil || !ok.Response {
			log.Infof("Failed to remove link %s: %s", link.String(), err)
			return err
		}
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

// -------------------------------------------------------------------------------------------------
func main() {
	fp, err := os.OpenFile("/var/log/kubedtn-cni.log", os.O_APPEND|os.O_CREATE|os.O_RDWR, 0666)
	if err == nil {
		log.SetOutput(fp)
	}

	SetInterNodeLinkType()
	log.Infof("INTER_NODE_LINK_TYPE: %v", interNodeLinkType)

	retCode := 0
	e := skel.PluginMainWithError(cmdAdd, cmdGet, cmdDel, version.All, "CNI plugin kubedtn v0.3.0")
	if e != nil {
		log.Errorf("failed to run kubedtn cni: %v", e.Print())
		retCode = 1
	}
	log.Infof("kubedtn cni call successful")
	fp.Close()
	os.Exit(retCode)
}

func cmdGet(args *skel.CmdArgs) error {
	log.Infof("cmdGet called: %+v", args)
	return fmt.Errorf("not implemented")
}

//-------------------------------------------------------------------------------------------------
