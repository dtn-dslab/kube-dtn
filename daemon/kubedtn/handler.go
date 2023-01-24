package kubedtn

import (
	"context"
	"fmt"
	"net"
	"os"

	v1 "github.com/y-young/kube-dtn/api/v1"
	"github.com/y-young/kube-dtn/daemon/grpcwire"
	"github.com/y-young/kube-dtn/daemon/vxlan"

	"github.com/davecgh/go-spew/spew"
	koko "github.com/redhat-nfvpe/koko/api"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	"github.com/google/gopacket/pcap"
	"github.com/y-young/kube-dtn/common"
	pb "github.com/y-young/kube-dtn/proto/v1"
)

// TODO: Call SetInterNodeLinkType
var interNodeLinkType = common.INTER_NODE_LINK_VXLAN

func (m *KubeDTN) getPod(ctx context.Context, name, ns string) (*v1.Topology, error) {
	log.Infof("Reading pod %s from K8s", name)
	return m.tClient.Topology(ns).Get(ctx, name, metav1.GetOptions{})
}

func (m *KubeDTN) updateStatus(ctx context.Context, topology *v1.Topology, ns string) error {
	log.Infof("Update pod status %s from K8s", topology.Name)
	_, err := m.tClient.Topology(ns).Update(ctx, topology, metav1.UpdateOptions{})
	return err
}

func (m *KubeDTN) Get(ctx context.Context, pod *pb.PodQuery) (*pb.Pod, error) {
	log.Infof("Retrieving %s's metadata from K8s...", pod.Name)

	topology, err := m.getPod(ctx, pod.Name, pod.KubeNs)
	if err != nil {
		log.Errorf("Failed to read pod %s from K8s", pod.Name)
		return nil, err
	}

	remoteLinks := topology.Spec.Links
	if remoteLinks == nil {
		log.Errorf("Could not find 'Link' array in pod's spec")
		return nil, fmt.Errorf("could not find 'Link' array in pod's spec")
	}

	links := make([]*pb.Link, len(remoteLinks))
	for i := range links {
		remoteLink := remoteLinks[i]
		newLink := &pb.Link{
			PeerPod:    remoteLink.PeerPod,
			PeerIntf:   remoteLink.PeerIntf,
			LocalIntf:  remoteLink.LocalIntf,
			LocalIp:    remoteLink.LocalIP,
			PeerIp:     remoteLink.PeerIP,
			Uid:        int64(remoteLink.UID),
			Properties: remoteLink.Properties.ToProto(),
		}
		links[i] = newLink
	}

	srcIP := topology.Status.SrcIP
	netNs := topology.Status.NetNs
	nodeIP := os.Getenv("HOST_IP")

	return &pb.Pod{
		Name:   pod.Name,
		SrcIp:  srcIP,
		NetNs:  netNs,
		KubeNs: pod.KubeNs,
		Links:  links,
		NodeIp: nodeIP,
	}, nil
}

func (m *KubeDTN) SetAlive(ctx context.Context, pod *pb.Pod) (*pb.BoolResponse, error) {
	log.Infof("Setting %s's SrcIp=%s and NetNs=%s", pod.Name, pod.SrcIp, pod.NetNs)

	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		topology, err := m.getPod(ctx, pod.Name, pod.KubeNs)
		if err != nil {
			log.Errorf("Failed to read pod %s from K8s", pod.Name)
			return err
		}

		topology.Status.SrcIP = pod.SrcIp
		topology.Status.NetNs = pod.NetNs

		return m.updateStatus(ctx, topology, pod.KubeNs)
	})

	if retryErr != nil {
		log.WithFields(log.Fields{
			"err":      retryErr,
			"function": "SetAlive",
		}).Errorf("Failed to update pod %s alive status", pod.Name)
		return &pb.BoolResponse{Response: false}, retryErr
	}

	return &pb.BoolResponse{Response: true}, nil
}

func (m *KubeDTN) Skip(ctx context.Context, skip *pb.SkipQuery) (*pb.BoolResponse, error) {
	log.Infof("Skipping of pod %s by pod %s", skip.Peer, skip.Pod)

	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		topology, err := m.getPod(ctx, skip.Pod, skip.KubeNs)
		if err != nil {
			log.Errorf("Failed to read pod %s from K8s", skip.Pod)
			return err
		}

		skipped := topology.Status.Skipped
		newSkipped := append(skipped, skip.Peer)
		topology.Status.Skipped = newSkipped

		return m.updateStatus(ctx, topology, skip.KubeNs)
	})
	if retryErr != nil {
		log.WithFields(log.Fields{
			"err":      retryErr,
			"function": "Skip",
		}).Errorf("Failed to update skip pod %s status", skip.Pod)
		return &pb.BoolResponse{Response: false}, retryErr
	}

	return &pb.BoolResponse{Response: true}, nil
}

func (m *KubeDTN) SkipReverse(ctx context.Context, skip *pb.SkipQuery) (*pb.BoolResponse, error) {
	log.Infof("Reverse-skipping of pod %s by pod %s", skip.Peer, skip.Pod)

	var podName string
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// setting the value for peer pod
		peerPod, err := m.getPod(ctx, skip.Peer, skip.KubeNs)
		if err != nil {
			log.Errorf("Failed to read pod %s from K8s", skip.Pod)
			return err
		}
		podName = peerPod.GetName()

		// extracting peer pod's skipped list and adding this pod's name to it
		peerSkipped := peerPod.Status.Skipped
		newPeerSkipped := append(peerSkipped, skip.Pod)

		log.Infof("Updating peer skipped list")
		// updating peer pod's skipped list locally
		peerPod.Status.Skipped = newPeerSkipped

		// sending peer pod's updates to k8s
		return m.updateStatus(ctx, peerPod, skip.KubeNs)
	})
	if retryErr != nil {
		log.WithFields(log.Fields{
			"err":      retryErr,
			"function": "SkipReverse",
		}).Errorf("Failed to update peer pod %s skipreverse status", podName)
		return &pb.BoolResponse{Response: false}, retryErr
	}

	retryErr = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// setting the value for this pod
		thisPod, err := m.getPod(ctx, skip.Pod, skip.KubeNs)
		if err != nil {
			log.Errorf("Failed to read pod %s from K8s", skip.Pod)
			return err
		}

		// extracting this pod's skipped list and removing peer pod's name from it
		thisSkipped := thisPod.Status.Skipped
		newThisSkipped := make([]string, 0)

		log.WithFields(log.Fields{
			"thisSkipped": thisSkipped,
		}).Info("THIS SKIPPED:")

		for _, el := range thisSkipped {
			if el != skip.Peer {
				log.Infof("Appending new element %s", el)
				newThisSkipped = append(newThisSkipped, el)
			}
		}

		log.WithFields(log.Fields{
			"newThisSkipped": newThisSkipped,
		}).Info("NEW THIS SKIPPED:")

		// updating this pod's skipped list locally
		if len(newThisSkipped) != 0 {
			thisPod.Status.Skipped = newThisSkipped
			// sending this pod's updates to k8s
			return m.updateStatus(ctx, thisPod, skip.KubeNs)
		}
		return nil
	})
	if retryErr != nil {
		log.WithFields(log.Fields{
			"err":      retryErr,
			"function": "SkipReverse",
		}).Error("Failed to update this pod skipreverse status")
		return &pb.BoolResponse{Response: false}, retryErr
	}

	return &pb.BoolResponse{Response: true}, nil
}

func (m *KubeDTN) IsSkipped(ctx context.Context, skip *pb.SkipQuery) (*pb.BoolResponse, error) {
	log.Infof("Checking if %s is skipped by %s", skip.Peer, skip.Pod)

	topology, err := m.getPod(ctx, skip.Peer, skip.KubeNs)
	if err != nil {
		log.Errorf("Failed to read pod %s from K8s", skip.Pod)
		return nil, err
	}

	skipped := topology.Status.Skipped

	for _, peer := range skipped {
		if skip.Pod == peer {
			return &pb.BoolResponse{Response: true}, nil
		}
	}

	return &pb.BoolResponse{Response: false}, nil
}

func (m *KubeDTN) Update(ctx context.Context, pod *pb.RemotePod) (*pb.BoolResponse, error) {
	var veth *koko.VEth
	var err error
	if veth, err = vxlan.CreateOrUpdate(pod); err != nil {
		log.Errorf("Failed to Update Vxlan")
		return &pb.BoolResponse{Response: false}, nil
	}

	qdiscs, err := common.MakeQdiscs(pod.Properties)
	if err != nil {
		log.Errorf("Failed to construct qdiscs: %s", err)
		return &pb.BoolResponse{Response: false}, err
	}
	err = common.SetVethQdiscs(veth, qdiscs)
	if err != nil {
		log.Errorf("Failed to set qdisc on remote veth %s: %v", veth, err)
		return &pb.BoolResponse{Response: false}, err
	}
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

	locInf, err := net.InterfaceByName(wireDef.VethNameLocalHost)
	if err != nil {
		log.Errorf("Failed to retrieve interface ID for interface %v. error:%v", wireDef.VethNameLocalHost, err)
		return &pb.BoolResponse{Response: false}, err
	}

	//Using google gopacket for packet receive. An alternative could be using socket. Not sure it it provides any advantage over gopacket.
	wrHandle, err := pcap.OpenLive(wireDef.VethNameLocalHost, 65365, true, pcap.BlockForever)
	if err != nil {
		log.Fatalf("Could not open interface for send/recv packets for containers. error:%v", err)
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
		PeerPodIP:   wireDef.PeerIp,

		Originator:   grpcwire.HOST_CREATED_WIRE,
		OriginatorIP: "unknown", /*+++todo retrieve host ip and set it here. Needed only for debugging */

		StopC:     make(chan struct{}),
		Namespace: wireDef.KubeNs,
	}

	grpcwire.AddWire(&aWire, wrHandle)

	log.Infof("Starting the local packet receive thread for pod interface %s", wireDef.IntfNameInPod)
	// TODO: handle error here
	go grpcwire.RecvFrmLocalPodThread(&aWire)

	return &pb.BoolResponse{Response: true}, nil
}

// ------------------------------------------------------------------------------------------------------
func (m *KubeDTN) SendToOnce(ctx context.Context, pkt *pb.Packet) (*pb.BoolResponse, error) {

	wrHandle, err := grpcwire.GetHostIntfHndl(pkt.RemotIntfId)
	if err != nil {
		log.Printf("SendToOnce (wire id - %v): Could not find local handle. err:%v", pkt.RemotIntfId, err)
		return &pb.BoolResponse{Response: false}, err
	}

	// In case any per packet log need to be generated.
	// pktType := grpcwire.DecodePkt(pkt.Frame)
	// log.Printf("Daemon(SendToOnce): Received [pkt: %s, bytes: %d, for local interface id: %d]. Sending it to local container", pktType, len(pkt.Frame), pkt.RemotIntfId)
	// log.Printf("Daemon(SendToOnce): Received [bytes: %d, for local interface id: %d]. Sending it to local container", len(pkt.Frame), pkt.RemotIntfId)

	err = wrHandle.WritePacketData(pkt.Frame)
	if err != nil {
		log.Printf("SendToOnce (wire id - %v): Could not write packet(%d bytes) to local interface. err:%v", pkt.RemotIntfId, len(pkt.Frame), err)
		return &pb.BoolResponse{Response: false}, err
	}

	return &pb.BoolResponse{Response: true}, nil
}

// ---------------------------------------------------------------------------------------------------------------
func (m *KubeDTN) AddGRPCWireRemote(ctx context.Context, wireDef *pb.WireDef) (*pb.WireCreateResponse, error) {

	stopC := make(chan struct{})
	wire, err := grpcwire.CreateGRPCWireRemoteTriggered(wireDef, stopC)

	if err == nil {
		// TODO: handle error here
		go grpcwire.RecvFrmLocalPodThread(wire)

		return &pb.WireCreateResponse{Response: true, PeerIntfId: wire.LocalNodeIfaceID}, nil
	}
	log.Errorf("AddWireRemote err: %v", err)
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

func (m *KubeDTN) addLink(ctx context.Context, localPod *pb.Pod, link *pb.Link) error {
	nodeIP := os.Getenv("HOST_IP")
	// Finding the source IP and interface for VXLAN VTEP
	srcIP, srcIntf, err := common.GetVxlanSource(nodeIP)
	if err != nil {
		return err
	}
	log.Infof("VxLan route is via %s@%s", srcIP, srcIntf)

	// Build koko's veth struct for local intf
	myVeth, err := common.MakeVeth(localPod.NetNs, link.LocalIntf, link.LocalIp)
	if err != nil {
		return err
	}

	// First option is macvlan interface
	if link.PeerPod == "localhost" {
		log.Infof("Peer link is MacVlan")
		macVlan := koko.MacVLan{
			ParentIF: link.PeerIntf,
			Mode:     common.MacvlanMode,
		}
		if err = koko.MakeMacVLan(*myVeth, macVlan); err != nil {
			log.Infof("Failed to add macvlan interface")
			return err
		}
		log.Infof("macvlan interfacee %s@%s has been added", link.LocalIntf, link.PeerIntf)
		return nil
	}

	// Initialising peer pod's metadata
	log.Infof("Retrieving peer pod %s information from KubeDTN daemon", link.PeerPod)
	peerPod, err := m.Get(ctx, &pb.PodQuery{
		Name:   link.PeerPod,
		KubeNs: localPod.KubeNs,
	})
	if err != nil {
		log.Infof("Failed to retrieve peer pod %s:%s topology", localPod.KubeNs, link.PeerPod)
		return err
	}

	isAlive := peerPod.SrcIp != "" && peerPod.NetNs != ""
	log.Infof("Is peer pod %s alive?: %t", peerPod.Name, isAlive)

	if isAlive { // This means we're coming up AFTER our peer so things are pretty easy
		log.Infof("Peer pod %s is alive", peerPod.Name)
		if peerPod.SrcIp == localPod.SrcIp { // This means we're on the same host
			log.Infof("%s and %s are on the same host", localPod.Name, peerPod.Name)
			// Creating koko's Veth struct for peer intf
			peerVeth, err := common.MakeVeth(peerPod.NetNs, link.PeerIntf, link.PeerIp)
			if err != nil {
				log.Infof("Failed to build koko Veth struct")
				return err
			}

			// Checking if interfaces already exist
			iExist, _ := koko.IsExistLinkInNS(myVeth.NsName, myVeth.LinkName)
			pExist, _ := koko.IsExistLinkInNS(peerVeth.NsName, peerVeth.LinkName)

			log.Infof("Does the link already exist? Local:%t, Peer:%t", iExist, pExist)
			if iExist && pExist { // If both link exist, we don't need to do anything
				log.Info("Both interfaces already exist in namespace")
			} else if !iExist && pExist { // If only peer link exists, we need to destroy it first
				log.Info("Only peer link exists, removing it first")
				if err := peerVeth.RemoveVethLink(); err != nil {
					log.Infof("Failed to remove a stale interface %s of my peer %s", peerVeth.LinkName, link.PeerPod)
					return err
				}
				log.Infof("Adding the new veth link to both pods")
				if err = m.makeVeth(myVeth, peerVeth, link); err != nil {
					log.Infof("Error creating VEth pair after peer link remove: %s", err)
					return err
				}
			} else if iExist && !pExist { // If only local link exists, we need to destroy it first
				log.Infof("Only local link exists, removing it first")
				if err := myVeth.RemoveVethLink(); err != nil {
					log.Infof("Failed to remove a local stale VEth interface %s for pod %s", myVeth.LinkName, localPod.Name)
					return err
				}
				log.Infof("Adding the new veth link to both pods")
				if err = m.makeVeth(myVeth, peerVeth, link); err != nil {
					log.Infof("Error creating VEth pair after local link remove: %s", err)
					return err
				}
			} else { // if neither link exists, we have two options
				log.Infof("Neither link exists. Checking if we've been skipped")
				isSkipped, err := m.IsSkipped(ctx, &pb.SkipQuery{
					Pod:    localPod.Name,
					Peer:   peerPod.Name,
					KubeNs: localPod.KubeNs,
				})
				if err != nil {
					log.Infof("Failed to read skipped status from our peer")
					return err
				}
				log.Infof("Have we been skipped by our peer %s? %t", peerPod.Name, isSkipped.Response)

				// Comparing names to determine higher priority
				higherPrio := localPod.Name > peerPod.Name
				log.Infof("DO we have a higher priority? %t", higherPrio)

				if isSkipped.Response || higherPrio { // If peer POD skipped us (booted before us) or we have a higher priority
					log.Infof("Peer POD has skipped us or we have a higher priority")
					if err = m.makeVeth(myVeth, peerVeth, link); err != nil {
						log.Infof("Error when creating a new VEth pair with koko: %s", err)
						log.Infof("MY VETH STRUCT: %+v", spew.Sdump(myVeth))
						log.Infof("PEER STRUCT: %+v", spew.Sdump(peerVeth))
						return err
					}
				} else { // peerPod has higherPrio and hasn't skipped us
					// In this case we do nothing, since the pod with a higher IP is supposed to connect veth pair
					log.Infof("Doing nothing, expecting peer pod %s to connect veth pair", peerPod.Name)
					return nil
				}
			}
		} else { // This means we're on different hosts
			log.Infof("%s@%s and %s@%s are on different hosts", localPod.Name, localPod.SrcIp, peerPod.Name, peerPod.SrcIp)
			if interNodeLinkType == common.INTER_NODE_LINK_GRPC {
				err = common.CreateGRPCChan(link, localPod, peerPod, m, ctx)
				if err != nil {
					log.Infof("!! Failed to create grpc wire. err: %v", err)
					return err
				}
				return nil
			}
			// Creating koko's Vxlan struct
			vxlan := common.MakeVxlan(srcIntf, peerPod.SrcIp, link.Uid)
			// Checking if interface already exists
			iExist, _ := koko.IsExistLinkInNS(myVeth.NsName, myVeth.LinkName)
			if iExist { // If VXLAN intf exists, we need to remove it first
				log.Infof("VXLAN intf already exists, removing it first")
				if err := myVeth.RemoveVethLink(); err != nil {
					log.Infof("Failed to remove a local stale VXLAN interface %s for pod %s", myVeth.LinkName, localPod.Name)
					return err
				}
			}
			if err = m.makeVxLan(myVeth, vxlan, link); err != nil {
				log.Infof("Error when creating a Vxlan interface with koko: %s", err)
				return err
			}

			// Now we need to make an API call to update the remote VTEP to point to us
			payload := &pb.RemotePod{
				NetNs:      peerPod.NetNs,
				IntfName:   link.PeerIntf,
				IntfIp:     link.PeerIp,
				PeerVtep:   localPod.SrcIp,
				Vni:        link.Uid + common.VxlanBase,
				KubeNs:     localPod.KubeNs,
				Properties: link.Properties,
			}

			url := fmt.Sprintf("%s:%s", peerPod.SrcIp, common.DefaultPort)
			log.Infof("Trying to do a remote update on %s", url)

			remote, err := grpc.Dial(url, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				log.Infof("Failed to dial remote gRPC url %s", url)
				return err
			}
			remoteClient := pb.NewRemoteClient(remote)
			ok, err := remoteClient.Update(ctx, payload)
			if err != nil || !ok.Response {
				log.Infof("Failed to do a remote update")
				return err
			}
			log.Infof("Successfully updated remote KubeDTN daemon")
		}
	} else { // This means that our peer pod hasn't come up yet
		// Since there's no way of telling if our peer is going to be on this host or another,
		// the only option is to do nothing, assuming that the peer POD will do all the plumbing when it comes up
		log.Infof("Peer pod %s isn't alive yet, continuing", peerPod.Name)
		// Here we need to set the skipped flag so that our peer can configure VEth interface when it comes up later
		ok, err := m.Skip(ctx, &pb.SkipQuery{
			Pod:    localPod.Name,
			Peer:   peerPod.Name,
			KubeNs: localPod.KubeNs,
		})
		if err != nil || !ok.Response {
			log.Infof("Failed to set a skipped flag on peer %s", peerPod.Name)
			return err
		}
	}
	return nil
}

func (m *KubeDTN) makeVeth(self *koko.VEth, peer *koko.VEth, link *pb.Link) error {
	err := koko.MakeVeth(*self, *peer)
	if err != nil {
		return err
	}
	qdiscs, err := common.MakeQdiscs(link.Properties)
	if err != nil {
		log.Errorf("Failed to construct qdiscs: %s", err)
		return err
	}
	err = common.SetVethQdiscs(self, qdiscs)
	if err != nil {
		log.Errorf("Failed to set qdisc on self veth %s: %v", self, err)
		return err
	}
	err = common.SetVethQdiscs(peer, qdiscs)
	if err != nil {
		log.Errorf("Failed to set qdisc on peer veth %s: %v", self, err)
		return err
	}
	return nil
}

func (m *KubeDTN) makeVxLan(self *koko.VEth, vxlan *koko.VxLan, link *pb.Link) error {
	err := koko.MakeVxLan(*self, *vxlan)
	if err != nil {
		return err
	}
	qdiscs, err := common.MakeQdiscs(link.Properties)
	if err != nil {
		log.Errorf("Failed to construct qdiscs: %s", err)
		return err
	}
	err = common.SetVethQdiscs(self, qdiscs)
	if err != nil {
		log.Errorf("Failed to set qdisc on self veth %s: %v", self, err)
		return err
	}
	return nil
}

func (m *KubeDTN) delLink(ctx context.Context, localPod *pb.Pod, link *pb.Link) error {
	// Creating koko's Veth struct for local intf
	myVeth, err := common.MakeVeth(localPod.NetNs, link.LocalIntf, link.LocalIp)
	if err != nil {
		log.Infof("Failed to construct koko Veth struct")
		return err
	}

	log.Infof("Removing link %s", link.LocalIntf)
	// API call to koko to remove local Veth link
	if err = myVeth.RemoveVethLink(); err != nil {
		// instead of failing, just log the error and move on
		log.Infof("Error removing Veth link: %s", err)
	}

	// Setting reversed skipped flag so that this pod will try to connect veth pair on restart
	log.Infof("Setting skip-reverse flag on peer %s", link.PeerPod)
	ok, err := m.SkipReverse(ctx, &pb.SkipQuery{
		Pod:    localPod.Name,
		Peer:   link.PeerPod,
		KubeNs: localPod.KubeNs,
	})
	if err != nil || !ok.Response {
		log.Infof("Failed to set skip reversed flag on our peer %s", link.PeerPod)
		return err
	}

	return nil
}

func (m *KubeDTN) AddLink(ctx context.Context, query *pb.AddLinkQuery) (*pb.BoolResponse, error) {
	err := m.addLink(ctx, query.LocalPod, query.Link)
	if err != nil {
		return &pb.BoolResponse{Response: false}, err
	}
	return &pb.BoolResponse{Response: true}, nil
}

func (m *KubeDTN) DelLink(ctx context.Context, query *pb.DelLinkQuery) (*pb.BoolResponse, error) {
	err := m.delLink(ctx, query.LocalPod, query.Link)
	if err != nil {
		return &pb.BoolResponse{Response: false}, err
	}
	return &pb.BoolResponse{Response: true}, nil
}
