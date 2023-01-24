package kubedtn

import (
	"fmt"
	"net"
	"path/filepath"

	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_ctxtags "github.com/grpc-ecosystem/go-grpc-middleware/tags"

	glogrus "github.com/grpc-ecosystem/go-grpc-middleware/logging/logrus"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"

	topologyclientv1 "github.com/y-young/kube-dtn/api/clientset/v1beta1"

	pb "github.com/y-young/kube-dtn/proto/v1"
)

type Config struct {
	Port     int
	GRPCOpts []grpc.ServerOption
}

type KubeDTN struct {
	pb.UnimplementedLocalServer
	pb.UnimplementedRemoteServer
	pb.UnimplementedWireProtocolServer
	config  Config
	kClient kubernetes.Interface
	tClient topologyclientv1.Interface
	rCfg    *rest.Config
	s       *grpc.Server
	lis     net.Listener
}

var logger *log.Entry = nil

func InitLogger() {
	logger = log.WithFields(log.Fields{"daemon": "kubedtnd"})
}

func restConfig() (*rest.Config, error) {
	logger.Infof("Trying in-cluster configuration")
	rCfg, err := rest.InClusterConfig()
	if err != nil {
		kubecfg := filepath.Join(".kube", "config")
		if home := homedir.HomeDir(); home != "" {
			kubecfg = filepath.Join(home, kubecfg)
		}
		logger.Infof("Falling back to kubeconfig: %q", kubecfg)
		rCfg, err = clientcmd.BuildConfigFromFlags("", kubecfg)
		if err != nil {
			return nil, err
		}
	}
	return rCfg, nil
}

func New(cfg Config) (*KubeDTN, error) {
	rCfg, err := restConfig()
	if err != nil {
		return nil, err
	}
	kClient, err := kubernetes.NewForConfig(rCfg)
	if err != nil {
		return nil, err
	}
	tClient, err := topologyclientv1.NewForConfig(rCfg)
	if err != nil {
		return nil, err
	}
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.Port))
	if err != nil {
		return nil, err
	}
	m := &KubeDTN{
		config:  cfg,
		rCfg:    rCfg,
		kClient: kClient,
		tClient: tClient,
		lis:     lis,
		s:       newServerWithLogging(cfg.GRPCOpts...),
	}
	pb.RegisterLocalServer(m.s, m)
	pb.RegisterRemoteServer(m.s, m)
	pb.RegisterWireProtocolServer(m.s, m)
	reflection.Register(m.s)
	return m, nil
}

func (m *KubeDTN) Serve() error {
	logger.Infof("GRPC server has started on port: %d", m.config.Port)
	return m.s.Serve(m.lis)
}

func (m *KubeDTN) Stop() {
	m.s.Stop()
}

func newServerWithLogging(opts ...grpc.ServerOption) *grpc.Server {
	lEntry := log.NewEntry(log.StandardLogger())
	lOpts := []glogrus.Option{}
	glogrus.ReplaceGrpcLogger(lEntry)
	opts = append(opts,
		grpc_middleware.WithUnaryServerChain(
			grpc_ctxtags.UnaryServerInterceptor(grpc_ctxtags.WithFieldExtractor(grpc_ctxtags.CodeGenRequestFieldExtractor)),
			glogrus.UnaryServerInterceptor(lEntry, lOpts...),
		),
		grpc_middleware.WithStreamServerChain(
			grpc_ctxtags.StreamServerInterceptor(grpc_ctxtags.WithFieldExtractor(grpc_ctxtags.CodeGenRequestFieldExtractor)),
			glogrus.StreamServerInterceptor(lEntry, lOpts...),
		))
	return grpc.NewServer(opts...)
}
