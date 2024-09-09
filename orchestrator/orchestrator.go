package orchestrator

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/ethereum-optimism/supersim/anvil"
	"github.com/ethereum-optimism/supersim/config"
	"github.com/ethereum-optimism/supersim/interop"
	opsimulator "github.com/ethereum-optimism/supersim/opsimulator"

	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/log"
)

type Orchestrator struct {
	log log.Logger

	l1Chain config.Chain

	l2Chains map[uint64]config.Chain
	l2OpSims map[uint64]*opsimulator.OpSimulator

	l2ToL2MsgIndexer *interop.L2ToL2MessageIndexer
	l2ToL2MsgRelayer *interop.L2ToL2MessageRelayer
}

func NewOrchestrator(log log.Logger, closeApp context.CancelCauseFunc, networkConfig *config.NetworkConfig, enableAutoRelay bool) (*Orchestrator, error) {
	// Spin up L1 anvil instance
	l1Anvil := anvil.New(log, closeApp, &networkConfig.L1Config)

	// Spin up L2 anvil instances fronted by opsim
	nextL2Port := networkConfig.L2StartingPort
	l2Anvils, l2OpSims := make(map[uint64]config.Chain), make(map[uint64]*opsimulator.OpSimulator)
	for i := range networkConfig.L2Configs {
		cfg := networkConfig.L2Configs[i]
		cfg.Port = 0 // explicitly set to zero as this anvil sits behind a proxy

		l2Anvil := anvil.New(log, closeApp, &cfg)
		l2Anvils[cfg.ChainID] = l2Anvil
	}
	l2ToL2MsgIndexer := interop.NewL2ToL2MessageIndexer(log)

	for i := range networkConfig.L2Configs {
		cfg := networkConfig.L2Configs[i]
		l2OpSims[cfg.ChainID] = opsimulator.New(log, closeApp, nextL2Port, l1Anvil, l2Anvils[cfg.ChainID], l2Anvils)

		// only increment expected port if it has been specified
		if nextL2Port > 0 {
			nextL2Port++
		}
	}

	var l2ToL2MessageRelayer *interop.L2ToL2MessageRelayer
	if enableAutoRelay {
		l2ToL2MessageRelayer = interop.NewL2ToL2MessageRelayer()
	}

	return &Orchestrator{log, l1Anvil, l2Anvils, l2OpSims, l2ToL2MsgIndexer, l2ToL2MessageRelayer}, nil
}

func (o *Orchestrator) Start(ctx context.Context) error {

	o.log.Info("starting orchestrator")
	if err := o.l1Chain.Start(ctx); err != nil {
		return fmt.Errorf("l1 chain %s failed to start: %w", o.l1Chain.Config().Name, err)
	}

	for _, chain := range o.l2Chains {
		if err := chain.Start(ctx); err != nil {
			return fmt.Errorf("l2 chain %s failed to start: %w", chain.Config().Name, err)
		}
	}
	for _, opSim := range o.l2OpSims {
		if err := opSim.Start(ctx); err != nil {
			return fmt.Errorf("op simulator instance %s failed to start: %w", opSim.Config().Name, err)
		}
	}

	if err := o.kickOffMining(ctx); err != nil {
		return fmt.Errorf("unable to start mining: %w", err)
	}

	// TODO: hack until opsim proxy supports websocket connections.
	// We need websocket connections to make subscriptions.
	// We should try to use make RPC through opsim not directly to the underlying chain
	l2ChainClientByChainId := make(map[uint64]*ethclient.Client)
	l2OpSimClientByChainId := make(map[uint64]*ethclient.Client)

	for chainID, opSim := range o.l2OpSims {
		l2ChainClientByChainId[chainID] = opSim.Chain.EthClient()
		l2OpSimClientByChainId[chainID] = opSim.EthClient()
	}

	if err := o.l2ToL2MsgIndexer.Start(ctx, l2ChainClientByChainId); err != nil {
		return fmt.Errorf("l2 to l2 message indexer failed to start: %w", err)
	}

	if o.l2ToL2MsgRelayer != nil {
		o.log.Info("starting L2ToL2CrossDomainMessenger autorelayer")
		if err := o.l2ToL2MsgRelayer.Start(o.l2ToL2MsgIndexer, l2OpSimClientByChainId); err != nil {
			return fmt.Errorf("l2 to l2 message relayer failed to start: %w", err)
		}
	}

	o.log.Debug("orchestrator is ready")
	return nil
}

func (o *Orchestrator) Stop(ctx context.Context) error {
	var errs []error

	o.log.Info("stopping orchestrator")

	if o.l2ToL2MsgRelayer != nil {
		o.log.Info("stopping L2ToL2CrossDomainMessenger autorelayer")
		o.l2ToL2MsgRelayer.Stop(ctx)
	}

	o.log.Info("stopping L2ToL2CrossDomainMessenger indexer")
	if err := o.l2ToL2MsgIndexer.Stop(ctx); err != nil {
		errs = append(errs, fmt.Errorf("l2 to l2 message indexer failed to stop: %w", err))
	}
	for _, opSim := range o.l2OpSims {
		o.log.Debug("stopping op simulator", "chain.id", opSim.Config().ChainID)
		if err := opSim.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("op simulator instance %s failed to stop: %w", opSim.Config().Name, err))
		}
	}
	for _, chain := range o.l2Chains {
		o.log.Debug("stopping l2 chain", "chain.id", chain.Config().ChainID)
		if err := chain.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("l2 chain %s failed to stop: %w", chain.Config().Name, err))
		}
	}

	if err := o.l1Chain.Stop(ctx); err != nil {
		o.log.Debug("stopping l1 chain", "chain.id", o.l1Chain.Config().ChainID)
		errs = append(errs, fmt.Errorf("l1 chain %s failed to stop: %w", o.l1Chain.Config().Name, err))
	}

	return errors.Join(errs...)
}

func (o *Orchestrator) kickOffMining(ctx context.Context) error {
	var once sync.Once
	var err error
	ctx, cancel := context.WithCancel(ctx)

	handleErr := func(e error) {
		if e == nil {
			return
		}

		once.Do(func() {
			err = e
			cancel()
		})
	}

	var wg sync.WaitGroup
	wg.Add(len(o.l2Chains) + 1)

	handleErr(o.l1Chain.SetIntervalMining(ctx, nil, 2))
	wg.Done()

	for _, chain := range o.l2Chains {
		go func() {
			defer wg.Done()
			handleErr(chain.SetIntervalMining(ctx, nil, 2))
		}()
	}

	wg.Wait()

	return err
}

func (o *Orchestrator) L1Chain() config.Chain {
	return o.l1Chain
}

func (o *Orchestrator) L2Chains() []config.Chain {
	var chains []config.Chain
	for _, chain := range o.l2OpSims {
		chains = append(chains, chain)
	}
	return chains
}

func (o *Orchestrator) Endpoint(chainId uint64) string {
	if o.l1Chain.Config().ChainID == chainId {
		return o.l1Chain.Endpoint()
	}
	return o.l2OpSims[chainId].Endpoint()
}

func (o *Orchestrator) ConfigAsString() string {
	var b strings.Builder
	l1Cfg := o.l1Chain.Config()
	fmt.Fprintf(&b, "L1: Name: %s  ChainID: %d  RPC: %s  LogPath: %s\n", l1Cfg.Name, l1Cfg.ChainID, o.l1Chain.Endpoint(), o.l1Chain.LogPath())

	if len(o.l2OpSims) > 0 {
		fmt.Fprintf(&b, "\nL2s: Predeploy Contracts Spec ( %s )\n", "https://specs.optimism.io/protocol/predeploys.html")

		opSims := make([]*opsimulator.OpSimulator, 0, len(o.l2OpSims))
		for _, chain := range o.l2OpSims {
			opSims = append(opSims, chain)
		}

		// sort by port number (retain ordering of chain flags)
		sort.Slice(opSims, func(i, j int) bool { return opSims[i].Config().Port < opSims[j].Config().Port })
		for _, opSim := range opSims {
			cfg := opSim.Config()
			fmt.Fprintf(&b, "\n")
			fmt.Fprintf(&b, "  * Name: %s  ChainID: %d  RPC: %s  LogPath: %s\n", cfg.Name, cfg.ChainID, opSim.Endpoint(), opSim.LogPath())
			fmt.Fprintf(&b, "    L1 Contracts:\n")
			fmt.Fprintf(&b, "     - OptimismPortal:         %s\n", cfg.L2Config.L1Addresses.OptimismPortalProxy)
			fmt.Fprintf(&b, "     - L1CrossDomainMessenger: %s\n", cfg.L2Config.L1Addresses.L1CrossDomainMessengerProxy)
			fmt.Fprintf(&b, "     - L1StandardBridge:       %s\n", cfg.L2Config.L1Addresses.L1StandardBridgeProxy)
		}
	}

	return b.String()
}
