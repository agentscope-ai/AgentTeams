package controller

import (
	"context"
	"errors"
	v1beta1 "github.com/agentscope-ai/AgentTeams/agentteams-controller/api/v1beta1"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/modelhealth"
	"github.com/agentscope-ai/AgentTeams/agentteams-controller/internal/service"
	"testing"
	"time"
)

type healthDeployer struct {
	service.ManagerDeployer
	entered, release, done chan struct{}
}

func (h *healthDeployer) CheckManagerModel(context.Context, string, string, string, string, modelhealth.Notify) error {
	close(h.entered)
	<-h.release
	close(h.done)
	return errors.New("safe diagnostic")
}

type healthProvisioner struct{ service.ManagerProvisioner }

func (healthProvisioner) ModelHealthAdmin() string { return "@admin:test" }
func (healthProvisioner) SendModelHealthNotice(context.Context, string, string, string, string) error {
	return nil
}
func TestManagerHealthDoesNotBlockLifecycle(t *testing.T) {
	d := &healthDeployer{entered: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
	r := &ManagerReconciler{Deployer: d, Provisioner: healthProvisioner{}}
	s := &managerScope{manager: &v1beta1.Manager{}, provResult: &service.ManagerProvisionResult{}}
	started := time.Now()
	r.reconcileManagerModelHealth(context.Background(), s)
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("preflight blocked lifecycle")
	}
	select {
	case <-d.entered:
	case <-time.After(time.Second):
		t.Fatal("not started")
	}
	// A second reconcile during a slow gateway probe must not launch another.
	r.reconcileManagerModelHealth(context.Background(), s)
	close(d.release)
	<-d.done
}
