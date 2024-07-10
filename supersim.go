package supersim

import (
	_ "embed"
	"fmt"
	"strings"
	"sync"

	"context"

	"github.com/ethereum-optimism/supersim/anvil"
	opsim "github.com/ethereum-optimism/supersim/op-simulator"

	"github.com/ethereum/go-ethereum/log"
)

type Config struct {
	l1Chain  ChainConfig
	l2Chains []ChainConfig
}

type ChainConfig struct {
	anvilConfig anvil.Config
	opSimConfig opsim.Config
}

//go:embed genesis/genesis-l1.json
var genesisL1JSON []byte

//go:embed genesis/genesis-l2.json
var genesisL2JSON []byte

var DefaultConfig = Config{
	l1Chain: ChainConfig{
		anvilConfig: anvil.Config{ChainId: 1, Port: 0, Genesis: genesisL1JSON},
		opSimConfig: opsim.Config{Port: 0},
	},
	l2Chains: []ChainConfig{
		{
			anvilConfig: anvil.Config{ChainId: 10, Port: 0, Genesis: genesisL2JSON},
			opSimConfig: opsim.Config{Port: 0},
		},
		{
			anvilConfig: anvil.Config{ChainId: 30, Port: 0, Genesis: genesisL2JSON},
			opSimConfig: opsim.Config{Port: 0},
		},
	},
}

type Supersim struct {
	log log.Logger

	l1Anvil *anvil.Anvil
	l1OpSim *opsim.OpSimulator

	l2Anvils map[uint64]*anvil.Anvil
	l2OpSims map[uint64]*opsim.OpSimulator
}

func NewSupersim(log log.Logger, config *Config) *Supersim {
	l1Anvil := anvil.New(log, &config.l1Chain.anvilConfig)
	l1OpSim := opsim.New(log, &config.l1Chain.opSimConfig, l1Anvil)

	l2Anvils := make(map[uint64]*anvil.Anvil)
	l2OpSims := make(map[uint64]*opsim.OpSimulator)
	for i := range config.l2Chains {
		l2ChainConfig := config.l2Chains[i]
		l2Anvil := anvil.New(log, &l2ChainConfig.anvilConfig)
		l2Anvils[l2ChainConfig.anvilConfig.ChainId] = l2Anvil
		l2OpSims[l2ChainConfig.anvilConfig.ChainId] = opsim.New(log, &l2ChainConfig.opSimConfig, l2Anvil)
	}

	return &Supersim{log, l1Anvil, l1OpSim, l2Anvils, l2OpSims}
}

func (s *Supersim) Start(ctx context.Context) error {
	s.log.Info("starting supersim")

	if err := s.l1Anvil.Start(ctx); err != nil {
		return fmt.Errorf("l1 anvil failed to start: %w", err)
	}
	if err := s.l1OpSim.Start(ctx); err != nil {
		return fmt.Errorf("l1 op simulator failed to start: %w", err)
	}

	for _, l2Anvil := range s.l2Anvils {
		if err := l2Anvil.Start(ctx); err != nil {
			return fmt.Errorf("l2 anvil failed to start: %w", err)
		}
	}
	for _, l2OpSim := range s.l2OpSims {
		if err := l2OpSim.Start(ctx); err != nil {
			return fmt.Errorf("l2 op simulator failed to start: %w", err)
		}
	}

	if err := s.WaitUntilReady(); err != nil {
		return fmt.Errorf("supersim failed to get ready: %w", err)
	}

	s.EnableLogging()

	s.log.Info("supersim is ready")
	s.log.Info(s.ConfigAsString())

	return nil
}

func (s *Supersim) Stop(ctx context.Context) error {
	s.log.Info("stopping supersim")

	for _, l2OpSim := range s.l2OpSims {
		if err := l2OpSim.Stop(ctx); err != nil {
			return fmt.Errorf("l2 op simulator failed to stop: %w", err)
		}
		s.log.Info("stopped op simulator", "chain.id", l2OpSim.ChainId())
	}
	for _, l2Anvil := range s.l2Anvils {
		if err := l2Anvil.Stop(); err != nil {
			return fmt.Errorf("l2 anvil failed to stop: %w", err)
		}
	}

	if err := s.l1OpSim.Stop(ctx); err != nil {
		return fmt.Errorf("l1 op simulator failed to stop: %w", err)
	}
	if err := s.l1Anvil.Stop(); err != nil {
		return fmt.Errorf("l1 anvil failed to stop: %w", err)
	}
	s.log.Info("stopped op simulator", "chain.id", s.l1OpSim.ChainId())

	return nil
}

func (s *Supersim) Stopped() bool {
	for _, l2OpSim := range s.l2OpSims {
		if stopped := l2OpSim.Stopped(); !stopped {
			return stopped
		}
	}
	for _, l2Anvil := range s.l2Anvils {
		if stopped := l2Anvil.Stopped(); !stopped {
			return stopped
		}
	}

	if stopped := s.l1Anvil.Stopped(); !stopped {
		return stopped
	}
	if stopped := s.l1OpSim.Stopped(); !stopped {
		return stopped
	}

	return true
}

func (s *Supersim) WaitUntilReady() error {
	var once sync.Once
	var err error
	ctx, cancel := context.WithCancel(context.Background())

	handleErr := func(e error) {
		if e != nil {
			once.Do(func() {
				err = e
				cancel()
			})
		}
	}

	var wg sync.WaitGroup

	waitForAnvil := func(anvil *anvil.Anvil) {
		defer wg.Done()
		handleErr(anvil.WaitUntilReady(ctx))
	}

	s.IterateChains(func(chain *anvil.Anvil) {
		wg.Add(1)
		go waitForAnvil(chain)
	})

	wg.Wait()

	return err
}

func (s *Supersim) EnableLogging() {
	s.IterateChains(func(chain *anvil.Anvil) {
		chain.EnableLogging()
	})
}

func (s *Supersim) IterateChains(fn func(anvil *anvil.Anvil)) {
	fn(s.l1Anvil)

	for _, l2Anvil := range s.l2Anvils {
		fn(l2Anvil)
	}
}

func (s *Supersim) ConfigAsString() string {
	var b strings.Builder

	fmt.Fprintf(&b, "\nSupersim Config:\n")
	fmt.Fprintf(&b, "L1:\n")
	fmt.Fprintf(&b, "  Chain ID: %d    RPC: %s\n", s.l1OpSim.ChainId(), s.l1OpSim.Endpoint())

	fmt.Fprintf(&b, "L2:\n")
	for _, l2OpSim := range s.l2OpSims {
		fmt.Fprintf(&b, "  Chain ID: %d    RPC: %s\n", l2OpSim.ChainId(), l2OpSim.Endpoint())
	}

	return b.String()
}
