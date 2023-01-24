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

package v1

import (
	pb "github.com/y-young/kube-dtn/proto/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// TopologySpec defines the desired state of Topology
type TopologySpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// +optional
	Links []Link `json:"links"`
}

// TopologyStatus defines the observed state of Topology
type TopologyStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// List of pods that are skipped by local pod
	// +optional
	Skipped []string `json:"skipped"`

	// Source IP of the pod
	// +optional
	SrcIP string `json:"src_ip"`

	// Network namespace of the pod
	// +optional
	NetNs string `json:"net_ns"`

	// Link statuses
	// +optional
	Links []Link `json:"links"`
}

// A complete definition of a p2p link
type Link struct {
	// Local interface name
	LocalIntf string `json:"local_intf"`

	// Local IP address
	// +optional
	LocalIP string `json:"local_ip"`

	// Peer interface name
	PeerIntf string `json:"peer_intf"`

	// Peer IP address
	// +optional
	PeerIP string `json:"peer_ip"`

	// Name of the peer pod
	PeerPod string `json:"peer_pod"`

	// Unique identifier of a p2p link
	UID int `json:"uid"`

	// Link properties, latency, bandwidth, etc
	// +optional
	Properties LinkProperties `json:"properties,omitempty"`
}

type LinkProperties struct {
	// Latency in duration string format, e.g. "300ms", "1.5s".
	// Valid time units are "ns", "us" (or "µs"), "ms", "s", "m", "h".
	// +optional
	Latency string `json:"latency,omitempty"`

	// Latency correlation in float percentage
	// +optional
	LatencyCorr string `json:"latency_corr,omitempty"`

	// Jitter in duration string format, e.g. "300ms", "1.5s".
	// Valid time units are "ns", "us" (or "µs"), "ms", "s", "m", "h".
	// +optional
	Jitter string `json:"jitter,omitempty"`

	// Loss rate in float percentage
	// +optional
	Loss string `json:"loss,omitempty"`

	// Loss correlation in float percentage
	// +optional
	LossCorr string `json:"loss_corr,omitempty"`

	// Bandwidth rate limit, e.g. 1000(bit/s), 100kbit, 100Mbps, 1Gibps.
	// For more information, refer to https://man7.org/linux/man-pages/man8/tc.8.html.
	// +optional
	Rate string `json:"rate,omitempty"`

	// Gap every N packets
	// +optional
	Gap uint32 `json:"gap,omitempty"`

	// Duplicate rate in float percentage
	// +optional
	Duplicate string `json:"duplicate,omitempty"`

	// Duplicate correlation in float percentage
	// +optional
	DuplicateCorr string `json:"duplicate_corr,omitempty"`

	// Reorder probability in float percentage
	// +optional
	ReorderProb string `json:"reorder_prob,omitempty"`

	// Reorder correlation in float percentage
	// +optional
	ReorderCorr string `json:"reorder_corr,omitempty"`

	// Corrupt probability in float percentage
	// +optional
	CorruptProb string `json:"corrupt_prob,omitempty"`

	// Corrupt correlation in float percentage
	// +optional
	CorruptCorr string `json:"corrupt_corr,omitempty"`
}

func (p *LinkProperties) ToProto() *pb.LinkProperties {
	return &pb.LinkProperties{
		Latency:       p.Latency,
		LatencyCorr:   p.LatencyCorr,
		Jitter:        p.Jitter,
		Loss:          p.Loss,
		LossCorr:      p.LossCorr,
		Rate:          p.Rate,
		Gap:           p.Gap,
		Duplicate:     p.Duplicate,
		DuplicateCorr: p.DuplicateCorr,
		ReorderProb:   p.ReorderProb,
		ReorderCorr:   p.ReorderCorr,
		CorruptProb:   p.CorruptProb,
		CorruptCorr:   p.CorruptCorr,
	}
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// Topology is the Schema for the topologies API
type Topology struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TopologySpec   `json:"spec,omitempty"`
	Status TopologyStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// TopologyList contains a list of Topology
type TopologyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Topology `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Topology{}, &TopologyList{})
}
