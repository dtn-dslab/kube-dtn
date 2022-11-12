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
	"fmt"
	"reflect"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"networkop.co.uk/meshnet/api/v1beta1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	mpb "github.com/networkop/meshnet-cni/daemon/proto/meshnet/v1beta1"
	"github.com/sirupsen/logrus"
)

const (
	defaultPort = "51111"
)

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

	var topology v1beta1.Topology
	if err := r.Get(ctx, req.NamespacedName, &topology); err != nil {
		log.Info("Unable to fetch Topology", "error", err)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Spec remains the same, nothing to do
	if reflect.DeepEqual(topology.Status.Links, topology.Spec.Links) {
		return ctrl.Result{}, nil
	}

	if topology.ObjectMeta.Generation == 1 {
		logrus.Infof("Topology %s created", topology.Name)
		// We'll copy initial links to status later
	} else {
		add, del := r.CalcDiff(topology.Status.Links, topology.Spec.Links)
		fmt.Printf("Topology %s changed,\n\tadd: %v,\n\tdel: %v\n", topology.Name, add, del)

		if err := r.DelLinks(ctx, &topology, del); err != nil {
			logrus.Errorf("Topology %s: Failed to update links: %s", topology.Name, err)
			return ctrl.Result{}, err
		}

		if err := r.AddLinks(ctx, &topology, add); err != nil {
			logrus.Errorf("Topology %s: Failed to update links: %s", topology.Name, err)
			return ctrl.Result{}, err
		}
	}

	// Since meshnet CNI will also update the status, retry on conflict
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var topology v1beta1.Topology
		if err := r.Get(ctx, req.NamespacedName, &topology); err != nil {
			log.Info("Unable to fetch Topology", "error", err)
			return client.IgnoreNotFound(err)
		}

		topology.Status.Links = topology.Spec.Links
		return r.Status().Update(ctx, &topology)
	})

	if err != nil {
		logrus.Errorf("Topology %s: Failed to update status: %s", topology.Name, err)
		return ctrl.Result{}, err
	}

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
		if err != nil || !result.GetResponse() {
			logrus.Errorf("Failed to add link %v: %s", link, err)
			return err
		}
	}
	return err
}

func (r *TopologyReconciler) DelLinks(ctx context.Context, topology *v1beta1.Topology, links []v1beta1.Link) error {
	daemonAddr := topology.Status.SrcIp + ":" + defaultPort
	conn, err := grpc.Dial(daemonAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		logrus.Infof("Failed to connect to daemon on %s", daemonAddr)
		return err
	}
	defer conn.Close()
	meshnetClient := mpb.NewLocalClient(conn)

	for _, link := range links {
		result, err := meshnetClient.DelLink(ctx, &mpb.DelLinkQuery{
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
		if err != nil || !result.GetResponse() {
			logrus.Errorf("Failed to delete link %v: %s", link, err)
			return err
		}
	}
	return err
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
