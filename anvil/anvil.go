package anvil

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/rpc"

	"github.com/ethereum/go-ethereum/log"
)

type Config struct {
	ChainId uint64
	Port    uint64
	Genesis []byte
}

type Anvil struct {
	rpcClient *rpc.Client

	log log.Logger

	cfg *Config
	cmd *exec.Cmd

	resourceCtx    context.Context
	resourceCancel context.CancelFunc

	stopped   atomic.Bool
	stoppedCh chan struct{}
}

const (
	host = "127.0.0.1"
)

func New(log log.Logger, cfg *Config) *Anvil {
	resCtx, resCancel := context.WithCancel(context.Background())
	return &Anvil{
		log:            log,
		cfg:            cfg,
		resourceCtx:    resCtx,
		resourceCancel: resCancel,
		stoppedCh:      make(chan struct{}, 1),
	}
}

func (a *Anvil) Start(ctx context.Context) error {
	if a.cmd != nil {
		return errors.New("anvil already started")
	}

	tempFile, err := os.CreateTemp("", "genesis-*.json")
	if err != nil {
		return fmt.Errorf("error creating temporary genesis file: %w", err)
	}

	_, err = tempFile.Write(a.cfg.Genesis)
	if err != nil {
		return fmt.Errorf("error writing to genesis file: %w", err)
	}

	// Prep args
	args := []string{
		"--silent",
		"--host", host,
		"--chain-id", fmt.Sprintf("%d", a.cfg.ChainId),
		"--port", fmt.Sprintf("%d", a.cfg.Port),
		"--init", tempFile.Name(),
	}

	anvilLog := a.log.New("role", "anvil", "chain.id", a.cfg.ChainId)
	anvilLog.Info("starting anvil", "args", args)
	a.cmd = exec.CommandContext(a.resourceCtx, "anvil", args...)
	go func() {
		<-ctx.Done()
		a.resourceCancel()
	}()

	// Handle stdout/stderr
	//  - TODO: Figure out best way to dump into logger. Some hex isn't showing appropriately
	stdout, err := a.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to get handle on stdout: %w", err)
	}
	stderr, err := a.cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to get handle on stderr: %w", err)
	}
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			anvilLog.Info(scanner.Text())
		}
	}()
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			anvilLog.Error(scanner.Text())
		}
	}()

	// Start anvil
	if err := a.cmd.Start(); err != nil {
		return fmt.Errorf("failed to start anvil: %w", err)
	}

	rpcClient, err := rpc.Dial(a.Endpoint())
	if err != nil {
		return fmt.Errorf("failed to create RPC client: %w", err)
	}
	a.rpcClient = rpcClient

	go func() {
		defer os.Remove(tempFile.Name())
		defer a.rpcClient.Close()

		if err := a.cmd.Wait(); err != nil {
			anvilLog.Error("anvil terminated with an error", "error", err)
		} else {
			anvilLog.Info("anvil terminated")
		}

		a.stoppedCh <- struct{}{}
	}()

	return nil
}

func (a *Anvil) Stop() error {
	if a.stopped.Load() {
		return errors.New("already stopped")
	}
	if !a.stopped.CompareAndSwap(false, true) {
		return nil // someone else stopped
	}

	a.resourceCancel()
	<-a.stoppedCh
	return nil
}

func (a *Anvil) Stopped() bool {
	return a.stopped.Load()
}

func (a *Anvil) Endpoint() string {
	return fmt.Sprintf("http://%s:%d", host, a.cfg.Port)
}

func (a *Anvil) ChainId() uint64 {
	return a.cfg.ChainId
}

func (a *Anvil) EnableLogging() {
	var result string
	if err := a.rpcClient.Call(&result, "anvil_setLoggingEnabled", true); err != nil {
		a.log.Error("failed to enable logging", "error", err)
	}
}

func (a *Anvil) WaitUntilReady(ctx context.Context) error {
	timeoutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled")
		case <-timeoutCtx.Done():
			return fmt.Errorf("timed out waiting for response from client")
		case <-ticker.C:
			var result string
			callErr := a.rpcClient.Call(&result, "web3_clientVersion")

			if callErr != nil {
				continue
			}

			if strings.HasPrefix(result, "anvil") {
				return nil
			}

			return fmt.Errorf("unexpected client version: %s", result)
		}
	}
}
