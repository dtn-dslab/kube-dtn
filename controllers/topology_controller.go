/*
Copyright 2022.

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

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"reflect"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"networkop.co.uk/meshnet/api/v1beta1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	mpb "github.com/networkop/meshnet-cni/daemon/proto/meshnet/v1beta1"
	koko "github.com/redhat-nfvpe/koko/api"
	"github.com/sirupsen/logrus"
)

const (
	vxlanBase   = 5000
	defaultPort = "51111"
	// macvlanMode           = netlink.MACVLAN_MODE_BRIDGE
	INTER_NODE_LINK_VXLAN = "VXLAN"
	INTER_NODE_LINK_GRPC  = "GRPC"
)

// makeVeth creates koko.Veth from NetNS and LinkName
func makeVeth(netNS, linkName string, ip string) (*koko.VEth, error) {
	logrus.Infof("Creating Veth struct with NetNS:%s and intfName: %s, IP:%s", netNS, linkName, ip)
	veth := koko.VEth{}
	veth.NsName = netNS
	veth.LinkName = linkName
	if ip != "" {
		ipAddr, ipSubnet, err := net.ParseCIDR(ip)
		if err != nil {
			return nil, fmt.Errorf("failed to parse CIDR %s: %s", ip, err)
		}
		veth.IPAddr = []net.IPNet{{
			IP:   ipAddr,
			Mask: ipSubnet.Mask,
		}}
	}
	return &veth, nil
}

// TopologyReconciler reconciles a Topology object
type TopologyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=networkop.co.uk,resources=topologies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=networkop.co.uk,resources=topologies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=networkop.co.uk,resources=topologies/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Topology object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (r *TopologyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// TODO(user): your logic here
	var topology v1beta1.Topology
	if err := r.Get(ctx, req.NamespacedName, &topology); err != nil {
		log.Info("Unable to fetch Topology", "error", err)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if topology.ObjectMeta.Generation == 1 {
		logrus.Infof("Topology %s created", topology.Name)
		return ctrl.Result{}, nil
	}

	jsonData := topology.Annotations["kubectl.kubernetes.io/last-applied-configuration"]
	var lastConfig v1beta1.Topology
	if err := json.Unmarshal([]byte(jsonData), &lastConfig); err != nil {
		logrus.Infof("Failed to decode JSON")
		return ctrl.Result{}, nil
	}

	// Spec not changed, nothing to do
	if reflect.DeepEqual(topology.Status.Links, topology.Spec.Links) {
		return ctrl.Result{}, nil
	}

	logrus.Infof("Topology %s changed", topology.Name)
	add, del := r.CalcDiff(topology.Status.Links, topology.Spec.Links)
	fmt.Printf("Topology %s changed,\n\tadd: %v,\n\tdel: %v\n", topology.Name, add, del)

	topology.Status.Links = topology.Spec.Links
	if err := r.Status().Update(ctx, &topology); err != nil {
		logrus.Errorf("Topology %s: Failed to update status: %s", topology.Name, err)
		return ctrl.Result{}, err
	}

	// obj, _ := json.MarshalIndent(topology, "", "  ")
	// logrus.Infof("Topology: %s", obj)

	return ctrl.Result{}, nil
}

func (r *TopologyReconciler) AddLinks(ctx context.Context, topology *v1beta1.Topology, links []v1beta1.Link) error {
	daemonAddr := topology.Status.SrcIp + ":" + defaultPort
	conn, err := grpc.Dial(daemonAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		logrus.Infof("Failed to connect to daemon on %s", daemonAddr)
		return err
	}
	defer conn.Close()

	meshnetClient := mpb.NewLocalClient(conn)

	for _, link := range links {
		result, err := meshnetClient.AddLink(ctx, &mpb.AddLinkQuery{
			LocalPod: &mpb.Pod{
				Name:   topology.Name,
				SrcIp:  topology.Status.SrcIp,
				NetNs:  topology.Status.NetNs,
				KubeNs: topology.Namespace,
			},
			Link: &mpb.Link{
				PeerPod:   link.PeerPod,
				LocalIntf: link.LocalIntf,
				PeerIntf:  link.PeerIntf,
				LocalIp:   link.LocalIP,
				PeerIp:    link.PeerIP,
				Uid:       int64(link.UID),
			},
		})
		if err != nil {
			logrus.Errorf("Failed to add link %s: %s", link, err)
			return err
		}
		logrus.Infof("Result: %v", result)
	}

	// result, err := meshnetClient.DelLink(ctx, &mpb.DelLinkQuery{
	// 	LocalPod: &mpb.Pod{
	// 		Name:   topology.Name,
	// 		SrcIp:  topology.Status.SrcIp,
	// 		NetNs:  topology.Status.NetNs,
	// 		KubeNs: topology.Namespace,
	// 	},
	// 	Link: &mpb.Link{
	// 		PeerPod:   "r1",
	// 		LocalIntf: "eth1",
	// 		PeerIntf:  "eth2",
	// 		LocalIp:   "13.13.13.3/24",
	// 		PeerIp:    "13.13.13.1/24",
	// 		Uid:       2,
	// 	},
	// })
	return err
}

func (r *TopologyReconciler) Del(ctx context.Context, topology *v1beta1.Topology) error {
	daemonAddr := topology.Status.SrcIp + ":" + defaultPort
	conn, err := grpc.Dial(daemonAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		logrus.Infof("Failed to connect to daemon on %s", daemonAddr)
		return err
	}
	defer conn.Close()

	meshnetClient := mpb.NewLocalClient(conn)

	/* Tell daemon to close the grpc tunnel for this pod netns (if any) */
	logrus.Infof("Retrieving pod's metadata from meshnet daemon")
	wireDef := mpb.WireDef{
		KubeNs:       topology.Namespace,
		LocalPodName: topology.Name,
	}

	removResp, err := meshnetClient.RemGRPCWire(ctx, &wireDef)
	if err != nil || !removResp.Response {
		return fmt.Errorf("could not remove grpc wire: %v", err)
	}

	logrus.Infof("Retrieving pod's (%s@%s) metadata from meshnet daemon", topology.Name, topology.Namespace)
	localPod, err := meshnetClient.Get(ctx, &mpb.PodQuery{
		Name:   topology.Name,
		KubeNs: topology.Namespace,
	})
	if err != nil {
		logrus.Infof("Pod %s:%s is not in topology returning. err:%v", topology.Namespace, topology.Name, err)
		return err
	}

	logrus.Infof("Topology data still exists in CRs, cleaning up it's status")
	// By setting srcIP and NetNS to "" we're marking this POD as dead
	localPod.NetNs = ""
	localPod.SrcIp = ""
	_, err = meshnetClient.SetAlive(ctx, localPod)
	if err != nil {
		return fmt.Errorf("could not set alive: %v", err)
	}

	logrus.Infof("Iterating over each link for clean-up")
	for _, link := range localPod.Links { // Iterate over each link of the local pod
		r.DelLink(ctx, meshnetClient, localPod, link)
	}

	return nil
}

func (r *TopologyReconciler) DelLink(ctx context.Context, meshnetClient mpb.LocalClient, localPod *mpb.Pod, link *mpb.Link) error {
	// Creating koko's Veth struct for local intf
	myVeth, err := makeVeth(localPod.NetNs, link.LocalIntf, link.LocalIp)
	if err != nil {
		logrus.Infof("Failed to construct koko Veth struct")
		return err
	}

	logrus.Infof("Removing link %s", link.LocalIntf)
	// API call to koko to remove local Veth link
	if err = myVeth.RemoveVethLink(); err != nil {
		// instead of failing, just log the error and move on
		logrus.Infof("Error removing Veth link: %s", err)
	}

	// Setting reversed skipped flag so that this pod will try to connect veth pair on restart
	logrus.Infof("Setting skip-reverse flag on peer %s", link.PeerPod)
	ok, err := meshnetClient.SkipReverse(ctx, &mpb.SkipQuery{
		Pod:    localPod.Name,
		Peer:   link.PeerPod,
		KubeNs: localPod.KubeNs,
	})
	if err != nil || !ok.Response {
		logrus.Infof("Failed to set skip reversed flag on our peer %s", link.PeerPod)
		return err
	}

	return nil
}

// Calculate difference between two old links and new links, returns a list of links to be added and a list of links to be deleted
func (r *TopologyReconciler) CalcDiff(old []v1beta1.Link, new []v1beta1.Link) (add []v1beta1.Link, del []v1beta1.Link) {
	fmt.Printf("Before: %v\n", old)
	fmt.Printf("After: %v\n", new)

	for _, oldLink := range old {
		found := false
		for _, newLink := range new {
			if reflect.DeepEqual(oldLink, newLink) {
				found = true
				break
			}
		}
		if !found {
			del = append(del, oldLink)
		}
	}

	for _, newLink := range new {
		found := false
		for _, oldLink := range old {
			if reflect.DeepEqual(oldLink, newLink) {
				found = true
				break
			}
		}
		if !found {
			add = append(add, newLink)
		}
	}
	return
}

// SetupWithManager sets up the controller with the Manager.
func (r *TopologyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.Topology{}).
		Complete(r)
}
