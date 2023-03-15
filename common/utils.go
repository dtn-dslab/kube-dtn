package common

import (
	"context"
	"fmt"

	koko "github.com/redhat-nfvpe/koko/api"
	log "github.com/sirupsen/logrus"
	"github.com/y-young/kube-dtn/daemon/vxlan"
	pb "github.com/y-young/kube-dtn/proto/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func Map[T, U any](ts []T, f func(T) U) []U {
	us := make([]U, len(ts))
	for i := range ts {
		us[i] = f(ts[i])
	}
	return us
}

// Generate VXLAN Vni from link UID
func GetVniFromUid(uid int64) int32 {
	return int32(VxlanBase + uid)
}

// Call remote daemon to set up link on their side
func UpdateRemote(ctx context.Context, localPod *pb.Pod, peerPod *pb.Pod, link *pb.Link) error {
	payload := &pb.RemotePod{
		NetNs:      peerPod.NetNs,
		IntfName:   link.PeerIntf,
		IntfIp:     link.PeerIp,
		PeerVtep:   localPod.SrcIp,
		Vni:        GetVniFromUid(link.Uid),
		KubeNs:     localPod.KubeNs,
		Properties: link.Properties,
		Name:       link.PeerPod,
	}

	url := fmt.Sprintf("passthrough:///%s:%s", peerPod.SrcIp, DefaultPort)
	log.Infof("Trying to do a remote update on %s", url)

	remote, err := grpc.Dial(url, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Infof("Failed to dial remote gRPC url %s", url)
		return err
	}
	remoteClient := pb.NewRemoteClient(remote)
	ok, err := remoteClient.Update(ctx, payload)
	if err != nil || !ok.Response {
		log.Infof("Failed to do a remote update: %s", err)
		return err
	}
	return nil
}

// Set up VXLAN link with qdiscs
func SetupVxLan(v *vxlan.VxlanSpec, properties *pb.LinkProperties) (err error) {
	var veth *koko.VEth
	if veth, err = vxlan.CreateOrUpdate(v); err != nil {
		log.Errorf("Failed to setup VXLAN: %v", err)
		return nil
	}

	qdiscs, err := MakeQdiscs(properties)
	if err != nil {
		log.Errorf("Failed to construct qdiscs: %v", err)
		return err
	}
	err = SetVethQdiscs(veth, qdiscs)
	if err != nil {
		log.Errorf("Failed to set qdisc on self veth %s: %v", veth, err)
		return err
	}
	return nil
}
