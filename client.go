package main

import (
	"context"
	"fmt"
	"net/url"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/simulator"
)

func newVCenterClient(ctx context.Context) (*govmomi.Client, error) {
	if enableMocking {
		return newMockClient(ctx)
	}
	return newRealClient(ctx)
}

func newMockClient(ctx context.Context) (*govmomi.Client, error) {
	// VPX (and not ESX) because the ESX model has no compute clusters.
	model := simulator.VPX()
	if err := model.Create(); err != nil {
		return nil, err
	}

	server := model.Service.NewServer()
	parsed, err := url.Parse(server.URL.String())
	if err != nil {
		return nil, err
	}
	parsed.User = url.UserPassword("user", "pass")
	return govmomi.NewClient(ctx, parsed, false)
}

func newRealClient(ctx context.Context) (*govmomi.Client, error) {
	if vcUrl == "" || vcUsername == "" || vcPassword == "" {
		return nil, fmt.Errorf("GOVC_URL, GOVC_USERNAME, and GOVC_PASSWORD must be set")
	}
	parsed, err := url.Parse(vcUrl)
	if err != nil {
		return nil, err
	}
	parsed.User = url.UserPassword(vcUsername, vcPassword)
	return govmomi.NewClient(ctx, parsed, false)
}
