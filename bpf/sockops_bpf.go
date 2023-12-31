// Code generated by bpf2go; DO NOT EDIT.

package bpf

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"

	"github.com/cilium/ebpf"
)

// loadSockops returns the embedded CollectionSpec for sockops.
func loadSockops() (*ebpf.CollectionSpec, error) {
	reader := bytes.NewReader(_SockopsBytes)
	spec, err := ebpf.LoadCollectionSpecFromReader(reader)
	if err != nil {
		return nil, fmt.Errorf("can't load sockops: %w", err)
	}

	return spec, err
}

// loadSockopsObjects loads sockops and converts it into a struct.
//
// The following types are suitable as obj argument:
//
//	*sockopsObjects
//	*sockopsPrograms
//	*sockopsMaps
//
// See ebpf.CollectionSpec.LoadAndAssign documentation for details.
func loadSockopsObjects(obj interface{}, opts *ebpf.CollectionOptions) error {
	spec, err := loadSockops()
	if err != nil {
		return err
	}

	return spec.LoadAndAssign(obj, opts)
}

// sockopsSpecs contains maps and programs before they are loaded into the kernel.
//
// It can be passed ebpf.CollectionSpec.Assign.
type sockopsSpecs struct {
	sockopsProgramSpecs
	sockopsMapSpecs
}

// sockopsSpecs contains programs before they are loaded into the kernel.
//
// It can be passed ebpf.CollectionSpec.Assign.
type sockopsProgramSpecs struct {
	BpfSockmap *ebpf.ProgramSpec `ebpf:"bpf_sockmap"`
}

// sockopsMapSpecs contains maps before they are loaded into the kernel.
//
// It can be passed ebpf.CollectionSpec.Assign.
type sockopsMapSpecs struct {
	DebugMap       *ebpf.MapSpec `ebpf:"debug_map"`
	MapActiveEstab *ebpf.MapSpec `ebpf:"map_active_estab"`
	MapProxy       *ebpf.MapSpec `ebpf:"map_proxy"`
	MapRedir       *ebpf.MapSpec `ebpf:"map_redir"`
}

// sockopsObjects contains all objects after they have been loaded into the kernel.
//
// It can be passed to loadSockopsObjects or ebpf.CollectionSpec.LoadAndAssign.
type sockopsObjects struct {
	sockopsPrograms
	sockopsMaps
}

func (o *sockopsObjects) Close() error {
	return _SockopsClose(
		&o.sockopsPrograms,
		&o.sockopsMaps,
	)
}

// sockopsMaps contains all maps after they have been loaded into the kernel.
//
// It can be passed to loadSockopsObjects or ebpf.CollectionSpec.LoadAndAssign.
type sockopsMaps struct {
	DebugMap       *ebpf.Map `ebpf:"debug_map"`
	MapActiveEstab *ebpf.Map `ebpf:"map_active_estab"`
	MapProxy       *ebpf.Map `ebpf:"map_proxy"`
	MapRedir       *ebpf.Map `ebpf:"map_redir"`
}

func (m *sockopsMaps) Close() error {
	return _SockopsClose(
		m.DebugMap,
		m.MapActiveEstab,
		m.MapProxy,
		m.MapRedir,
	)
}

// sockopsPrograms contains all programs after they have been loaded into the kernel.
//
// It can be passed to loadSockopsObjects or ebpf.CollectionSpec.LoadAndAssign.
type sockopsPrograms struct {
	BpfSockmap *ebpf.Program `ebpf:"bpf_sockmap"`
}

func (p *sockopsPrograms) Close() error {
	return _SockopsClose(
		p.BpfSockmap,
	)
}

func _SockopsClose(closers ...io.Closer) error {
	for _, closer := range closers {
		if err := closer.Close(); err != nil {
			return err
		}
	}
	return nil
}

// Do not access this directly.
//
//go:embed sockops_bpf.o
var _SockopsBytes []byte
