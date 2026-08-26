package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/dns"
	"github.com/grafana/dskit/flagext"
	"github.com/grafana/dskit/kv"
	"github.com/grafana/dskit/kv/codec"
	"github.com/grafana/dskit/kv/memberlist"
	"github.com/grafana/dskit/ring"
	"github.com/grafana/dskit/services"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	// ringKey is the key under which we store the foreman-exporter's ring in the KVStore.
	ringKey = "foreman-exporter"

	// ringNumTokens is how many tokens each foreman-exporter should have in the
	// ring. foreman-exporter uses tokens to establish a ring leader, therefore
	// only one token is needed.
	ringNumTokens = 1

	// ringAutoForgetUnhealthyPeriods is how many consecutive timeout periods an
	// unhealthy instance in the ring will be automatically removed after.
	ringAutoForgetUnhealthyPeriods = 3

	// leaderToken is the special token that makes the owner the ring leader.
	leaderToken = 0
)

// RingLifecyclerConfig holds the ring lifecycler settings exposed on the CLI
// (see main.go), instead of hardcoded values.
type RingLifecyclerConfig struct {
	HeartbeatPeriod                 time.Duration
	HeartbeatTimeout                time.Duration
	KeepInstanceInTheRingOnShutdown bool
}

// ringOp is used as an instance state filter when obtaining instances from the
// ring. Instances in the LEAVING state are included to help minimise the number
// of leader changes during rollout and scaling operations. These instances will
// be forgotten after ringAutoForgetUnhealthyPeriods (see
// `KeepInstanceInTheRingOnShutdown`).
var ringOp = ring.NewOp([]ring.InstanceState{ring.ACTIVE, ring.LEAVING}, nil)

type ExporterRing struct {
	enabled       bool
	client        *ring.Ring
	lifecycler    *ring.BasicLifecycler
	memberlistsvc *memberlist.KVInitService
	kvStore       *memberlist.KV
	jsonClient    *memberlist.Client
}

func newRing(instanceID, instanceAddr, joinMembers, instanceInterfaceNames string, instancePort int, mlKVConfig memberlist.KVConfig, lifecyclerCfg RingLifecyclerConfig, logger log.Logger) (ExporterRing, error) {
	var config ExporterRing
	ctx := context.Background()

	joinMembersSlice := make([]string, 0)
	if joinMembers != "" {
		joinMembersSlice = strings.Split(joinMembers, ",")
	}

	instanceInterfaceNamesSlice := make([]string, 0)
	if instanceInterfaceNames != "" {
		instanceInterfaceNamesSlice = strings.Split(instanceInterfaceNames, ",")
	}

	if instanceID == "" {
		var err error
		instanceID, err = os.Hostname()
		if err != nil {
			level.Error(logger).Log("msg", "failed to get hostname", "err", err) // #nosec G104
			os.Exit(1)
		}
	}

	reg := prometheus.DefaultRegisterer
	reg = prometheus.WrapRegistererWithPrefix("foreman_exporter_", reg)

	// start memberlist service.
	memberlistsvc := SimpleMemberlistKV(mlKVConfig, instanceID, instanceAddr, instancePort, joinMembersSlice, logger, reg)
	if err := services.StartAndAwaitRunning(ctx, memberlistsvc); err != nil {
		return config, err
	}

	store, err := memberlistsvc.GetMemberlistKV()
	if err != nil {
		return config, err
	}

	ringClient, err := memberlist.NewClient(store, ring.GetCodec())
	if err != nil {
		return config, err
	}

	jsonClient, err := memberlist.NewClient(store, cacheCodec)
	if err != nil {
		return config, err
	}

	lfc, err := SimpleRingLifecycler(ringClient, instanceID, instanceAddr, instancePort, instanceInterfaceNamesSlice, lifecyclerCfg, logger, reg)
	if err != nil {
		return config, err
	}

	// start lifecycler service
	if err := services.StartAndAwaitRunning(ctx, lfc); err != nil {
		return config, err
	}

	ringsvc, err := SimpleRing(ringClient, logger, reg)
	if err != nil {
		return config, err
	}
	// start the ring service
	if err := services.StartAndAwaitRunning(ctx, ringsvc); err != nil {
		return config, err
	}

	return ExporterRing{
		enabled:       true,
		client:        ringsvc,
		lifecycler:    lfc,
		memberlistsvc: memberlistsvc,
		kvStore:       store,
		jsonClient:    jsonClient,
	}, nil
}

// SimpleRing returns an instance of `ring.Ring` as a service. Starting and Stopping the service is upto the caller.
func SimpleRing(store kv.Client, logger log.Logger, reg prometheus.Registerer) (*ring.Ring, error) {
	var config ring.Config
	flagext.DefaultValues(&config)
	config.ReplicationFactor = 1
	config.SubringCacheDisabled = true

	return ring.NewWithStoreClientAndStrategy(
		config,
		ringKey,           // ring name
		"collectors/ring", // prefix key where peers are stored
		store,
		ring.NewDefaultReplicationStrategy(),
		reg,
		log.With(logger, "component", "ring"),
	)
}

// SimpleMemberlistKV returns a memberlist KV as a service. Starting and Stopping the service is upto the caller.
// Caller can create an instance `kv.Client` from returned service by explicity calling `.GetMemberlistKV()`
// which can be used as dependency to create a ring or ring lifecycler.
func SimpleMemberlistKV(config memberlist.KVConfig, instanceID, instanceAddr string, instancePort int, joinMembers []string, logger log.Logger, reg prometheus.Registerer) *memberlist.KVInitService {
	// config already carries every memberlist setting from the CLI flags (with
	// dskit defaults). Only override what is specific to this exporter.

	// Codecs tell the memberlist library how to (de)serialize messages between
	// peers. `ring.GetCodec()` uses protobuf.
	config.Codecs = []codec.Codec{ring.GetCodec(), cacheCodec}

	// Set the listen addr/port on the EXISTING transport config. Replacing the
	// whole TCPTransportConfig would zero the transport timeouts (dial, write,
	// max-concurrent-writes, acquire-writer-timeout); with those at zero,
	// MaxConcurrentWrites clamps to a single writer and AcquireWriterTimeout=0
	// drops any packet that can't grab that slot instantly — on a high-latency
	// (cross-DC) link that silently drops probe ACKs and gossip and makes the
	// cluster flap. Those values now come from the CLI flags.
	config.TCPTransport.BindPort = instancePort
	config.TCPTransport.BindAddrs = []string{instanceAddr}

	// The ring instance id is authoritative for the memberlist node name.
	config.NodeName = instanceID

	// joinMembers comes from --ring.join-members (the dnssrv+ SRV record).
	if len(joinMembers) > 0 {
		config.JoinMembers = joinMembers
	}

	// resolver defines how each peer's IP address should be resolved.
	// maxIdleConnections is only used by the miekgdns resolver; the golang
	// resolver ignores it.
	resolver := dns.NewProvider(dns.GolangResolverType, 0, log.With(logger, "component", "dns"), reg)

	return memberlist.NewKVInitService(
		&config,
		log.With(logger, "component", "memberlist"),
		resolver,
		reg,
	)
}

// SimpleRingLifecycler returns an instance lifecycler for the given `kv.Client`.
// Usually lifecycler will be part of the server side that act as a single peer.
func SimpleRingLifecycler(store kv.Client, instanceID, instanceAddr string, instancePort int, instanceInterfaceNames []string, cfg RingLifecyclerConfig, logger log.Logger, reg prometheus.Registerer) (*ring.BasicLifecycler, error) {
	var config ring.BasicLifecyclerConfig
	instanceAddr, err := ring.GetInstanceAddr(instanceAddr, instanceInterfaceNames, logger, false)
	if err != nil {
		return nil, err
	}

	config.ID = instanceID
	config.Addr = fmt.Sprintf("%s:%d", instanceAddr, instancePort)
	config.HeartbeatPeriod = cfg.HeartbeatPeriod
	config.HeartbeatTimeout = cfg.HeartbeatTimeout
	config.TokensObservePeriod = 0
	config.NumTokens = ringNumTokens
	config.KeepInstanceInTheRingOnShutdown = cfg.KeepInstanceInTheRingOnShutdown

	var delegate ring.BasicLifecyclerDelegate

	delegate = ring.NewInstanceRegisterDelegate(ring.ACTIVE, ringNumTokens)
	delegate = ring.NewLeaveOnStoppingDelegate(delegate, logger)
	delegate = ring.NewAutoForgetDelegate(ringAutoForgetUnhealthyPeriods*cfg.HeartbeatPeriod, delegate, logger)

	return ring.NewBasicLifecycler(
		config,
		ringKey,
		"collectors/ring",
		store,
		delegate,
		log.With(logger, "component", "lifecycler"),
		reg,
	)
}

// isLeader checks whether this instance is the leader replica that exports metrics for all tenants.
func isLeader(expRing ExporterRing) (bool, error) {
	// Get the leader from the ring and check whether it's this replica.
	rl, err := ringLeader(expRing.client)
	if err != nil {
		return false, err
	}

	return rl.Addr == expRing.lifecycler.GetInstanceAddr(), nil
}

// ringLeader returns the ring member that owns the special token.
func ringLeader(r ring.ReadRing) (*ring.InstanceDesc, error) {
	rs, err := r.Get(leaderToken, ringOp, nil, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get a healthy instance for token %d: %w", leaderToken, err)
	}
	if len(rs.Instances) != 1 {
		return nil, fmt.Errorf("got %d instances for token %d (but expected 1)", len(rs.Instances), leaderToken)
	}

	return &rs.Instances[0], nil
}

// Node roles reported by foreman_exporter_node_role.
const (
	nodeRoleUnknown  = 0
	nodeRoleLeader   = 1
	nodeRoleFollower = 2
)

var (
	nodeRoleDesc = prometheus.NewDesc(
		"foreman_exporter_node_role",
		"Node role, 0 = unknown, 1 = leader, 2 = follower",
		nil, nil,
	)

	ringLeaderLookupErrorsMetric = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "foreman_exporter_ring_leader_lookup_errors_total",
		Help: "A counter of failures to resolve the ring leader.",
	})
)

// nodeRoleCollector exposes which role this instance holds in the ring. Only
// the leader collects from foreman, so knowing who it is at any moment is what
// makes the rest of the exporter's metrics readable across replicas.
//
// The role is resolved at scrape time rather than cached, so the metric can
// never report a role the instance no longer holds. A ring that cannot be
// resolved reports "unknown" rather than defaulting to follower: an unreachable
// ring must not look like a healthy non-leader, otherwise a cluster left with
// no leader at all would go unnoticed.
type nodeRoleCollector struct {
	leaderFn func() (bool, error)
	logger   *slog.Logger
}

func newNodeRoleCollector(expRing ExporterRing, logger *slog.Logger) nodeRoleCollector {
	return nodeRoleCollector{
		leaderFn: func() (bool, error) { return isLeader(expRing) },
		logger:   logger,
	}
}

func (c nodeRoleCollector) Describe(ch chan<- *prometheus.Desc) { ch <- nodeRoleDesc }

func (c nodeRoleCollector) Collect(ch chan<- prometheus.Metric) {
	role := float64(nodeRoleUnknown)

	switch isLeaderNow, err := c.leaderFn(); {
	case err != nil:
		ringLeaderLookupErrorsMetric.Inc()
		c.logger.Warn("Failed to determine ring leader", "err", err)
	case isLeaderNow:
		role = nodeRoleLeader
	default:
		role = nodeRoleFollower
	}

	ch <- prometheus.MustNewConstMetric(nodeRoleDesc, prometheus.GaugeValue, role)
}
