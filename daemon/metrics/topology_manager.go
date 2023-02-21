package metrics

import (
	"context"
	"os"

	"github.com/y-young/kube-dtn/api/clientset/v1beta1"
	v1 "github.com/y-young/kube-dtn/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type TopologyManager struct {
	topologies map[string]*v1.Topology
}

func NewTopologyManager() *TopologyManager {
	return &TopologyManager{
		topologies: make(map[string]*v1.Topology),
	}
}

func (m *TopologyManager) Init(client v1beta1.TopologyInterface) error {
	ctx := context.Background()
	topologies, err := client.List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	nodeIP := os.Getenv("HOST_IP")
	for _, topology := range topologies.Items {
		// https://medium.com/swlh/use-pointer-of-for-range-loop-variable-in-go-3d3481f7ffc9
		// Iteration variables are re-used each iteration,
		// through shadowing we create a new local variable for each iteration.
		topology := topology
		if topology.Status.SrcIP == nodeIP {
			m.topologies[topology.Name] = &topology
		}
	}
	return nil
}

func (m *TopologyManager) Add(topology *v1.Topology) {
	m.topologies[topology.Name] = topology
}

func (m *TopologyManager) Delete(name string) {
	delete(m.topologies, name)
}

func (m *TopologyManager) Get(name string) *v1.Topology {
	return m.topologies[name]
}

func (m *TopologyManager) List() []*v1.Topology {
	topologies := make([]*v1.Topology, 0, len(m.topologies))
	for _, topology := range m.topologies {
		topologies = append(topologies, topology)
	}
	return topologies
}

func (m *TopologyManager) Update(topology *v1.Topology) {
	m.topologies[topology.Name] = topology
}
