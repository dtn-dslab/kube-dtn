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
	"reflect"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/go-redis/redis/v8"
	v1 "github.com/y-young/kube-dtn/api/v1"
	"github.com/y-young/kube-dtn/common"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/y-young/kube-dtn/proto/v1"
)

// TopologyReconciler reconciles a Topology object
type TopologyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Redis  *redis.Client
	Ctx    context.Context
}

//+kubebuilder:rbac:groups=y-young.github.io,resources=topologies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=y-young.github.io,resources=topologies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=y-young.github.io,resources=topologies/finalizers,verbs=update

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
	start := time.Now()
	log := log.FromContext(ctx)

	// Get new topology from k8s
	var topology v1.Topology
	if err := r.Get(ctx, req.NamespacedName, &topology); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Topology deleted")
			r.Redis.Del(r.Ctx, "cni_"+req.Name+"_spec")
		} else {
			log.Error(err, "Unable to fetch Topology")
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Get old topology from redis
	startTime := time.Now()
	oldTopoSpec := &common.RedisTopologySpec{}
	oldTopoSpecJSON, err := r.Redis.Get(r.Ctx, "cni_"+topology.Name+"_spec").Result()
	if err != redis.Nil {
		if err = json.Unmarshal([]byte(oldTopoSpecJSON), &oldTopoSpec); err != nil {
			log.Error(err, "Failed to unmarshal topology status from redis")
		}
	}
	elapsed := time.Since(startTime)
	log.Info("Get topology spec from redis", "elapsed", elapsed.Milliseconds())

	// Get status from redis
	startTime = time.Now()
	oldTopoStatus := &common.RedisTopologyStatus{}
	oldTopoStatusJSON, err := r.Redis.Get(r.Ctx, "cni_"+topology.Name+"_status").Result()
	if err != redis.Nil {
		if err = json.Unmarshal([]byte(oldTopoStatusJSON), &oldTopoStatus); err != nil {
			log.Error(err, "Failed to unmarshal topology status from redis")
		}
	}
	elapsed = time.Since(startTime)
	log.Info("Get topology status from redis", "elapsed", elapsed.Milliseconds())

	// Set new topology to redis
	redisTopoSpec := &common.RedisTopologySpec{
		Links: topology.Spec.Links,
	}
	specJSON, err := json.Marshal(redisTopoSpec)
	if err != nil {
		log.Error(err, "Failed to marshal topology status")
	}
	err = r.Redis.Set(r.Ctx, "cni_"+topology.Name+"_spec", specJSON, 0).Err()
	if err != nil {
		log.Error(err, "Failed to set topology spec to redis")
	}

	topology.Status.Links = oldTopoSpec.Links
	topology.Status.SrcIP = oldTopoStatus.SrcIP
	topology.Status.NetNs = oldTopoStatus.NetNs

	// Spec remains the same, nothing to do
	if reflect.DeepEqual(topology.Status.Links, topology.Spec.Links) {
		return ctrl.Result{}, nil
	}

	add, readd, del, propertiesChanged := r.CalcDiff(topology.Status.Links, topology.Spec.Links)
	// log.Info("Topology changed", "add", add, "del", del, "update", propertiesChanged)

	go func() {
		del_start := time.Now()

		if err := r.DelLinks(ctx, &topology, del); err != nil {
			log.Error(err, "Failed to delete links")
			// return ctrl.Result{}, err
		}

		del_elapsed := time.Since(del_start)
		fmt.Printf("%s: Topology %s del links: %d ms\n", time.Now(), topology.Name, del_elapsed.Milliseconds())
	}()

	go func() {
		add_start := time.Now()

		pb_links := common.Map(add, func(link v1.Link) *pb.Link { return link.ToProto() })

		if err := r.AddLinks(ctx, &topology, pb_links); err != nil {
			log.Error(err, "Failed to add links")
			// return ctrl.Result{}, err
		}

		add_elapsed := time.Since(add_start)
		fmt.Printf("%s: Topology %s add links: %d ms\n", time.Now(), topology.Name, add_elapsed.Milliseconds())
	}()

	go func() {
		readd_start := time.Now()

		pb_links := common.Map(readd, func(link v1.Link) *pb.Link { return link.ToProto() })
		for _, link := range pb_links {
			link.Detect = true
		}

		if err := r.AddLinks(ctx, &topology, pb_links); err != nil {
			log.Error(err, "Failed to readd links")
			// return ctrl.Result{}, err
		}

		readd_elapsed := time.Since(readd_start)
		fmt.Printf("%s: Topology %s readd links: %d ms\n", time.Now(), topology.Name, readd_elapsed.Milliseconds())
	}()

	go func() {
		err_start := time.Now()

		if err := r.UpdateLinks(ctx, &topology, propertiesChanged); err != nil {
			log.Error(err, "Failed to update links")
			// return ctrl.Result{}, err
		}

		err_elapsed := time.Since(err_start)
		fmt.Printf("%s: Topology %s update links: %d ms\n", time.Now(), topology.Name, err_elapsed.Milliseconds())
	}()

	elapsed = time.Since(start)
	fmt.Printf("%s: Topology %s changed total time: %d ms\n", time.Now(), topology.Name, elapsed.Milliseconds())

	return ctrl.Result{}, nil
}

func (r *TopologyReconciler) AddLinks(ctx context.Context, topology *v1.Topology, links []*pb.Link) error {
	if len(links) == 0 {
		return nil
	}

	log := log.FromContext(ctx)

	// conn_start := time.Now()

	conn, err := ConnectDaemon(ctx, topology.Status.SrcIP)
	if err != nil {
		return err
	}
	defer conn.Close()

	// conn_elapsed := time.Since(conn_start)
	// fmt.Printf("%s: Topology %s connect daemon: %d ms\n", time.Now(), topology.Name, conn_elapsed.Milliseconds())

	kubedtnClient := pb.NewLocalClient(conn)

	// request_start := time.Now()

	result, err := kubedtnClient.AddLinks(ctx, &pb.LinksBatchQuery{
		LocalPod: &pb.Pod{
			Name:   topology.Name,
			SrcIp:  topology.Status.SrcIP,
			NetNs:  topology.Status.NetNs,
			KubeNs: topology.Namespace,
			Safe:   false, // Make add links unsafe and fast
		},
		Links: links,
	})

	// request_elapsed := time.Since(request_start)
	// fmt.Printf("%s: Topology %s request: %d ms\n", time.Now(), topology.Name, request_elapsed.Milliseconds())

	if err != nil || !result.GetResponse() {
		log.Error(err, "Failed to add links")
		return err
	}
	// log.Info("Successfully added links", "links", common.Map(links, func(link v1.Link) int { return link.UID }))
	return nil
}

func (r *TopologyReconciler) DelLinks(ctx context.Context, topology *v1.Topology, links []v1.Link) error {
	if len(links) == 0 {
		return nil
	}

	log := log.FromContext(ctx)

	// conn_start := time.Now()

	conn, err := ConnectDaemon(ctx, topology.Status.SrcIP)
	if err != nil {
		return err
	}
	defer conn.Close()

	// conn_elapsed := time.Since(conn_start)
	// fmt.Printf("%s: Topology %s connect daemon: %d ms\n", time.Now(), topology.Name, conn_elapsed.Milliseconds())

	kubedtnClient := pb.NewLocalClient(conn)

	// request_start := time.Now()

	result, err := kubedtnClient.DelLinks(ctx, &pb.LinksBatchQuery{
		LocalPod: &pb.Pod{
			Name:   topology.Name,
			SrcIp:  topology.Status.SrcIP,
			NetNs:  topology.Status.NetNs,
			KubeNs: topology.Namespace,
		},
		Links: common.Map(links, func(link v1.Link) *pb.Link { return link.ToProto() }),
	})

	// request_elapsed := time.Since(request_start)
	// fmt.Printf("%s: Topology %s request: %d ms\n", time.Now(), topology.Name, request_elapsed.Milliseconds())

	if err != nil || !result.GetResponse() {
		log.Error(err, "Failed to delete links")
		return err
	}
	// log.Info("Successfully deleted links", "links", common.Map(links, func(link v1.Link) int { return link.UID }))
	return nil
}

func (r *TopologyReconciler) UpdateLinks(ctx context.Context, topology *v1.Topology, links []v1.Link) error {
	if len(links) == 0 {
		return nil
	}

	log := log.FromContext(ctx)

	// conn_start := time.Now()

	conn, err := ConnectDaemon(ctx, topology.Status.SrcIP)
	if err != nil {
		return err
	}
	defer conn.Close()

	// conn_elapsed := time.Since(conn_start)
	// fmt.Printf("%s: Topology %s connect daemon: %d ms\n", time.Now(), topology.Name, conn_elapsed.Milliseconds())

	kubedtnClient := pb.NewLocalClient(conn)

	// request_start := time.Now()

	result, err := kubedtnClient.UpdateLinks(ctx, &pb.LinksBatchQuery{
		LocalPod: &pb.Pod{
			Name:   topology.Name,
			SrcIp:  topology.Status.SrcIP,
			NetNs:  topology.Status.NetNs,
			KubeNs: topology.Namespace,
		},
		Links: common.Map(links, func(link v1.Link) *pb.Link { return link.ToProto() }),
	})

	// request_elapsed := time.Since(request_start)
	// fmt.Printf("%s: Topology %s request: %d ms\n", time.Now(), topology.Name, request_elapsed.Milliseconds())

	if err != nil || !result.GetResponse() {
		log.Error(err, "Failed to delete link")
		return err
	}
	// log.Info("Successfully updated links", "links", common.Map(links, func(link v1.Link) int { return link.UID }))
	return err
}

// Calculate difference between two old links and new links, returns a list of links to be added and a list of links to be deleted
func (r *TopologyReconciler) CalcDiff(old []v1.Link, new []v1.Link) (add []v1.Link, readd, del []v1.Link, propertiesChanged []v1.Link) {
	// Remove links that are in old but not in new. These links' name are definitely different and need to be deleted obviously
	for _, oldLink := range old {
		found := false
		for _, newLink := range new {
			if EqualWithLocalIntf(oldLink, newLink) {
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
			// If new link is not in old, we need to add it. While new link with different properties but same name will be deleted(not obviously) and added.
			if EqualWithoutProperties(oldLink, newLink) {
				found = true
				// If properties are different, we need to update it.
				if !reflect.DeepEqual(oldLink.Properties, newLink.Properties) {
					propertiesChanged = append(propertiesChanged, newLink)
				} else {
					readd = append(readd, newLink)
				}
				break
			}
		}
		if !found {
			add = append(add, newLink)
		}
	}
	return
}

func ConnectDaemon(ctx context.Context, ip string) (*grpc.ClientConn, error) {
	log := log.FromContext(ctx)
	daemonAddr := "passthrough:///" + ip + ":" + common.DefaultPort
	conn, err := grpc.Dial(daemonAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Error(err, "Failed to connect to daemon", "address", daemonAddr)
		return nil, err
	}
	return conn, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *TopologyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.Topology{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 3000,
		}).
		Complete(r)
}

func EqualWithLocalIntf(a, b v1.Link) bool {
	return a.LocalIntf == b.LocalIntf
}

// EqualWithoutProperties compares two links without comparing link properties
func EqualWithoutProperties(a, b v1.Link) bool {
	return a.LocalIntf == b.LocalIntf &&
		a.LocalIP == b.LocalIP &&
		a.LocalMAC == b.LocalMAC &&
		a.PeerIntf == b.PeerIntf &&
		a.PeerIP == b.PeerIP &&
		a.PeerMAC == b.PeerMAC &&
		a.PeerPod == b.PeerPod &&
		a.UID == b.UID
}
