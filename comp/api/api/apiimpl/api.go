// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2023-present Datadog, Inc.

// Package apiimpl implements the internal Agent API which exposes endpoints such as config, flare or status
package apiimpl

import (
	"context"
	"net"

	"go.uber.org/fx"

	"github.com/DataDog/datadog-agent/comp/aggregator/diagnosesendermanager"
	api "github.com/DataDog/datadog-agent/comp/api/api/def"
	"github.com/DataDog/datadog-agent/comp/api/authtoken"
	"github.com/DataDog/datadog-agent/comp/collector/collector"
	"github.com/DataDog/datadog-agent/comp/core/autodiscovery"
	"github.com/DataDog/datadog-agent/comp/core/config"
	remoteagentregistry "github.com/DataDog/datadog-agent/comp/core/remoteagentregistry/def"
	"github.com/DataDog/datadog-agent/comp/core/secrets"
	tagger "github.com/DataDog/datadog-agent/comp/core/tagger/def"
	"github.com/DataDog/datadog-agent/comp/core/telemetry"
	workloadmeta "github.com/DataDog/datadog-agent/comp/core/workloadmeta/def"
	"github.com/DataDog/datadog-agent/comp/dogstatsd/pidmap"
	replay "github.com/DataDog/datadog-agent/comp/dogstatsd/replay/def"
	dogstatsdServer "github.com/DataDog/datadog-agent/comp/dogstatsd/server"
	"github.com/DataDog/datadog-agent/comp/remote-config/rcservice"
	"github.com/DataDog/datadog-agent/comp/remote-config/rcservicemrf"
	"github.com/DataDog/datadog-agent/pkg/util/fxutil"
	"github.com/DataDog/datadog-agent/pkg/util/option"
)

// Module defines the fx options for this component.
func Module() fxutil.Module {
	return fxutil.Component(
		fx.Provide(newAPIServer))
}

type apiServer struct {
	dogstatsdServer     dogstatsdServer.Component
	capture             replay.Component
	cfg                 config.Component
	pidMap              pidmap.Component
	secretResolver      secrets.Component
	rcService           option.Option[rcservice.Component]
	rcServiceMRF        option.Option[rcservicemrf.Component]
	authToken           authtoken.Component
	taggerComp          tagger.Component
	autoConfig          autodiscovery.Component
	wmeta               workloadmeta.Component
	collector           option.Option[collector.Component]
	senderManager       diagnosesendermanager.Component
	remoteAgentRegistry remoteagentregistry.Component
	cmdListener         net.Listener
	ipcListener         net.Listener
	telemetry           telemetry.Component
	endpointProviders   []api.EndpointProvider
}

type dependencies struct {
	fx.In

	Lc                    fx.Lifecycle
	DogstatsdServer       dogstatsdServer.Component
	Capture               replay.Component
	PidMap                pidmap.Component
	SecretResolver        secrets.Component
	RcService             option.Option[rcservice.Component]
	RcServiceMRF          option.Option[rcservicemrf.Component]
	AuthToken             authtoken.Component
	Tagger                tagger.Component
	Cfg                   config.Component
	AutoConfig            autodiscovery.Component
	WorkloadMeta          workloadmeta.Component
	Collector             option.Option[collector.Component]
	DiagnoseSenderManager diagnosesendermanager.Component
	Telemetry             telemetry.Component
	EndpointProviders     []api.EndpointProvider `group:"agent_endpoint"`
	RemoteAgentRegistry   remoteagentregistry.Component
}

var _ api.Component = (*apiServer)(nil)

func newAPIServer(deps dependencies) api.Component {

	server := apiServer{
		dogstatsdServer:     deps.DogstatsdServer,
		capture:             deps.Capture,
		pidMap:              deps.PidMap,
		secretResolver:      deps.SecretResolver,
		rcService:           deps.RcService,
		rcServiceMRF:        deps.RcServiceMRF,
		authToken:           deps.AuthToken,
		taggerComp:          deps.Tagger,
		cfg:                 deps.Cfg,
		autoConfig:          deps.AutoConfig,
		wmeta:               deps.WorkloadMeta,
		collector:           deps.Collector,
		senderManager:       deps.DiagnoseSenderManager,
		telemetry:           deps.Telemetry,
		endpointProviders:   fxutil.GetAndFilterGroup(deps.EndpointProviders),
		remoteAgentRegistry: deps.RemoteAgentRegistry,
	}

	deps.Lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error { return server.startServers() },
		OnStop: func(_ context.Context) error {
			server.stopServers()
			return nil
		},
	})

	return &server
}

// ServerAddress returns the server address.
func (server *apiServer) CMDServerAddress() *net.TCPAddr {
	return server.cmdListener.Addr().(*net.TCPAddr)
}

// ServerAddress returns the server address.
func (server *apiServer) IPCServerAddress() *net.TCPAddr {
	return server.ipcListener.Addr().(*net.TCPAddr)
}
