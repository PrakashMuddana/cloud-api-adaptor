// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"context"
	"fmt"
	"net/netip"
	"net/url"
	"testing"

	cri "github.com/containerd/containerd/pkg/cri/annotations"
	pb "github.com/kata-containers/kata-containers/src/runtime/protocols/hypervisor"
	"github.com/stretchr/testify/assert"

	"github.com/confidential-containers/cloud-api-adaptor/src/cloud-api-adaptor/pkg/adaptor/proxy"
	"github.com/confidential-containers/cloud-api-adaptor/src/cloud-api-adaptor/pkg/forwarder"
	"github.com/confidential-containers/cloud-api-adaptor/src/cloud-api-adaptor/pkg/podnetwork"
	"github.com/confidential-containers/cloud-api-adaptor/src/cloud-api-adaptor/pkg/podnetwork/tunneler"
	"github.com/confidential-containers/cloud-api-adaptor/src/cloud-api-adaptor/pkg/securecomms/kubemgr"
	"github.com/confidential-containers/cloud-api-adaptor/src/cloud-api-adaptor/pkg/securecomms/ppssh"
	"github.com/confidential-containers/cloud-api-adaptor/src/cloud-api-adaptor/pkg/util/tlsutil"
	"github.com/confidential-containers/cloud-api-adaptor/src/cloud-api-adaptor/test/securecomms/test"
	provider "github.com/confidential-containers/cloud-api-adaptor/src/cloud-providers"
	"github.com/confidential-containers/cloud-api-adaptor/src/cloud-providers/util/cloudinit"
)

type mockProvider struct{}

func (p *mockProvider) CreateInstance(ctx context.Context, podName, sandboxID string, cloudConfig cloudinit.CloudConfigGenerator, spec provider.InstanceTypeSpec) (*provider.Instance, error) {
	return &provider.Instance{
		Name: "abc",
		ID:   fmt.Sprintf("%s-%.8s", podName, sandboxID),
		IPs: []netip.Addr{
			netip.MustParseAddr("127.0.0.1"),
		},
	}, nil
}

func (p *mockProvider) DeleteInstance(ctx context.Context, instanceID string) error {
	return nil
}

func (p *mockProvider) Teardown() error {
	return nil
}

func (p *mockProvider) ConfigVerifier() error {
	return nil
}

func (p *mockProvider) SelectInstanceType(ctx context.Context, vCPU int64, memory int64) (instanceType string, err error) {
	return "", nil
}

type mockProxy struct {
	readyCh    chan struct{}
	stopCh     chan struct{}
	socketPath string
}

func (p *mockProxy) Start(ctx context.Context, serverURL *url.URL) error {
	close(p.readyCh)
	<-p.stopCh
	return nil
}

func (p *mockProxy) Ready() chan struct{} {
	return p.readyCh
}

func (p *mockProxy) Shutdown() error {
	close(p.stopCh)
	return nil
}

func (p *mockProxy) ClientCA() (certPEM []byte) {
	return nil
}

func (p *mockProxy) CAService() tlsutil.CAService {
	return nil
}

type mockProxyFactory struct {
	podsDir string
}

func (f *mockProxyFactory) New(serverName, socketPath string) proxy.AgentProxy {
	return &mockProxy{
		socketPath: socketPath,
		readyCh:    make(chan struct{}),
		stopCh:     make(chan struct{}),
	}
}

type mockWorkerNode struct{}

func (n mockWorkerNode) Inspect(nsPath string) (*tunneler.Config, error) {
	return &tunneler.Config{
		TunnelType:          podnetwork.DefaultTunnelType,
		Index:               0,
		ExternalNetViaPodVM: false,
	}, nil
}

func (n *mockWorkerNode) Setup(nsPath string, podNodeIPs []netip.Addr, config *tunneler.Config) error {
	return nil
}

func (n *mockWorkerNode) Teardown(nsPath string, config *tunneler.Config) error {
	return nil
}

func TestCloudService(t *testing.T) {

	ctx := context.Background()
	dir := t.TempDir()

	proxyFactory := &mockProxyFactory{
		podsDir: dir,
	}

	cfg := &ServerConfig{
		SecureComms:   false,
		PodsDir:       dir,
		ForwarderPort: forwarder.DefaultListenPort,
	}

	// false, "", "", "", "", "", dir, forwarder.DefaultListenPort, ""
	s := NewService(&mockProvider{}, proxyFactory, &mockWorkerNode{}, cfg, "")

	assert.NotNil(t, s)

	sandboxID := "123"
	sandboxNS := "default"
	sandboxName := "mypod"

	req := &pb.CreateVMRequest{
		Id: sandboxID,
		Annotations: map[string]string{
			cri.SandboxNamespace: sandboxNS,
			cri.SandboxName:      sandboxName,
		},
	}

	res1, err := s.CreateVM(ctx, req)

	assert.NoError(t, err)
	assert.NotNil(t, res1)
	assert.Contains(t, res1.AgentSocketPath, dir)

	res2, err := s.StartVM(ctx, &pb.StartVMRequest{Id: sandboxID})

	assert.NoError(t, err)
	assert.NotNil(t, res2)

	res3, err := s.StopVM(ctx, &pb.StopVMRequest{Id: sandboxID})

	assert.NoError(t, err)
	assert.NotNil(t, res3)
}

func TestCloudServiceWithSecureComms(t *testing.T) {
	sshport := "6001"
	kubemgr.InitKubeMgrMock()
	test.CreatePKCS8Secret(t)
	test.KBSServer("9009")
	s9009 := test.HttpServer(forwarder.DefaultListenPort)
	if s9009 == nil {
		t.Error("Failed - could not create server")
	}

	// create a podvm
	gkc := test.NewGetKeyClient("9019")
	ctx2, cancel := context.WithCancel(context.Background())

	ppSecrets := ppssh.NewPpSecrets(ppssh.GetSecret(gkc.GetKey))
	ppSecrets.AddKey(ppssh.WN_PUBLIC_KEY)
	ppSecrets.AddKey(ppssh.PP_PRIVATE_KEY)

	sshServer := ppssh.NewSshServer([]string{"BOTH_PHASES:KBS:9019"}, []string{"KUBERNETES_PHASE:KATAAGENT:127.0.0.1:7111"}, ppSecrets, sshport)
	_ = sshServer.Start(ctx2)
	defer func() {
		cancel()
	}()
	ctx := context.Background()
	dir := t.TempDir()

	proxyFactory := &mockProxyFactory{
		podsDir: dir,
	}

	cfg := &ServerConfig{
		SecureComms:           true,
		SecureCommsTrustee:    true,
		PodsDir:               dir,
		ForwarderPort:         forwarder.DefaultListenPort,
		SecureCommsKbsAddress: "127.0.0.1:9009",
	}

	s := NewService(&mockProvider{}, proxyFactory, &mockWorkerNode{}, cfg, sshport)

	assert.NotNil(t, s)

	sandboxID := "123"
	sandboxNS := "default"
	sandboxName := "mypod"

	req := &pb.CreateVMRequest{
		Id: sandboxID,
		Annotations: map[string]string{
			cri.SandboxNamespace: sandboxNS,
			cri.SandboxName:      sandboxName,
		},
	}

	res1, err := s.CreateVM(ctx, req)

	assert.NoError(t, err)
	assert.NotNil(t, res1)
	assert.Contains(t, res1.AgentSocketPath, dir)

	res2, err := s.StartVM(ctx, &pb.StartVMRequest{Id: sandboxID})

	assert.NoError(t, err)
	assert.NotNil(t, res2)

	res3, err := s.StopVM(ctx, &pb.StopVMRequest{Id: sandboxID})

	assert.NoError(t, err)
	assert.NotNil(t, res3)
}
